package accountdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nireo/vire/gateway"
)

// Set VIRE_TEST_DATABASE_URL to a disposable Postgres database. This test owns
// the account rows it creates but intentionally exercises real migrations and SQL.
func TestAccountLifecycleAndRollup(t *testing.T) {
	url := os.Getenv("VIRE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("VIRE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	signup, err := store.Signup(ctx, "test-"+time.Now().Format("20060102150405.000000000")+"@example.com", "Test account", "a sufficiently long password")
	if err != nil {
		t.Fatal(err)
	}
	accountID := signup.AccountID
	keyID, token, err := store.IssueKey(ctx, accountID, "test key")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := store.Authenticate(ctx, token)
	if err != nil || identity != (gateway.KeyIdentity{AccountID: accountID, KeyID: keyID}) {
		t.Fatalf("identity=%+v err=%v", identity, err)
	}
	if _, err := store.Authenticate(ctx, token+"wrong"); !errors.Is(err, gateway.ErrInvalidKey) {
		t.Fatalf("modified key error = %v", err)
	}
	if _, err := store.SetPrice(ctx, "test", 2_000_000, 5_000_000); err != nil {
		t.Fatal(err)
	}
	requestID := "test-request-" + time.Now().Format("20060102150405.000000000")
	start := gateway.UsageStart{RequestID: requestID, AccountID: accountID, KeyID: keyID, Model: "test"}
	if err := store.BeginUsage(ctx, start); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginUsage(ctx, start); err == nil {
		t.Fatal("duplicate request ID was accepted")
	}
	prompt, completion := int64(1000), int64(100)
	if err := store.FinishUsage(ctx, gateway.UsageFinish{RequestID: requestID, Outcome: "complete", Status: 200,
		PromptTokens: &prompt, CompletionTokens: &completion}); err != nil {
		t.Fatal(err)
	}
	if err := store.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := store.DailyUsage(ctx, accountID, time.Now().UTC().AddDate(0, 0, -1))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Requests != 1 || rows[0].Complete != 1 || rows[0].PromptTokens != 1000 || rows[0].CompletionTokens != 100 || rows[0].EstimatedCostMicro != 2500 || rows[0].Priced != 1 {
		t.Fatalf("daily usage = %+v", rows)
	}
	for i := range 24 {
		id := fmt.Sprintf("%s-%d", requestID, i)
		if err := store.BeginUsage(ctx, gateway.UsageStart{RequestID: id, AccountID: accountID, KeyID: keyID, Model: "test"}); err != nil {
			t.Fatal(err)
		}
		one := int64(1)
		if err := store.FinishUsage(ctx, gateway.UsageFinish{RequestID: id, Outcome: "complete", Status: 200,
			PromptTokens: &one, CompletionTokens: &one}); err != nil {
			t.Fatal(err)
		}
	}
	var workers sync.WaitGroup
	maintenanceErrors := make(chan error, 2)
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			maintenanceErrors <- store.Maintain(ctx)
		}()
	}
	workers.Wait()
	close(maintenanceErrors)
	for err := range maintenanceErrors {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err = store.DailyUsage(ctx, accountID, time.Now().UTC().AddDate(0, 0, -1))
	if err != nil || len(rows) != 1 || rows[0].Requests != 25 || rows[0].Complete != 25 {
		t.Fatalf("concurrent rollup = %+v, err=%v", rows, err)
	}
	staleID := requestID + "-stale"
	if err := store.BeginUsage(ctx, gateway.UsageStart{RequestID: staleID, AccountID: accountID, KeyID: keyID, Model: "test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `UPDATE usage_events SET started_at=now() - interval '8 days' WHERE request_id=$1`, staleID); err != nil {
		t.Fatal(err)
	}
	if err := store.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	var retained int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE request_id=$1`, staleID).Scan(&retained); err != nil || retained != 0 {
		t.Fatalf("stale raw row count=%d err=%v", retained, err)
	}
	rows, err = store.DailyUsage(ctx, accountID, time.Now().UTC().AddDate(0, 0, -9))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Incomplete != 1 {
		t.Fatalf("stale incomplete rollup = %+v", rows)
	}
	if err := store.RevokeKey(ctx, keyID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Authenticate(ctx, token); !errors.Is(err, gateway.ErrInvalidKey) {
		t.Fatalf("revoked key error = %v", err)
	}
}

func TestSignupCreatesAccountAndKeyAtomically(t *testing.T) {
	url := os.Getenv("VIRE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("VIRE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	name := "Signup test " + time.Now().Format("20060102150405.000000000")
	email := "signup-" + time.Now().Format("20060102150405.000000000") + "@example.com"
	result, err := store.Signup(ctx, email, name, "abcdefghij")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := store.Authenticate(ctx, result.APIKey)
	if err != nil || identity != (gateway.KeyIdentity{AccountID: result.AccountID, KeyID: result.KeyID}) {
		t.Fatalf("identity=%+v err=%v", identity, err)
	}
	if _, err := store.Signup(ctx, email, name, "abcdefghij"); !errors.Is(err, gateway.ErrEmailTaken) {
		t.Fatalf("duplicate signup error=%v", err)
	}
	account, err := store.Session(ctx, result.SessionToken)
	if err != nil || account.AccountID != result.AccountID || account.Email != email {
		t.Fatalf("signup session=%+v err=%v", account, err)
	}
	if _, err := store.Login(ctx, email, "incorrect password"); !errors.Is(err, gateway.ErrInvalidCredentials) {
		t.Fatalf("wrong password error=%v", err)
	}
	login, err := store.Login(ctx, email, "abcdefghij")
	if err != nil || login.Account.AccountID != result.AccountID {
		t.Fatalf("login=%+v err=%v", login, err)
	}
	if err := store.Logout(ctx, login.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Session(ctx, login.Token); !errors.Is(err, gateway.ErrInvalidSession) {
		t.Fatalf("revoked session error=%v", err)
	}
	if _, err := store.Usage(ctx, result.AccountID, time.Now().AddDate(0, 0, -29)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE name=$1`, name).Scan(&count); err != nil || count != 1 {
		t.Fatalf("account count=%d err=%v", count, err)
	}
}

