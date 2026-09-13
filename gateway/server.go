package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
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
	proxies    map[string]*httputil.ReverseProxy
	transport  *http.Transport
}

func NewServer(addr, registryPath string) (*Server, error) {
	models, err := loadModels(registryPath)
	if err != nil {
		return nil, fmt.Errorf("load model registry: %w", err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 2 * time.Minute
	// Pass compressed responses through rather than decompressing them here.
	transport.DisableCompression = true

	mux := http.NewServeMux()
	server := &Server{
		httpServer: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			IdleTimeout:       60 * time.Second,
			// No WriteTimeout: generation streams can legitimately be long-lived.
		},
		models:    models,
		proxies:   make(map[string]*httputil.ReverseProxy, len(models)),
		transport: transport,
	}
	for _, model := range models {
		// loadModels has already validated the URL.
		target, _ := url.Parse(model.URL)
		server.proxies[model.Name] = newProxy(target, transport)
	}

	mux.HandleFunc("GET /health", healthHandler)
	mux.HandleFunc("GET /models", server.modelsHandler)
	mux.HandleFunc("POST /v1/chat/completions", server.handleCompletions)

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

	names := make(map[string]bool, len(models))
	for i, model := range models {
		if strings.TrimSpace(model.Name) == "" {
			return nil, fmt.Errorf("model %d: name is required", i)
		}
		if names[model.Name] {
			return nil, fmt.Errorf("model %d: duplicate name %q", i, model.Name)
		}
		names[model.Name] = true
		target, err := url.Parse(model.URL)
		if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" {
			return nil, fmt.Errorf("model %d: url must be an absolute HTTP(S) URL", i)
		}
		if target.User != nil || (target.Path != "" && target.Path != "/") || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || strings.Contains(model.URL, "#") {
			return nil, fmt.Errorf("model %d: url must contain only an origin, without credentials, query, or fragment", i)
		}
	}

	return models, nil
}

func (s *Server) Run(ctx context.Context) error {
	defer s.transport.CloseIdleConnections()

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
