package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"slices"
	"strings"
)

var ErrEmailTaken = errors.New("email already registered")

type SignupResult struct {
	AccountID string `json:"account_id"`
	KeyID     string `json:"key_id"`
	APIKey    string `json:"api_key"`
}

type SignupStore interface {
	Signup(context.Context, string, string) (SignupResult, error)
}

func validSignup(email, name string) bool {
	if len(email) == 0 || len(email) > 254 || len(name) == 0 || len(name) > 100 {
		return false
	}
	address, err := mail.ParseAddress(email)
	return err == nil && address.Address == email && !strings.ContainsAny(name, "\r\n")
}

func (s *Server) signupHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.signupStore == nil {
		writeAPIError(w, http.StatusServiceUnavailable, "account signup is unavailable")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	if err := decoder.Decode(&input); err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid signup request")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeAPIError(w, http.StatusBadRequest, "invalid signup request")
		return
	}
	input.Email = strings.ToLower(strings.TrimSpace(input.Email))
	input.Name = strings.TrimSpace(input.Name)
	if !validSignup(input.Email, input.Name) {
		writeAPIError(w, http.StatusBadRequest, "a valid email and account name are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), accountQueryTimeout)
	defer cancel()
	result, err := s.signupStore.Signup(ctx, input.Email, input.Name)
	if errors.Is(err, ErrEmailTaken) {
		writeAPIError(w, http.StatusConflict, "email is already registered")
		return
	}
	if err != nil {
		s.observability.logger.Error("account signup failed", "error", err)
		writeAPIError(w, http.StatusServiceUnavailable, "account signup is unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(result)
}

func (s *Server) portalModelsHandler(w http.ResponseWriter, _ *http.Request) {
	names := make([]string, 0, len(s.proxies))
	for name := range s.proxies {
		names = append(names, name)
	}
	slices.Sort(names)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Models []string `json:"models"`
	}{Models: names})
}
