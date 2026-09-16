package gateway

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAffinitySelection(t *testing.T) {
	for _, count := range []int{1, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			route := &modelRoute{}
			for i := 0; i < count; i++ {
				route.backends = append(route.backends, &routeBackend{origin: fmt.Sprintf("http://gpu-%d:8000", i)})
			}
			route.buildRing()
			seen := make(map[*routeBackend]bool)
			for i := 0; i < 100; i++ {
				id := fmt.Sprint(i)
				backend := route.selectBackend(id)
				seen[backend] = true
				if route.selectBackend(id) != backend {
					t.Fatal("conversation changed replica")
				}
			}
			if len(seen) != count {
				t.Fatalf("used %d of %d replicas", len(seen), count)
			}
			// Sticky requests must not consume the fallback counter.
			for i := 0; i < 6; i++ {
				if route.selectBackend("") != route.backends[i%count] {
					t.Fatal("incorrect round-robin fallback")
				}
			}
		})
	}
}

func TestConversationAffinityHTTP(t *testing.T) {
	models := []Model{}
	for i := 0; i < 3; i++ {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Conversation-ID") != "" {
				t.Error("routing header leaked upstream")
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
			}
			w.Header().Set("X-Replica", fmt.Sprint(i))
			if strings.Contains(string(body), `"stream":true`) {
				w.Header().Set("Content-Type", "text/event-stream")
			}
			w.Write(body)
		}))
		t.Cleanup(backend.Close)
		models = append(models, Model{Name: "qwen", URL: backend.URL})
	}
	_, front := newProxyTestServer(t, models)
	// A fresh gateway and reversed registry preserve the same assignment.
	reversed := []Model{models[2], models[1], models[0]}
	_, other := newProxyTestServer(t, reversed)
	request := func(t *testing.T, base, body string) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Conversation-ID", "conversation-123")
		res, err := testHTTPClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if got := readBody(t, res); res.StatusCode != 200 || got != body {
			t.Fatalf("status=%d body=%q", res.StatusCode, got)
		}
		return res.Header.Get("X-Replica")
	}
	want := request(t, front.URL, `{"model":"qwen","messages":[]}`)
	for i := 0; i < 24; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			body := fmt.Sprintf(`{"model":"qwen","messages":[{"role":"user","content":"turn %d"}],"stream":%t}`, i, i%2 == 0)
			base := front.URL
			if i%2 == 0 {
				base = other.URL
			}
			if got := request(t, base, body); got != want {
				t.Errorf("affinity changed: got %s want %s", got, want)
			}
		})
	}
}
