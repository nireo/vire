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
	"sync/atomic"

	"github.com/buraksezer/consistent"
	"github.com/cespare/xxhash/v2"
)

const maxRequestBytes = 1 << 20 // 1 MiB; only requests are buffered, never responses.

type routeBackend struct {
	proxy  *httputil.ReverseProxy
	origin string
}

func (backend *routeBackend) String() string { return backend.origin }

type routeHasher struct{}

func (routeHasher) Sum64(key []byte) uint64 { return xxhash.Sum64(key) }

// modelRoute is immutable after startup except for its atomic request counter.
// Each request stays on its selected backend for the entire response stream.
type modelRoute struct {
	backends []*routeBackend
	ring     *consistent.Consistent
	next     atomic.Uint64
}

func (route *modelRoute) buildRing() {
	if len(route.backends) < 2 {
		return
	}
	members := make([]consistent.Member, len(route.backends))
	for i, backend := range route.backends {
		members[i] = backend
	}
	// Keep these fixed across restarts and gateways to preserve affinity.
	route.ring = consistent.New(members, consistent.Config{
		PartitionCount:    4093,
		ReplicationFactor: 20,
		Load:              1.25,
		Hasher:            routeHasher{},
	})
}

func (route *modelRoute) selectBackend(conversationID string) *routeBackend {
	if len(route.backends) == 1 {
		return route.backends[0]
	}
	if conversationID != "" {
		return route.ring.LocateKey([]byte(conversationID)).(*routeBackend)
	}
	index := (route.next.Add(1) - 1) % uint64(len(route.backends))
	return route.backends[index]
}

func (route *modelRoute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	backend := route.selectBackend(r.Header.Get("X-Conversation-ID"))
	backend.proxy.ServeHTTP(w, r)
}

func newProxy(target *url.URL, transport http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Header.Del("X-Conversation-ID") // Gateway-only routing metadata.
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
