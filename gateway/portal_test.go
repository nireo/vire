package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	chatKeyID    string
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
func (f *fakeSignupStore) ChatKeyID(context.Context, string) (string, error) { return f.chatKeyID, nil }

func portalServer(t *testing.T, signup PortalStore, webDir string) *Server {
	t.Helper()
	server, err := NewServerWithOptions(":0", writeRegistry(t, `[{"name":"zeta","url":"http://localhost:8000"},{"name":"alpha","url":"http://localhost:8001"}]`), Options{PortalStore: signup, WebDir: webDir})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestPortalModelsExposeMetadataWithoutOrigins(t *testing.T) {
	res := httptest.NewRecorder()
	portalServer(t, nil, "").Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d", res.Code)
	}
	if got := res.Body.String(); got != `{"models":[{"id":"alpha","display_name":"alpha","pricing":null},{"id":"zeta","display_name":"zeta","pricing":null}]}`+"\n" {
		t.Fatalf("models = %s", got)
	}
}

func TestPortalModelsShowPricesAndDeduplicateReplicas(t *testing.T) {
	registry := `[{"name":"qwen","url":"http://localhost:8000","display_name":"Qwen 2.5 0.5B Instruct","pricing":{"input_rate_micro_per_million":100000,"output_rate_micro_per_million":200000,"example":true}},{"name":"qwen","url":"http://localhost:8001","display_name":"Qwen 2.5 0.5B Instruct","pricing":{"input_rate_micro_per_million":100000,"output_rate_micro_per_million":200000,"example":true}}]`
	server, err := NewServer(":0", writeRegistry(t, registry))
	if err != nil {
		t.Fatal(err)
	}
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, "/api/models", nil))
	if got := res.Body.String(); got != `{"models":[{"id":"qwen","display_name":"Qwen 2.5 0.5B Instruct","pricing":{"input_usd_per_million":0.1,"output_usd_per_million":0.2,"example":true}}]}`+"\n" {
		t.Fatalf("models = %s", got)
	}
	if got := len(server.PricedModels()); got != 1 {
		t.Fatalf("priced replicas = %d", got)
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
	for _, path := range []string{"/", "/chat"} {
		res := httptest.NewRecorder()
		portalServer(t, nil, dir).Handler().ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusOK || res.Body.String() != "portal" {
			t.Fatalf("path=%s status=%d body=%q", path, res.Code, res.Body.String())
		}
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

func TestPortalChatStreamsWithSessionAndMetersUsage(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "Bearer backend-secret" {
			t.Errorf("backend request path=%q cookie=%q authorization=%q", r.URL.Path, r.Header.Get("Cookie"), r.Header.Get("Authorization"))
		}
		var input struct {
			Model    string                           `json:"model"`
			Messages []struct{ Role, Content string } `json:"messages"`
			Stream   bool                             `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil || input.Model != "test" || !input.Stream || len(input.Messages) != 1 || input.Messages[0].Content != "Hello" {
			t.Errorf("backend input=%+v err=%v", input, err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1},\"choices\":[]}\n\ndata: [DONE]\n\n")
	}))
	defer backend.Close()
	registry, _ := json.Marshal([]Model{{Name: "test", URL: backend.URL}})
	accounts := &recordingAccountStore{}
	portal := &fakeSignupStore{account: PortalAccount{AccountID: "account-1"}, chatKeyID: "key-1"}
	server, err := NewServerWithOptions(":0", writeRegistry(t, string(registry)), Options{AccountStore: accounts, PortalStore: portal, BackendAPIKey: "backend-secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer server.transport.CloseIdleConnections()
	request := httptest.NewRequest(http.MethodPost, "/api/chat/completions", strings.NewReader(`{"model":"test","messages":[{"role":"user","content":"Hello"}],"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer browser-supplied")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "session"})
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, request)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "data: [DONE]") || res.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d headers=%v body=%s", res.Code, res.Header(), res.Body.String())
	}
	starts, finishes := accounts.snapshot()
	if len(starts) != 1 || starts[0].AccountID != "account-1" || starts[0].KeyID != "key-1" || len(finishes) != 1 || finishes[0].Outcome != "complete" {
		t.Fatalf("usage starts=%+v finishes=%+v", starts, finishes)
	}
}

func TestPortalChatRequiresSessionAndActiveKey(t *testing.T) {
	for _, test := range []struct {
		name   string
		store  *fakeSignupStore
		origin string
		want   int
	}{
		{"signed out", &fakeSignupStore{}, "", http.StatusUnauthorized},
		{"no active key", &fakeSignupStore{account: PortalAccount{AccountID: "account-1"}}, "", http.StatusForbidden},
		{"cross origin", &fakeSignupStore{account: PortalAccount{AccountID: "account-1"}, chatKeyID: "key-1"}, "https://elsewhere.example", http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := portalServer(t, test.store, "")
			server.accountStore = &recordingAccountStore{}
			request := httptest.NewRequest(http.MethodPost, "/api/chat/completions", strings.NewReader(`{"model":"alpha"}`))
			request.Header.Set("Content-Type", "application/json")
			if test.origin != "" {
				request.Header.Set("Origin", test.origin)
			}
			request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "session"})
			res := httptest.NewRecorder()
			server.Handler().ServeHTTP(res, request)
			if res.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", res.Code, test.want, res.Body.String())
			}
		})
	}
}
