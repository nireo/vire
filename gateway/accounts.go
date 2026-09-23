package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"
)

// ErrInvalidKey is returned when a bearer key is absent, unknown, or revoked.
var ErrInvalidKey = errors.New("invalid API key")

type KeyIdentity struct {
	AccountID string
	KeyID     string
}

type UsageStart struct {
	RequestID string
	AccountID string
	KeyID     string
	Model     string
}

type UsageFinish struct {
	RequestID        string
	Backend          string
	Status           int
	Outcome          string // complete, incomplete, or failed
	PromptTokens     *int64
	CompletionTokens *int64
}

// AccountStore persists one usage row per request. A request must be recorded
// before it is sent upstream, so a process crash leaves a visible pending row.
type AccountStore interface {
	Authenticate(context.Context, string) (KeyIdentity, error)
	BeginUsage(context.Context, UsageStart) error
	FinishUsage(context.Context, UsageFinish) error
}

const accountQueryTimeout = 3 * time.Second

type accountContextKey struct{}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (KeyIdentity, bool) {
	if s.accountStore == nil {
		return KeyIdentity{}, true
	}
	values := r.Header.Values("Authorization")
	if len(values) != 1 || len(values[0]) > 256 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeAPIError(w, http.StatusUnauthorized, "a valid API key is required")
		return KeyIdentity{}, false
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeAPIError(w, http.StatusUnauthorized, "a valid API key is required")
		return KeyIdentity{}, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), accountQueryTimeout)
	defer cancel()
	identity, err := s.accountStore.Authenticate(ctx, parts[1])
	if errors.Is(err, ErrInvalidKey) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeAPIError(w, http.StatusUnauthorized, "a valid API key is required")
		return KeyIdentity{}, false
	}
	if err != nil {
		s.observability.meteringErrors.WithLabelValues("authenticate").Inc()
		s.observability.logger.Error("authentication store unavailable", "error", err)
		writeAPIError(w, http.StatusServiceUnavailable, "account store unavailable")
		return KeyIdentity{}, false
	}
	return identity, true
}

func (s *Server) publicModelsHandler(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authenticate(w, r); !ok {
		return
	}
	names := make([]string, 0, len(s.catalog))
	for name := range s.catalog {
		names = append(names, name)
	}
	slices.Sort(names)
	type model struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	data := make([]model, 0, len(names))
	for _, name := range names {
		data = append(data, model{ID: name, Object: "model"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(struct {
		Object string  `json:"object"`
		Data   []model `json:"data"`
	}{Object: "list", Data: data})
}
