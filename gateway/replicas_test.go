package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestReplicaRoundRobin(t *testing.T) {
	var calls [2]atomic.Int64
	models := make([]Model, 0, 2)
	for i := range calls {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls[i].Add(1)
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"model":"qwen"}` {
				t.Errorf("unexpected request: %s", body)
			}
			fmt.Fprint(w, i)
		}))
		t.Cleanup(backend.Close)
		models = append(models, Model{Name: "qwen", URL: backend.URL})
	}
	_, front := newProxyTestServer(t, models)
	for i := 0; i < 6; i++ {
		res := postCompletion(t, front.URL, `{"model":"qwen"}`)
		got := readBody(t, res)
		res.Body.Close()
		if res.StatusCode != http.StatusOK || got != fmt.Sprint(i%2) {
			t.Fatalf("request %d: status=%d body=%q", i, res.StatusCode, got)
		}
	}
	t.Run("concurrent", func(t *testing.T) {
		for i := 0; i < 40; i++ {
			t.Run(fmt.Sprint(i), func(t *testing.T) {
				t.Parallel()
				res := postCompletion(t, front.URL, `{"model":"qwen"}`)
				defer res.Body.Close()
				got := readBody(t, res)
				if res.StatusCode != http.StatusOK || (got != "0" && got != "1") {
					t.Errorf("status=%d body=%q", res.StatusCode, got)
				}
			})
		}
	})
	for i := range calls {
		if got := calls[i].Load(); got != 23 {
			t.Errorf("replica %d: got %d calls, want 23", i, got)
		}
	}
}

func TestReplicaFailureDoesNotRetry(t *testing.T) {
	failed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	failed.Close()
	var calls atomic.Int64
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: healthy\n\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(healthy.Close)
	_, front := newProxyTestServer(t, []Model{{Name: "qwen", URL: failed.URL}, {Name: "qwen", URL: healthy.URL}})
	for i, want := range []int{502, 200, 502, 200} {
		res := postCompletion(t, front.URL, `{"model":"qwen","stream":true}`)
		body := readBody(t, res)
		res.Body.Close()
		if res.StatusCode != want {
			t.Fatalf("request %d: status=%d body=%q", i, res.StatusCode, body)
		}
		if want == 200 && body != "data: healthy\n\ndata: [DONE]\n\n" {
			t.Fatalf("stream=%q", body)
		}
		if got := calls.Load(); got != int64((i+1)/2) {
			t.Fatalf("unexpected retry: healthy calls=%d", got)
		}
	}
}
