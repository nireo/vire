package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const shutdownTimeout = 5 * time.Second

type Model struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type Server struct {
	httpServer *http.Server
	models     []Model
}

func NewServer(addr, registryPath string) (*Server, error) {
	models, err := loadModels(registryPath)
	if err != nil {
		return nil, fmt.Errorf("load model registry: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler)

	server := &Server{
		httpServer: &http.Server{
			Addr:    addr,
			Handler: mux,
		},
		models: models,
	}
	mux.HandleFunc("GET /models", server.modelsHandler)

	return server, nil
}

func loadModels(path string) ([]Model, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var models []Model
	if err := json.Unmarshal(data, &models); err != nil {
		return nil, err
	}
	if models == nil {
		return nil, errors.New("registry must be a JSON array")
	}

	for i, model := range models {
		if strings.TrimSpace(model.Name) == "" {
			return nil, fmt.Errorf("model %d: name is required", i)
		}
		if strings.TrimSpace(model.URL) == "" {
			return nil, fmt.Errorf("model %d: url is required", i)
		}
	}

	return models, nil
}

func (s *Server) Run(ctx context.Context) error {
	errs := make(chan error, 1)
	go func() {
		errs <- s.httpServer.ListenAndServe()
	}()

	select {
	case err := <-errs:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			_ = s.httpServer.Close()
			<-errs
			return fmt.Errorf("shutdown: %w", err)
		}

		if err := <-errs; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	}
}

func (s *Server) Handler() http.Handler {
	return s.httpServer.Handler
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) modelsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(s.models)
}
