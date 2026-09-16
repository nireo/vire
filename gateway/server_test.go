package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	res := httptest.NewRecorder()

	newTestServer(t).Handler().ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, res.Code)
	}
	if got := res.Body.String(); got != "ok\n" {
		t.Fatalf("expected body %q, got %q", "ok\n", got)
	}
}

func TestModels(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/models", nil)
	res := httptest.NewRecorder()

	newTestServer(t).Handler().ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("expected status %d, got %d", http.StatusOK, res.Code)
	}

	var got []Model
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	want := []Model{{Name: "test", URL: "http://localhost:8000"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("expected models %#v, got %#v", want, got)
	}
}

func TestNewServerLoadsModels(t *testing.T) {
	server := newTestServer(t)
	want := []Model{{Name: "test", URL: "http://localhost:8000"}}

	if !reflect.DeepEqual(server.models, want) {
		t.Fatalf("expected models %#v, got %#v", want, server.models)
	}
}

func TestNewServerRejectsInvalidRegistry(t *testing.T) {
	for _, contents := range []string{
		`null`, `{}`, `{`,
		`[{"name":"test"}]`,
		`[{"name":" ","url":"http://localhost:8000"}]`,
		`[{"name":"test","url":"localhost:8000"}]`,
		`[{"name":"test","url":"ftp://localhost:8000"}]`,
		`[{"name":"test","url":"http://"}]`,
		`[{"name":"test","url":"http://localhost:abc"}]`,
		`[{"name":"test","url":"http://user:secret@localhost:8000"}]`,
		`[{"name":"test","url":"http://localhost:8000/v1"}]`,
		`[{"name":"test","url":"http://localhost:8000?key=value"}]`,
		`[{"name":"test","url":"http://localhost:8000?"}]`,
		`[{"name":"test","url":"http://localhost:8000#fragment"}]`,
		`[{"name":"test","url":"http://localhost:8000"},{"name":"test","url":"http://localhost:8000"}]`,
		`[{"name":"test","url":"http://localhost:8000"},{"name":"test","url":"http://localhost:8000/"}]`,
	} {
		t.Run(contents, func(t *testing.T) {
			if _, err := NewServer(":8080", writeRegistry(t, contents)); err == nil {
				t.Fatal("expected invalid registry to fail")
			}
		})
	}
}

func TestNewServerAcceptsOrigins(t *testing.T) {
	for _, backend := range []string{"http://localhost:8000", "https://example.com/", "http://[::1]:8000"} {
		t.Run(backend, func(t *testing.T) {
			data, err := json.Marshal([]Model{{Name: "test", URL: backend}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewServer(":0", writeRegistry(t, string(data))); err != nil {
				t.Fatalf("valid origin rejected: %v", err)
			}
		})
	}
}

func TestNewServerAcceptsEmptyRegistry(t *testing.T) {
	if _, err := NewServer(":0", writeRegistry(t, `[]`)); err != nil {
		t.Fatal(err)
	}
}

func TestRunStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := newTestServer(t).Run(ctx); err != nil {
		t.Fatalf("expected clean shutdown, got %v", err)
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()

	server, err := NewServer(":0", writeRegistry(t, `[{"name":"test","url":"http://localhost:8000"}]`))
	if err != nil {
		t.Fatalf("create server: %v", err)
	}
	return server
}

func writeRegistry(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write model registry: %v", err)
	}
	return path
}
