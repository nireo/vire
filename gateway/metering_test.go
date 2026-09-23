package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type recordingAccountStore struct {
	mu       sync.Mutex
	starts   []UsageStart
	finishes []UsageFinish
	beginErr error
}

func (s *recordingAccountStore) Authenticate(_ context.Context, token string) (KeyIdentity, error) {
	if token != "test-secret" {
		return KeyIdentity{}, ErrInvalidKey
	}
	return KeyIdentity{AccountID: "account-1", KeyID: "key-1"}, nil
}

func (s *recordingAccountStore) BeginUsage(_ context.Context, start UsageStart) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.beginErr != nil {
		return s.beginErr
	}
	s.starts = append(s.starts, start)
	return nil
}

func (s *recordingAccountStore) FinishUsage(_ context.Context, finish UsageFinish) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finishes = append(s.finishes, finish)
	return nil
}

func (s *recordingAccountStore) snapshot() ([]UsageStart, []UsageFinish) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]UsageStart(nil), s.starts...), append([]UsageFinish(nil), s.finishes...)
}

func meteredTestServer(t *testing.T, backend http.Handler, store *recordingAccountStore) (*httptest.Server, *httptest.Server) {
	t.Helper()
	upstream := httptest.NewServer(backend)
	t.Cleanup(upstream.Close)
	registry, err := json.Marshal([]Model{{Name: "test", URL: upstream.URL}})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServerWithOptions("127.0.0.1:0", writeRegistry(t, string(registry)), Options{
		AccountStore: store, BackendAPIKey: "backend-secret", MaxInflight: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.transport.CloseIdleConnections)
	front := httptest.NewServer(server.Handler())
	t.Cleanup(front.Close)
	return upstream, front
}

func TestMeteredAuthenticationAndUsage(t *testing.T) {
	store := &recordingAccountStore{}
	var backendCalls atomic.Int32
	_, front := meteredTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendCalls.Add(1)
		if got := r.Header.Get("Authorization"); got != "Bearer backend-secret" {
			t.Errorf("upstream authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"prompt_tokens":12,"completion_tokens":3},"choices":[]}`)
	}), store)
	request := func(key string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
		if err != nil {
			t.Fatal(err)
		}
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		res, err := testHTTPClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		return res
	}
	if got := request("").StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d", got)
	}
	if got := request("wrong").StatusCode; got != http.StatusUnauthorized {
		t.Fatalf("wrong key status = %d", got)
	}
	if got := request("test-secret").StatusCode; got != http.StatusOK {
		t.Fatalf("valid key status = %d", got)
	}
	starts, finishes := store.snapshot()
	if backendCalls.Load() != 1 || len(starts) != 1 || len(finishes) != 1 {
		t.Fatalf("calls=%d starts=%d finishes=%d", backendCalls.Load(), len(starts), len(finishes))
	}
	if starts[0].RequestID == "" || starts[0].RequestID != finishes[0].RequestID || starts[0].AccountID != "account-1" || starts[0].KeyID != "key-1" || starts[0].Model != "test" {
		t.Fatalf("start=%+v finish=%+v", starts[0], finishes[0])
	}
	if finishes[0].Outcome != "complete" || *finishes[0].PromptTokens != 12 || *finishes[0].CompletionTokens != 3 {
		t.Fatalf("finish=%+v", finishes[0])
	}
	res, err := testHTTPClient().Get(front.URL + "/models")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("raw registry status = %d", res.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, front.URL+"/v1/models", nil)
	req.Header.Set("Authorization", "Bearer test-secret")
	res, err = testHTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusOK || !strings.Contains(string(body), `"id":"test"`) || strings.Contains(string(body), "127.0.0.1") {
		t.Fatalf("public models: status=%d body=%s", res.StatusCode, body)
	}
}

func TestMeteredStreamAndUnknownUsage(t *testing.T) {
	store := &recordingAccountStore{}
	_, front := meteredTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\n")
		w.(http.Flusher).Flush()
		if r.URL.Query().Get("incomplete") == "1" {
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":4},\"choices\":[]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}), store)
	for _, path := range []string{"/v1/chat/completions", "/v1/chat/completions?incomplete=1"} {
		req, _ := http.NewRequest(http.MethodPost, front.URL+path, strings.NewReader(`{"model":"test","stream":true}`))
		req.Header.Set("Authorization", "Bearer test-secret")
		res, err := testHTTPClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status = %d", res.StatusCode)
		}
	}
	_, finishes := store.snapshot()
	if len(finishes) != 2 || finishes[0].Outcome != "complete" || *finishes[0].PromptTokens != 7 || *finishes[0].CompletionTokens != 4 || finishes[1].Outcome != "incomplete" || finishes[1].PromptTokens != nil {
		t.Fatalf("stream finishes = %+v", finishes)
	}
}

func TestMeteredStreamReachesClientBeforeCompletion(t *testing.T) {
	store := &recordingAccountStore{}
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseBackend := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseBackend)
	_, front := meteredTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\n")
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":4}}\n\ndata: [DONE]\n\n")
	}), store)
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test","stream":true}`))
	req.Header.Set("Authorization", "Bearer test-secret")
	res, err := testHTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	reader := bufio.NewReader(res.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != "data: {\"choices\":[]}\n" {
		t.Fatalf("first event=%q err=%v", first, err)
	}
	releaseBackend()
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatal(err)
	}
}

func TestMeteringBeginFailureDoesNotDispatch(t *testing.T) {
	store := &recordingAccountStore{beginErr: errors.New("database down")}
	var called atomic.Bool
	_, front := meteredTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusOK)
	}), store)
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"test"}`))
	req.Header.Set("Authorization", "Bearer test-secret")
	res, err := testHTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable || called.Load() {
		t.Fatalf("status=%d backend called=%v", res.StatusCode, called.Load())
	}
}
