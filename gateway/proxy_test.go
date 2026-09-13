package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCompletionsRoutesAndPreservesRequest(t *testing.T) {
	const body = "{\n  \"model\": \"second\", \"Model\": \"first\", \"messages\": [], \"future_vllm_field\": {\"enabled\": true}\n}"
	type received struct {
		body, method, uri, host, contentType, custom, forwarded, xForwarded string
	}
	requests := make(chan received, 1)
	wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("request reached the wrong backend")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(wrong.Close)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
		}
		requests <- received{string(data), r.Method, r.URL.RequestURI(), r.Host,
			r.Header.Get("Content-Type"), r.Header.Get("X-Custom"),
			r.Header.Get("Forwarded"), r.Header.Get("X-Forwarded-For")}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream", "preserved")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"choices":[],"extra":"unchanged"}`)
	}))
	t.Cleanup(backend.Close)
	_, front := newProxyTestServer(t, []Model{{Name: "first", URL: wrong.URL}, {Name: "second", URL: backend.URL}})
	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions?trace=1", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Custom", "preserved")
	req.Header.Set("Forwarded", "for=spoofed")
	req.Header.Set("X-Forwarded-For", "spoofed")
	res, err := testHTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if got := readBody(t, res); got != `{"choices":[],"extra":"unchanged"}` || res.StatusCode != http.StatusCreated || res.Header.Get("X-Upstream") != "preserved" {
		t.Fatalf("unexpected response: status=%d headers=%v body=%q", res.StatusCode, res.Header, got)
	}
	got := <-requests
	want := received{body, http.MethodPost, "/v1/chat/completions?trace=1", strings.TrimPrefix(backend.URL, "http://"), "application/json", "preserved", "", ""}
	if got != want {
		t.Fatalf("upstream request: got %#v, want %#v", got, want)
	}
}

func TestCompletionsRejectsInvalidRequests(t *testing.T) {
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)
	_, front := newProxyTestServer(t, []Model{{Name: "test", URL: backend.URL}})
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"empty", "", http.StatusBadRequest},
		{"malformed", `{`, http.StatusBadRequest},
		{"trailing JSON", `{"model":"test"} {}`, http.StatusBadRequest},
		{"null", `null`, http.StatusBadRequest},
		{"array", `[]`, http.StatusBadRequest},
		{"missing model", `{"messages":[]}`, http.StatusBadRequest},
		{"uppercase model key", `{"MODEL":"test"}`, http.StatusBadRequest},
		{"titlecase model key", `{"Model":"test"}`, http.StatusBadRequest},
		{"empty model", `{"model":""}`, http.StatusBadRequest},
		{"wrong type", `{"model":123}`, http.StatusBadRequest},
		{"unknown model", `{"model":"other"}`, http.StatusNotFound},
		{"oversized", `{"model":"test","padding":"` + strings.Repeat("x", maxRequestBytes) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := postCompletion(t, front.URL, tc.body)
			defer res.Body.Close()
			body := readBody(t, res)
			if res.StatusCode != tc.status || res.Header.Get("Content-Type") != "application/json" || !json.Valid([]byte(body)) {
				t.Fatalf("status=%d body=%q", res.StatusCode, body)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("invalid requests reached backend %d times", calls.Load())
	}
}

func TestCompletionsAcceptsBodyAtLimit(t *testing.T) {
	const prefix = `{"model":"test","padding":"`
	const suffix = `"}`
	body := prefix + strings.Repeat("x", maxRequestBytes-len(prefix)-len(suffix)) + suffix
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != body {
			t.Errorf("body was not preserved at the size limit: bytes=%d err=%v", len(data), err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(backend.Close)
	_, front := newProxyTestServer(t, []Model{{Name: "test", URL: backend.URL}})
	res := postCompletion(t, front.URL, body)
	defer res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("status=%d body=%q", res.StatusCode, readBody(t, res))
	}
}

func TestCompletionsPreservesUpstreamErrorsWithoutRetry(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			const body = `{"error":{"message":"backend error","code":"custom_code"}}`
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "3")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(backend.Close)
			_, front := newProxyTestServer(t, []Model{{Name: "test", URL: backend.URL}})
			res := postCompletion(t, front.URL, `{"model":"test"}`)
			defer res.Body.Close()
			if got := readBody(t, res); res.StatusCode != status || got != body || res.Header.Get("Retry-After") != "3" {
				t.Fatalf("status=%d headers=%v body=%q", res.StatusCode, res.Header, got)
			}
			if calls.Load() != 1 {
				t.Fatalf("expected one request, got %d", calls.Load())
			}
		})
	}
}

func TestCompletionsUnavailableBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	backend.Close()
	_, front := newProxyTestServer(t, []Model{{Name: "test", URL: backend.URL}})
	res := postCompletion(t, front.URL, `{"model":"test"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status=%d body=%q", res.StatusCode, readBody(t, res))
	}
}

func TestCompletionsHeaderTimeout(t *testing.T) {
	canceled := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		<-r.Context().Done()
		close(canceled)
	}))
	t.Cleanup(backend.Close)
	server, front := newProxyTestServer(t, []Model{{Name: "test", URL: backend.URL}})
	server.transport.ResponseHeaderTimeout = 50 * time.Millisecond
	res := postCompletion(t, front.URL, `{"model":"test"}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusGatewayTimeout {
		t.Fatalf("status=%d body=%q", res.StatusCode, readBody(t, res))
	}
	waitSignal(t, canceled, "upstream cancellation after timeout")
}

func TestCompletionsStreamsIncrementally(t *testing.T) {
	const first = "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n"
	const rest = "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	release := make(chan struct{})
	var releaseOnce sync.Once
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		select {
		case <-release:
			_, _ = io.WriteString(w, rest)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(backend.Close)
	_, front := newProxyTestServer(t, []Model{{Name: "test", URL: backend.URL}})
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	res := postCompletion(t, front.URL, `{"model":"test","stream":true}`)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("unexpected streaming response: %d %v", res.StatusCode, res.Header)
	}
	// The backend cannot finish until we have received the first event.
	reader := bufio.NewReader(res.Body)
	chunk := make([]byte, len(first))
	if _, err := io.ReadFull(reader, chunk); err != nil || string(chunk) != first {
		t.Fatalf("first event=%q err=%v", chunk, err)
	}
	releaseOnce.Do(func() { close(release) })
	remaining, err := io.ReadAll(reader)
	if err != nil || string(remaining) != rest {
		t.Fatalf("remaining events=%q err=%v", remaining, err)
	}
}

func TestCompletionsBrokenStream(t *testing.T) {
	const first = "data: {}\n\n"
	release := make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, first)
		w.(http.Flusher).Flush()
		select {
		case <-release:
			panic(http.ErrAbortHandler) // Simulate an upstream disconnect mid-generation.
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(backend.Close)
	_, front := newProxyTestServer(t, []Model{{Name: "test", URL: backend.URL}})
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	res := postCompletion(t, front.URL, `{"model":"test","stream":true}`)
	defer res.Body.Close()
	chunk := make([]byte, len(first))
	if _, err := io.ReadFull(res.Body, chunk); err != nil || string(chunk) != first {
		t.Fatalf("first event=%q err=%v", chunk, err)
	}
	once.Do(func() { close(release) })
	rest, err := io.ReadAll(res.Body)
	if err == nil || len(rest) != 0 || calls.Load() != 1 {
		t.Fatalf("broken stream should abort without retry or fabricated data: rest=%q err=%v calls=%d", rest, err, calls.Load())
	}
}

func TestCompletionsCancellation(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "before headers"
		if streaming {
			name = "during stream"
		}
		t.Run(name, func(t *testing.T) {
			started := make(chan struct{})
			streamRead := make(chan struct{})
			canceled := make(chan struct{})
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {}\n\n")
					w.(http.Flusher).Flush()
				}
				close(started)
				<-r.Context().Done()
				close(canceled)
			}))
			t.Cleanup(backend.Close)
			_, front := newProxyTestServer(t, []Model{{Name: "test", URL: backend.URL}})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test","stream":true}`))
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				res, err := testHTTPClient().Do(req)
				if err == nil {
					defer res.Body.Close()
					if streaming {
						chunk := make([]byte, len("data: {}\n\n"))
						if _, err := io.ReadFull(res.Body, chunk); err == nil {
							close(streamRead)
						}
						_, _ = io.Copy(io.Discard, res.Body)
					}
				}
			}()
			waitSignal(t, started, "backend request")
			if streaming {
				waitSignal(t, streamRead, "first event at the client")
			}
			cancel()
			waitSignal(t, canceled, "upstream cancellation")
			waitSignal(t, done, "client exit")
		})
	}
}

func newProxyTestServer(t *testing.T, models []Model) (*Server, *httptest.Server) {
	t.Helper()
	data, err := json.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer("127.0.0.1:0", writeRegistry(t, string(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.transport.CloseIdleConnections)
	front := httptest.NewServer(server.Handler())
	t.Cleanup(front.Close)
	return server, front
}

func testHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second}
}

func postCompletion(t *testing.T, baseURL, body string) *http.Response {
	t.Helper()
	res, err := testHTTPClient().Post(baseURL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func readBody(t *testing.T, res *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}