func TestGatewayUsageAgainstPostgres(t *testing.T) {
	url := os.Getenv("VIRE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("VIRE_TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	signup, err := store.Signup(ctx, "gateway-"+time.Now().Format("20060102150405.000000000")+"@example.com", "Gateway test", "a sufficiently long password")
	if err != nil {
		t.Fatal(err)
	}
	accountID := signup.AccountID
	_, key, err := store.IssueKey(ctx, accountID, "integration")
	if err != nil {
		t.Fatal(err)
	}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("client API key reached inference backend")
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\n")
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, "data: {\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2},\"choices\":[]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
	}))
	defer backend.Close()
	registry, _ := json.Marshal([]gateway.Model{{Name: "test", URL: backend.URL}})
	registryPath := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(registryPath, registry, 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := gateway.NewServerWithOptions("127.0.0.1:0", registryPath, gateway.Options{AccountStore: store})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(server.Handler())
	defer front.Close()
	for _, body := range []string{`{"model":"test"}`, `{"model":"test","stream":true}`} {
		req, _ := http.NewRequest(http.MethodPost, front.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("completion status = %d", res.StatusCode)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var complete int
		if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM usage_events WHERE account_id=$1 AND state='complete'`, accountID).Scan(&complete); err != nil {
			t.Fatal(err)
		}
		if complete == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("complete usage rows = %d, want 2", complete)
		}
		time.Sleep(10 * time.Millisecond)
	}
	live, err := store.Usage(ctx, accountID, time.Now().UTC().AddDate(0, 0, -1))
	if err != nil || len(live) != 1 || live[0].Requests != 2 || live[0].PromptTokens != 8 || live[0].CompletionTokens != 3 {
		t.Fatalf("live portal usage = %+v, err=%v", live, err)
	}
	if err := store.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := store.DailyUsage(ctx, accountID, time.Now().UTC().AddDate(0, 0, -1))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Complete != 2 || rows[0].PromptTokens != 8 || rows[0].CompletionTokens != 3 {
		t.Fatalf("gateway daily usage = %+v", rows)
	}
	rolled, err := store.Usage(ctx, accountID, time.Now().UTC().AddDate(0, 0, -1))
	if err != nil || len(rolled) != 1 || rolled[0].Requests != 2 || rolled[0].PromptTokens != 8 || rolled[0].CompletionTokens != 3 {
		t.Fatalf("rolled portal usage = %+v, err=%v", rolled, err)
	}
}
