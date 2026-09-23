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
	"strings"
	"sync/atomic"

	"github.com/buraksezer/consistent"
	"github.com/cespare/xxhash/v2"
)

const maxRequestBytes = 1 << 20 // 1 MiB for model lookup.

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
	backends      []*routeBackend
	ring          *consistent.Consistent
	next          atomic.Uint64
	model         string
	observability *observability
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
	conversationID := r.Header.Get("X-Conversation-ID")
	if accountID, _ := r.Context().Value(accountContextKey{}).(string); accountID != "" && conversationID != "" {
		conversationID = accountID + "\x00" + conversationID
	}
	backend := route.selectBackend(conversationID)
	if state := observation(r); state != nil {
		state.model, state.backend = route.model, backend.origin
		state.routing = "round_robin"
		if len(route.backends) == 1 {
			state.routing = "single"
		} else if conversationID != "" {
			state.routing = "affinity"
		}
		route.observability.routed.WithLabelValues(state.model, state.backend, state.routing).Inc()
		inflight := route.observability.inflight.WithLabelValues(state.model, state.backend)
		inflight.Inc()
		defer inflight.Dec()
	}
	backend.proxy.ServeHTTP(w, r)
}

func newProxy(target *url.URL, transport http.RoundTripper, authenticated bool, backendAPIKey string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(target)
			r.Out.Header.Del("X-Conversation-ID") // Gateway-only routing metadata.
			if authenticated {
				r.Out.Header.Del("Authorization")
				r.Out.Header.Del("Accept-Encoding") // Metering observes plain JSON/SSE.
				if backendAPIKey != "" {
					r.Out.Header.Set("Authorization", "Bearer "+backendAPIKey)
				}
			}
			if state := observation(r.Out); state != nil {
				r.Out.Header.Set("X-Request-ID", state.id)
			}
			// Do not trust or forward client-supplied Forwarded/X-Forwarded headers.
		},
		Transport: transport,
		ErrorLog:  discardedProxyLog,
		ModifyResponse: func(response *http.Response) error {
			if state := observation(response.Request); state != nil && state.usage != nil {
				state.usage.status = response.StatusCode
				state.usage.stream = state.usage.stream || strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream")
				if response.StatusCode >= 200 && response.StatusCode < 300 {
					response.Body = &usageBody{ReadCloser: response.Body, capture: state.usage}
				}
			}
			return nil
		},
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
			if state := observation(r); state != nil {
				state.outcome = "upstream_unavailable"
				if status == http.StatusGatewayTimeout {
					state.outcome = "upstream_timeout"
				}
				if state.usage != nil {
					state.usage.status = status
				}
			}
			writeAPIError(w, status, message)
		},
		// ReverseProxy automatically flushes SSE responses as data arrives.
		// Do not retry generation: an upstream may already have accepted it.
	}
}

func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	if s.inflightLimit != nil {
		select {
		case s.inflightLimit <- struct{}{}:
			defer func() { <-s.inflightLimit }()
		default:
			w.Header().Set("Retry-After", "1")
			writeAPIError(w, http.StatusTooManyRequests, "gateway is at its concurrency limit")
			return
		}
	}
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
	if s.accountStore != nil {
		state := observation(r)
		start := UsageStart{RequestID: state.id, AccountID: identity.AccountID, KeyID: identity.KeyID, Model: model}
		ctx, cancel := context.WithTimeout(r.Context(), accountQueryTimeout)
		err := s.accountStore.BeginUsage(ctx, start)
		cancel()
		if err != nil {
			s.observability.meteringErrors.WithLabelValues("begin").Inc()
			s.observability.logger.Error("could not begin usage record", "request_id", state.id, "error", err)
			writeAPIError(w, http.StatusServiceUnavailable, "usage store unavailable")
			return
		}
		metered := &usageCapture{stream: string(fields["stream"]) == "true"}
		state.usage = metered
		r = r.WithContext(context.WithValue(r.Context(), accountContextKey{}, identity.AccountID))
		defer func() {
			incomplete := r.Context().Err() != nil
			if writer, ok := w.(*observedWriter); ok {
				_, _, writeFailed := writer.snapshot()
				incomplete = incomplete || writeFailed
			}
			status, outcome, prompt, completion := metered.result(incomplete)
			finish := UsageFinish{RequestID: state.id, Backend: state.backend, Status: status,
				Outcome: outcome, PromptTokens: prompt, CompletionTokens: completion}
			// Client cancellation must not cancel the accounting write.
			ctx, cancel := context.WithTimeout(context.Background(), accountQueryTimeout)
			defer cancel()
			if err := s.accountStore.FinishUsage(ctx, finish); err != nil {
				s.observability.meteringErrors.WithLabelValues("finish").Inc()
				s.observability.logger.Error("could not finish usage record", "request_id", state.id, "error", err)
			} else {
				s.observability.metered.WithLabelValues(outcome).Inc()
			}
		}()
		proxy.ServeHTTP(w, r)
		return
	}
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
