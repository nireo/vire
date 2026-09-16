package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMultiServerRouting(t *testing.T) {
	newBackend := func(name string) *httptest.Server {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request struct {
				Model  string `json:"model"`
				Stream bool   `json:"stream"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if request.Model != name {
				t.Errorf("backend %s received model %s", name, request.Model)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if request.Stream {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "data: {\"model\":%q}\n\n", name)
				w.(http.Flusher).Flush()
				io.WriteString(w, "data: [DONE]\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"model":%q}`, name)
		}))
		t.Cleanup(backend.Close)
		return backend
	}
	first, second := newBackend("first"), newBackend("second")
	_, front := newProxyTestServer(t, []Model{
		{Name: "first", URL: first.URL},
		{Name: "second", URL: second.URL},
	})

	// Exercise both destinations concurrently, including streaming responses.
	t.Run("concurrent routes", func(t *testing.T) {
		for i := 0; i < 24; i++ {
			name := []string{"first", "second"}[i%2]
			stream := i%4 < 2
			t.Run(fmt.Sprintf("%s/stream=%t/%d", name, stream, i), func(t *testing.T) {
				t.Parallel()
				res := postCompletion(t, front.URL, fmt.Sprintf(`{"model":%q,"stream":%t}`, name, stream))
				defer res.Body.Close()
				want := fmt.Sprintf(`{"model":%q}`, name)
				if stream {
					want = "data: " + want + "\n\ndata: [DONE]\n\n"
				}
				if got := readBody(t, res); res.StatusCode != http.StatusOK || got != want {
					t.Fatalf("status=%d body=%q, want %q", res.StatusCode, got, want)
				}
			})
		}
	})

	first.Close()
	for _, tc := range []struct {
		model  string
		status int
	}{
		{"first", http.StatusBadGateway},
		{"second", http.StatusOK},
		{"unknown", http.StatusNotFound},
	} {
		res := postCompletion(t, front.URL, fmt.Sprintf(`{"model":%q}`, tc.model))
		body := readBody(t, res)
		res.Body.Close()
		if res.StatusCode != tc.status {
			t.Fatalf("model=%s status=%d body=%q", tc.model, res.StatusCode, body)
		}
		if tc.status == http.StatusOK && !strings.Contains(body, `"model":"second"`) {
			t.Fatalf("healthy backend response: %q", body)
		}
	}
}
