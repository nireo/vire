package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeSignupStore struct {
	result SignupResult
	err    error
	email  string
	name   string
}

func (f *fakeSignupStore) Signup(_ context.Context, email, name string) (SignupResult, error) {
	f.email, f.name = email, name
	return f.result, f.err
}

func portalServer(t *testing.T, signup SignupStore, webDir string) *Server {
	t.Helper()
	server, err := NewServerWithOptions(":0", writeRegistry(t, `[{"name":"zeta","url":"http://localhost:8000"},{"name":"alpha","url":"http://localhost:8001"}]`), Options{SignupStore: signup, WebDir: webDir})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestPortalModelsExposeNamesOnly(t *testing.T) {
	res := httptest.NewRecorder()
	portalServer(t, nil, "").Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	if got := res.Body.String(); got != `{"models":["alpha","zeta"]}`+"\n" {
		t.Fatalf("models = %s", got)
	}
}

func TestPortalSignup(t *testing.T) {
	store := &fakeSignupStore{result: SignupResult{AccountID: "account", KeyID: "key", APIKey: "vire_secret"}}
	server := portalServer(t, store, "")
	request := httptest.NewRequest(http.MethodPost, "/api/signup", strings.NewReader(`{"email":"  USER@Example.com ","name":" Team "}`))
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, request)
	if res.Code != http.StatusCreated || store.email != "user@example.com" || store.name != "Team" {
		t.Fatalf("status=%d email=%q name=%q body=%s", res.Code, store.email, store.name, res.Body.String())
	}
	if res.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("signup response must not be cached")
	}
	var result SignupResult
	if err := json.Unmarshal(res.Body.Bytes(), &result); err != nil || result != store.result {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestPortalSignupErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   string
		store  SignupStore
		status int
	}{
		{"unavailable", `{}`, nil, http.StatusServiceUnavailable},
		{"bad email", `{"email":"bad","name":"Team"}`, &fakeSignupStore{}, http.StatusBadRequest},
		{"extra field", `{"email":"a@example.com","name":"Team","role":"owner"}`, &fakeSignupStore{}, http.StatusBadRequest},
		{"trailing JSON", `{"email":"a@example.com","name":"Team"}{}`, &fakeSignupStore{}, http.StatusBadRequest},
		{"duplicate", `{"email":"a@example.com","name":"Team"}`, &fakeSignupStore{err: ErrEmailTaken}, http.StatusConflict},
		{"store failure", `{"email":"a@example.com","name":"Team"}`, &fakeSignupStore{err: errors.New("database unavailable")}, http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			res := httptest.NewRecorder()
			portalServer(t, test.store, "").Handler().ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/api/signup", strings.NewReader(test.body)))
			if res.Code != test.status {
				t.Fatalf("status=%d want=%d body=%s", res.Code, test.status, res.Body.String())
			}
		})
	}
}

func TestPortalServesBuiltFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("portal"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	portalServer(t, nil, dir).Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/", nil))
	if res.Code != http.StatusOK || res.Body.String() != "portal" {
		t.Fatalf("status=%d body=%q", res.Code, res.Body.String())
	}
}
