package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func replaceRegistry(t *testing.T, path string, models []Model) {
	t.Helper()
	data, err := json.Marshal(models)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMembershipReloadConvergesAcrossGateways(t *testing.T) {
	newBackend := func(name string) *httptest.Server {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				w.WriteHeader(http.StatusOK)
				return
			}
			w.Header().Set("X-Replica", name)
			io.WriteString(w, `{}`)
		}))
		t.Cleanup(backend.Close)
		return backend
	}
	a, b, c := newBackend("a"), newBackend("b"), newBackend("c")
	initial := []Model{{Name: "qwen", URL: a.URL}, {Name: "qwen", URL: b.URL}}
	firstPath := writeRegistry(t, `[]`)
	secondPath := writeRegistry(t, `[]`)
	replaceRegistry(t, firstPath, initial)
	replaceRegistry(t, secondPath, []Model{initial[1], initial[0]})
	first, err := NewServer(":0", firstPath)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewServer(":0", secondPath)
	if err != nil {
		t.Fatal(err)
	}
	frontA, frontB := httptest.NewServer(first.Handler()), httptest.NewServer(second.Handler())
	t.Cleanup(frontA.Close)
	t.Cleanup(frontB.Close)
	if first.routing.Load().fingerprint != second.routing.Load().fingerprint {
		t.Fatal("registry order changed membership fingerprint")
	}
	request := func(base, id string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(`{"model":"qwen"}`))
		req.Header.Set("X-Conversation-ID", id)
		res, err := testHTTPClient().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		io.Copy(io.Discard, res.Body)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("status=%d", res.StatusCode)
		}
		return res.Header.Get("X-Replica")
	}
	for i := 0; i < 25; i++ {
		id := fmt.Sprint(i)
		if request(frontA.URL, id) != request(frontB.URL, id) {
			t.Fatalf("gateways disagree before reload for %s", id)
		}
	}
	updated := append(initial, Model{Name: "qwen", URL: c.URL})
	replaceRegistry(t, firstPath, updated)
	replaceRegistry(t, secondPath, []Model{updated[2], updated[1], updated[0]})
	for _, server := range []*Server{first, second} {
		if err := server.ReloadRegistry(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if first.routing.Load().fingerprint != second.routing.Load().fingerprint {
		t.Fatal("gateways disagree on new membership fingerprint")
	}
	for i := 0; i < 25; i++ {
		id := fmt.Sprint(i)
		if request(frontA.URL, id) != request(frontB.URL, id) {
			t.Fatalf("gateways disagree after reload for %s", id)
		}
	}
}

func TestMembershipReloadDrainsSelectedStream(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	old := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: first\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-release
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(old.Close)
	newer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		io.WriteString(w, "new")
	}))
	t.Cleanup(newer.Close)
	path := writeRegistry(t, `[]`)
	replaceRegistry(t, path, []Model{{Name: "qwen", URL: old.URL}})
	server, err := NewServer(":0", path)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(server.Handler())
	t.Cleanup(front.Close)
	request, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"qwen","stream":true}`))
	res, err := testHTTPClient().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not start")
	}
	replaceRegistry(t, path, []Model{{Name: "qwen", URL: newer.URL}})
	if err := server.ReloadRegistry(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := postCompletion(t, front.URL, `{"model":"qwen"}`)
	if body := readBody(t, second); second.StatusCode != 200 || body != "new" {
		t.Fatalf("new request status=%d body=%q", second.StatusCode, body)
	}
	second.Body.Close()
	releaseOnce.Do(func() { close(release) })
	if body, err := io.ReadAll(res.Body); err != nil || string(body) != "data: first\n\ndata: [DONE]\n\n" {
		t.Fatalf("draining stream body=%q err=%v", body, err)
	}
}

func TestHealthFailoverAndRecovery(t *testing.T) {
	var ready [2]atomic.Bool
	var calls [2]atomic.Int64
	models := make([]Model, 0, 2)
	for i := range ready {
		ready[i].Store(true)
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				if !ready[i].Load() {
					w.WriteHeader(http.StatusServiceUnavailable)
				}
				return
			}
			calls[i].Add(1)
			fmt.Fprint(w, i)
		}))
		t.Cleanup(backend.Close)
		models = append(models, Model{Name: "qwen", URL: backend.URL})
	}
	path := writeRegistry(t, `[]`)
	replaceRegistry(t, path, models)
	server, err := NewServer(":0", path)
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(server.Handler())
	t.Cleanup(front.Close)
	route := server.routing.Load().proxies["qwen"]
	id := ""
	for i := 0; i < 100; i++ {
		candidate := fmt.Sprint(i)
		if route.selectBackend(candidate) == route.backends[0] {
			id = candidate
			break
		}
	}
	if id == "" {
		t.Fatal("could not find affinity key for first backend")
	}
	ready[0].Store(false)
	server.probeBackends(context.Background())
	req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(`{"model":"qwen"}`))
	req.Header.Set("X-Conversation-ID", id)
	res, err := testHTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if body := readBody(t, res); res.StatusCode != 200 || body != "1" {
		t.Fatalf("failover status=%d body=%q", res.StatusCode, body)
	}
	res.Body.Close()
	if calls[0].Load() != 0 || calls[1].Load() != 1 {
		t.Fatalf("requests dispatched to unhealthy backend: %d, %d", calls[0].Load(), calls[1].Load())
	}
	ready[1].Store(false)
	server.probeBackends(context.Background())
	res = postCompletion(t, front.URL, `{"model":"qwen"}`)
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("all backends unhealthy: status=%d", res.StatusCode)
	}
	ready[0].Store(true)
	server.probeBackends(context.Background())
	res = postCompletion(t, front.URL, `{"model":"qwen"}`)
	if body := readBody(t, res); res.StatusCode != 200 || body != "0" {
		t.Fatalf("recovery status=%d body=%q", res.StatusCode, body)
	}
	res.Body.Close()
}

func TestReloadRejectsUnreadyBackendAndCatalogChanges(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			io.WriteString(w, "healthy")
		}
	}))
	t.Cleanup(healthy.Close)
	unready := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(unready.Close)
	path := writeRegistry(t, `[]`)
	replaceRegistry(t, path, []Model{{Name: "qwen", URL: healthy.URL}})
	server, err := NewServer(":0", path)
	if err != nil {
		t.Fatal(err)
	}
	before := server.routing.Load().fingerprint
	for _, models := range [][]Model{
		{{Name: "qwen", URL: healthy.URL}, {Name: "qwen", URL: unready.URL}},
		{{Name: "other", URL: healthy.URL}},
	} {
		replaceRegistry(t, path, models)
		if err := server.ReloadRegistry(context.Background()); err == nil {
			t.Fatal("expected registry reload rejection")
		}
		if server.routing.Load().fingerprint != before {
			t.Fatal("rejected registry was published")
		}
	}
}

func TestRegistryWatcherPublishesMountedFileChange(t *testing.T) {
	newBackend := func() *httptest.Server {
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				w.WriteHeader(http.StatusOK)
			}
		}))
		t.Cleanup(backend.Close)
		return backend
	}
	first, second := newBackend(), newBackend()
	path := writeRegistry(t, `[]`)
	initial := []Model{{Name: "qwen", URL: first.URL}}
	replaceRegistry(t, path, initial)
	server, err := NewServer(":0", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		server.watchRegistry(ctx)
		close(done)
	}()
	t.Cleanup(func() { cancel(); <-done })
	updated := append(initial, Model{Name: "qwen", URL: second.URL})
	replaceRegistry(t, path, updated)
	want := membershipFingerprint(updated)
	deadline := time.After(5 * time.Second)
	for server.routing.Load().fingerprint != want {
		select {
		case <-deadline:
			t.Fatal("mounted registry change was not published")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
