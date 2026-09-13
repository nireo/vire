package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
)

const maxRequestBytes = 1 << 20 // 1 MiB; only requests are buffered, never responses.

func newProxy(target *url.URL, transport http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			// Do not trust or forward client-supplied Forwarded/X-Forwarded headers.
		},
		Transport: transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return
			}
			status := http.StatusBadGateway
			message := "inference backend unavailable"
			var netErr net.Error
			if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
				status = http.StatusGatewayTimeout
				message = "inference backend timed out"
			}
			writeAPIError(w, status, message)
		},
		// ReverseProxy automatically flushes SSE responses as data arrives.
		// Do not retry generation: an upstream may already have accepted it.
	}
}

func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "request body exceeds 1 MiB")
		} else {
			writeAPIError(w, http.StatusBadRequest, "could not read request body")
		}
		return
	}

	// Inspect routing metadata only. Leave all other validation to the backend,
	// and retain the original bytes so new vLLM fields need no gateway changes.
	// JSON keys are case-sensitive. A struct would also accept "Model", which
	// could select a different backend than the exact "model" field requests.
	var fields map[string]json.RawMessage
	var model string
	if json.Unmarshal(body, &fields) != nil || json.Unmarshal(fields["model"], &model) != nil || model == "" {
		writeAPIError(w, http.StatusBadRequest, "request must be a JSON object with a nonempty model string")
		return
	}
	proxy, ok := s.proxies[model]
	if !ok {
		writeAPIError(w, http.StatusNotFound, "unknown model")
		return
	}

	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = nil // Do not make the generation request replayable.
	proxy.ServeHTTP(w, r)
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	errorType := "invalid_request_error"
	if status >= 500 {
		errorType = "server_error"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{"message": message, "type": errorType},
	})
}
