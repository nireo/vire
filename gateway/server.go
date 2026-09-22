package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const shutdownTimeout = 5 * time.Minute

type Model struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type Server struct {
	httpServer    *http.Server
	models        []Model
	proxies       map[string]*modelRoute
	transport     *http.Transport
	metricsServer *http.Server
	observability *observability
}

func NewServer(addr, registryPath string) (*Server, error) {
	return NewServerWithOptions(addr, registryPath, Options{})
}

func NewServerWithOptions(addr, registryPath string, options Options) (*Server, error) {
	models, err := loadModels(registryPath)
	if err != nil {
		return nil, fmt.Errorf("load model registry: %w", err)
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 2 * time.Minute
	// Pass compressed responses through rather than decompressing them here.
	transport.DisableCompression = true

	logger := options.Logger
	if logger == nil {
		logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	telemetry := newObservability(logger)
	mux := http.NewServeMux()
	server := &Server{
		httpServer: &http.Server{
			Addr:              addr,
			Handler:           telemetry.wrap(mux),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			IdleTimeout:       60 * time.Second,
			// No WriteTimeout: generation streams can legitimately be long-lived.
		},
		models:        models,
		proxies:       make(map[string]*modelRoute, len(models)),
		transport:     transport,
		observability: telemetry,
	}
	if options.MetricsAddr != "" {
		server.metricsServer = &http.Server{
			Addr:              options.MetricsAddr,
			Handler:           server.MetricsHandler(),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       10 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       30 * time.Second,
		}
	}
	for _, model := range models {
		// loadModels has already validated the URL.
		target, _ := url.Parse(model.URL)
		route := server.proxies[model.Name]
		if route == nil {
			route = &modelRoute{model: model.Name, observability: telemetry}
			server.proxies[model.Name] = route
		}
		origin := strings.TrimSuffix(target.String(), "/")
		route.backends = append(route.backends, &routeBackend{
			proxy:  newProxy(target, observedTransport{base: transport, o: telemetry}),
			origin: origin,
		})
		telemetry.inflight.WithLabelValues(model.Name, origin).Set(0)
		telemetry.headers.WithLabelValues(model.Name, origin)
		for _, mode := range []string{"single", "affinity", "round_robin"} {
			telemetry.routed.WithLabelValues(model.Name, origin, mode)
		}
	}
	for _, route := range server.proxies {
		route.buildRing()
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

	destinations := make(map[Model]bool, len(models))
	for i, model := range models {
		if strings.TrimSpace(model.Name) == "" {
			return nil, fmt.Errorf("model %d: name is required", i)
		}
		target, err := url.Parse(model.URL)
		if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" {
			return nil, fmt.Errorf("model %d: url must be an absolute HTTP(S) URL", i)
		}
		if target.User != nil || (target.Path != "" && target.Path != "/") || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || strings.Contains(model.URL, "#") {
			return nil, fmt.Errorf("model %d: url must contain only an origin, without credentials, query, or fragment", i)
		}
		// Treat an origin with a trailing slash as the same destination.
		key := Model{Name: model.Name, URL: strings.TrimSuffix(target.String(), "/")}
		if destinations[key] {
			return nil, fmt.Errorf("model %d: duplicate destination for %q", i, model.Name)
		}
		destinations[key] = true
	}

	return models, nil
}

func (s *Server) Run(ctx context.Context) error {
	defer s.transport.CloseIdleConnections()
	if ctx.Err() != nil {
		return nil
	}
	// Bind both sockets before serving either endpoint. A configuration error
	// must not leave a partially running gateway behind.
	api, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("listen API: %w", err)
	}
	defer api.Close()
	var metrics net.Listener
	if s.metricsServer != nil {
		metrics, err = net.Listen("tcp", s.metricsServer.Addr)
		if err != nil {
			return fmt.Errorf("listen metrics: %w", err)
		}
		defer metrics.Close()
	}
	closeServers := func() {
		_ = s.httpServer.Close()
		if s.metricsServer != nil {
			_ = s.metricsServer.Close()
		}
	}
	defer closeServers()

	errs := make(chan error, 2)
	start := func(name string, server *http.Server, listener net.Listener) {
		s.observability.logger.Info("listener started", "endpoint", name, "addr", listener.Addr().String())
		go func() { errs <- fmt.Errorf("serve %s: %w", name, server.Serve(listener)) }()
	}
	start("api", s.httpServer, api)
	remaining := 1
	if metrics != nil {
		start("metrics", s.metricsServer, metrics)
		remaining++
	}

	var runErr error
	select {
	case err := <-errs:
		remaining--
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = err
		}
		closeServers() // An exited listener must not leave its sibling running.
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		// Keep metrics available while active API requests drain.
		if err := s.httpServer.Shutdown(shutdownCtx); err != nil {
			runErr = fmt.Errorf("shutdown API: %w", err)
			_ = s.httpServer.Close()
		}
		if s.metricsServer != nil {
			if err := s.metricsServer.Shutdown(shutdownCtx); err != nil && runErr == nil {
				runErr = fmt.Errorf("shutdown metrics: %w", err)
			}
		}
		closeServers()
	}
	for ; remaining > 0; remaining-- {
		if err := <-errs; !errors.Is(err, http.ErrServerClosed) && runErr == nil {
			runErr = err
		}
	}
	s.observability.logger.Info("gateway stopped")
	return runErr
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
