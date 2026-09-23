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
	"time"
)

type fakeSignupStore struct {
	result       SignupResult
	err          error
	email        string
	name         string
	password     string
	account      PortalAccount
	usageAccount string
}

func (f *fakeSignupStore) Signup(_ context.Context, email, name, password string) (SignupResult, error) {
	f.email, f.name, f.password = email, name, password
	return f.result, f.err
}
func (f *fakeSignupStore) Login(context.Context, string, string) (PortalSession, error) {
	return PortalSession{}, ErrInvalidCredentials
}
func (f *fakeSignupStore) Session(context.Context, string) (PortalAccount, error) {
	if f.account.AccountID == "" {
		return PortalAccount{}, ErrInvalidSession
	}
	return f.account, nil
}
func (f *fakeSignupStore) Logout(context.Context, string) error { return nil }
func (f *fakeSignupStore) Usage(_ context.Context, accountID string, _ time.Time) ([]PortalUsage, error) {
	f.usageAccount = accountID
	return []PortalUsage{}, nil
}

func portalServer(t *testing.T, signup PortalStore, webDir string) *Server {
	t.Helper()
	server, err := NewServerWithOptions(":0", writeRegistry(t, `[{"name":"zeta","url":"http://localhost:8000"},{"name":"alpha","url":"http://localhost:8001"}]`), Options{PortalStore: signup, WebDir: webDir})
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

func TestPasswordLength(t *testing.T) {
	if validPassword("abcdefghi") || !validPassword("abcdefghij") || !validPassword("éééééééééé") {
		t.Fatal("minimum must be 10 Unicode characters")
	}
	if validPassword(strings.Repeat("é", 129)) {
		t.Fatal("password exceeds the byte limit")
	}
}

func TestPortalSignup(t *testing.T) {
	store := &fakeSignupStore{result: SignupResult{AccountID: "account", KeyID: "key", APIKey: "vire_secret"}}
	server := portalServer(t, store, "")
	request := httptest.NewRequest(http.MethodPost, "/api/signup", strings.NewReader(`{"email":"  USER@Example.com ","name":" Team ","password":"a sufficiently long password"}`))
	request.Header.Set("Content-Type", "application/json")
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, request)
	if res.Code != http.StatusCreated || store.email != "user@example.com" || store.name != "Team" || store.password != "a sufficiently long password" {
		t.Fatalf("status=%d email=%q name=%q body=%s", res.Code, store.email, store.name, res.Body.String())
	}
	if res.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("signup response must not be cached")
	}
	var result SignupResult
	if err := json.Unmarshal(res.Body.Bytes(), &result); err != nil || result.AccountID != store.result.AccountID || result.APIKey != store.result.APIKey {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestPortalSignupErrors(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   string
		store  PortalStore
		status int
	}{
		{"unavailable", `{}`, nil, http.StatusServiceUnavailable},
		{"bad email", `{"email":"bad","name":"Team","password":"a sufficiently long password"}`, &fakeSignupStore{}, http.StatusBadRequest},
		{"extra field", `{"email":"a@example.com","name":"Team","password":"a sufficiently long password","role":"owner"}`, &fakeSignupStore{}, http.StatusBadRequest},
		{"trailing JSON", `{"email":"a@example.com","name":"Team","password":"a sufficiently long password"}{}`, &fakeSignupStore{}, http.StatusBadRequest},
		{"duplicate", `{"email":"a@example.com","name":"Team","password":"a sufficiently long password"}`, &fakeSignupStore{err: ErrEmailTaken}, http.StatusConflict},
		{"store failure", `{"email":"a@example.com","name":"Team","password":"a sufficiently long password"}`, &fakeSignupStore{err: errors.New("database unavailable")}, http.StatusServiceUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			res := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/signup", strings.NewReader(test.body))
			request.Header.Set("Content-Type", "application/json")
			portalServer(t, test.store, "").Handler().ServeHTTP(res, request)
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

func TestPortalUsageUsesSessionAccount(t *testing.T) {
	store := &fakeSignupStore{account: PortalAccount{AccountID: "owned", Email: "user@example.com", Name: "Team"}}
	request := httptest.NewRequest(http.MethodGet, "/api/usage?account_id=other", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "session"})
	res := httptest.NewRecorder()
	portalServer(t, store, "").Handler().ServeHTTP(res, request)
	if res.Code != http.StatusOK || store.usageAccount != "owned" {
		t.Fatalf("status=%d usage account=%q body=%s", res.Code, store.usageAccount, res.Body.String())
	}
}

func TestPortalRejectsCrossOriginSignup(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/api/signup", strings.NewReader(`{"email":"a@example.com","name":"Team","password":"a sufficiently long password"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://elsewhere.example")
	res := httptest.NewRecorder()
	portalServer(t, &fakeSignupStore{}, "").Handler().ServeHTTP(res, request)
	if res.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", res.Code, res.Body.String())
	}
}
