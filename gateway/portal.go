package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrEmailTaken         = errors.New("email already registered")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrLoginLimited       = errors.New("too many login attempts")
	ErrInvalidSession     = errors.New("invalid session")
)

const sessionCookie = "vire_session"

type SignupResult struct {
	AccountID    string `json:"account_id"`
	KeyID        string `json:"key_id"`
	APIKey       string `json:"api_key"`
	SessionToken string `json:"-"`
}

type PortalAccount struct {
	AccountID string `json:"account_id"`
	Name      string `json:"name"`
	Email     string `json:"email"`
}

type PortalSession struct {
	Token   string        `json:"-"`
	Account PortalAccount `json:"account"`
}

type PortalUsage struct {
	Day                time.Time `json:"day"`
	Model              string    `json:"model"`
	Requests           int64     `json:"requests"`
	Complete           int64     `json:"complete"`
	Incomplete         int64     `json:"incomplete"`
	Failed             int64     `json:"failed"`
	PromptTokens       int64     `json:"prompt_tokens"`
	CompletionTokens   int64     `json:"completion_tokens"`
	EstimatedCostMicro int64     `json:"estimated_cost_micro"`
	Priced             int64     `json:"priced"`
}

type PortalStore interface {
	Signup(context.Context, string, string, string) (SignupResult, error)
	Login(context.Context, string, string) (PortalSession, error)
	Session(context.Context, string) (PortalAccount, error)
	Logout(context.Context, string) error
	Usage(context.Context, string, time.Time) ([]PortalUsage, error)
	ChatKeyID(context.Context, string) (string, error)
}

func validSignup(email, name string) bool {
	if len(email) == 0 || len(email) > 254 || len(name) == 0 || len(name) > 100 {
		return false
	}
	address, err := mail.ParseAddress(email)
	return err == nil && address.Address == email && !strings.ContainsAny(name, "\r\n")
}

func validPassword(password string) bool {
	return utf8.ValidString(password) && utf8.RuneCountInString(password) >= 10 && len(password) <= 256
}

func (s *Server) browserPost(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Content-Type") != "application/json" {
		writeAPIError(w, http.StatusUnsupportedMediaType, "JSON is required")
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || (u.Host != r.Host && origin != s.portalOrigin) || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
			writeAPIError(w, http.StatusForbidden, "cross-origin request denied")
			return false
		}
		if r.TLS != nil && u.Scheme != "https" {
			writeAPIError(w, http.StatusForbidden, "cross-origin request denied")
			return false
		}
	}
	return true
}

func readPortalJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid request")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "invalid request")
		return false
	}
	return true
}

func (s *Server) setSession(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/api", MaxAge: 7 * 24 * 60 * 60, HttpOnly: true, Secure: !s.insecureCookies, SameSite: http.SameSiteStrictMode})
}

func (s *Server) signupHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.portalStore == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "account signup is unavailable")
		return
	}
	if !s.browserPost(w, r) {
		return
	}
	var input struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
	}
	if !readPortalJSON(w, r, &input) {
		return
	}
	input.Email = strings.ToLower(strings.TrimSpace(input.Email))
	input.Name = strings.TrimSpace(input.Name)
	if !validSignup(input.Email, input.Name) || !validPassword(input.Password) {
		writeAPIError(w, http.StatusBadRequest, "valid email, account name, and password of at least 10 characters are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := s.portalStore.Signup(ctx, input.Email, input.Name, input.Password)
	if errors.Is(err, ErrEmailTaken) {
		writeAPIError(w, http.StatusConflict, "email is already registered")
		return
	}
	if err != nil {
		s.observability.logger.Error("account signup failed", "error", err)
		writeAPIError(w, http.StatusServiceUnavailable, "account signup is unavailable")
		return
	}
	s.setSession(w, result.SessionToken)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(result)
}

func (s *Server) loginHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.portalStore == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "account login is unavailable")
		return
	}
	if !s.browserPost(w, r) {
		return
	}
	var input struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if !readPortalJSON(w, r, &input) {
		return
	}
	input.Email = strings.ToLower(strings.TrimSpace(input.Email))
	if len(input.Email) > 254 || len(input.Password) > 256 || input.Email == "" || input.Password == "" {
		writeAPIError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	result, err := s.portalStore.Login(ctx, input.Email, input.Password)
	if errors.Is(err, ErrLoginLimited) {
		w.Header().Set("Retry-After", "900")
		writeAPIError(w, http.StatusTooManyRequests, "too many attempts; try again later")
		return
	}
	if errors.Is(err, ErrInvalidCredentials) {
		writeAPIError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	if err != nil {
		s.observability.logger.Error("account login failed", "error", err)
		writeAPIError(w, http.StatusServiceUnavailable, "account login is unavailable")
		return
	}
	s.setSession(w, result.Token)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (s *Server) sessionAccount(w http.ResponseWriter, r *http.Request) (PortalAccount, string, bool) {
	if s.portalStore == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "account portal is unavailable")
		return PortalAccount{}, "", false
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		writeAPIError(w, http.StatusUnauthorized, "sign in required")
		return PortalAccount{}, "", false
	}
	ctx, cancel := context.WithTimeout(r.Context(), accountQueryTimeout)
	defer cancel()
	account, err := s.portalStore.Session(ctx, cookie.Value)
	if errors.Is(err, ErrInvalidSession) {
		writeAPIError(w, http.StatusUnauthorized, "sign in required")
		return PortalAccount{}, "", false
	}
	if err != nil {
		s.observability.logger.Error("session lookup failed", "error", err)
		writeAPIError(w, http.StatusServiceUnavailable, "account portal is unavailable")
		return PortalAccount{}, "", false
	}
	return account, cookie.Value, true
}

func (s *Server) accountHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	account, _, ok := s.sessionAccount(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(account)
}

func (s *Server) usageHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	account, _, ok := s.sessionAccount(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), accountQueryTimeout)
	defer cancel()
	rows, err := s.portalStore.Usage(ctx, account.AccountID, time.Now().UTC().AddDate(0, 0, -29))
	if err != nil {
		s.observability.logger.Error("usage lookup failed", "error", err)
		writeAPIError(w, http.StatusServiceUnavailable, "usage is unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Usage []PortalUsage `json:"usage"`
	}{rows})
}

func (s *Server) logoutHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !s.browserPost(w, r) {
		return
	}
	_, token, ok := s.sessionAccount(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), accountQueryTimeout)
	defer cancel()
	if err := s.portalStore.Logout(ctx, token); err != nil {
		writeAPIError(w, http.StatusServiceUnavailable, "logout is unavailable")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/api", MaxAge: -1, HttpOnly: true, Secure: !s.insecureCookies, SameSite: http.SameSiteStrictMode})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) portalModelsHandler(w http.ResponseWriter, _ *http.Request) {
	type publicPricing struct {
		InputUSDPerMillion  float64 `json:"input_usd_per_million"`
		OutputUSDPerMillion float64 `json:"output_usd_per_million"`
		Example             bool    `json:"example"`
	}
	type publicModel struct {
		ID          string         `json:"id"`
		DisplayName string         `json:"display_name"`
		Pricing     *publicPricing `json:"pricing"`
	}
	byName := make(map[string]publicModel, len(s.catalog))
	for _, model := range s.models {
		if _, exists := byName[model.Name]; exists {
			continue
		}
		item := publicModel{ID: model.Name, DisplayName: model.DisplayName}
		if item.DisplayName == "" {
			item.DisplayName = model.Name
		}
		if model.Pricing != nil {
			item.Pricing = &publicPricing{
				InputUSDPerMillion:  float64(model.Pricing.InputRateMicroPerMillion) / 1_000_000,
				OutputUSDPerMillion: float64(model.Pricing.OutputRateMicroPerMillion) / 1_000_000,
				Example:             model.Pricing.Example,
			}
		}
		byName[model.Name] = item
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	slices.Sort(names)
	models := make([]publicModel, 0, len(names))
	for _, name := range names {
		models = append(models, byName[name])
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Models []publicModel `json:"models"`
	}{Models: models})
}
