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
	"slices"
	"strings"
	"time"
)

const shutdownTimeout = 5 * time.Minute

type Model struct {
	Name        string        `json:"name"`
	URL         string        `json:"url"`
	DisplayName string        `json:"display_name,omitempty"`
	Pricing     *ModelPricing `json:"pricing,omitempty"`
	Aliases     []string      `json:"aliases,omitempty"`
}

// Rates are micro-USD per million tokens, matching the usage ledger.
type ModelPricing struct {
	InputRateMicroPerMillion  int64 `json:"input_rate_micro_per_million"`
	OutputRateMicroPerMillion int64 `json:"output_rate_micro_per_million"`
	Example                   bool  `json:"example,omitempty"`
}

type Server struct {
	httpServer      *http.Server
	models          []Model
	proxies         map[string]*modelRoute
	aliases         map[string]string
	transport       *http.Transport
	metricsServer   *http.Server
	observability   *observability
	accountStore    AccountStore
	portalStore     PortalStore
	insecureCookies bool
	portalOrigin    string
	inflightLimit   chan struct{}
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
		models:          models,
		proxies:         make(map[string]*modelRoute, len(models)),
		aliases:         make(map[string]string),
		transport:       transport,
		observability:   telemetry,
		accountStore:    options.AccountStore,
		portalStore:     options.PortalStore,
		insecureCookies: options.InsecureCookies,
		portalOrigin:    options.PortalOrigin,
	}
	if options.AccountStore != nil {
		limit := options.MaxInflight
		if limit <= 0 {
			limit = 32
		}
		server.inflightLimit = make(chan struct{}, limit)
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
		for _, alias := range model.Aliases {
			server.aliases[alias] = model.Name
		}
		// loadModels has already validated the URL.
		target, _ := url.Parse(model.URL)
		route := server.proxies[model.Name]
		if route == nil {
			route = &modelRoute{model: model.Name, observability: telemetry}
			server.proxies[model.Name] = route
		}
		origin := strings.TrimSuffix(target.String(), "/")
		route.backends = append(route.backends, &routeBackend{
			proxy:  newProxy(target, observedTransport{base: transport, o: telemetry}, options.AccountStore != nil, options.BackendAPIKey),
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
	if options.AccountStore == nil {
		mux.HandleFunc("GET /models", server.modelsHandler)
	} else {
		mux.HandleFunc("GET /v1/models", server.publicModelsHandler)
	}
	mux.HandleFunc("POST /v1/chat/completions", server.handleCompletions)
	mux.HandleFunc("GET /api/models", server.portalModelsHandler)
	mux.HandleFunc("POST /api/signup", server.signupHandler)
	mux.HandleFunc("POST /api/login", server.loginHandler)
	mux.HandleFunc("POST /api/logout", server.logoutHandler)
	mux.HandleFunc("GET /api/account", server.accountHandler)
	mux.HandleFunc("GET /api/usage", server.usageHandler)
	if options.WebDir != "" {
		mux.HandleFunc("GET /account", func(w http.ResponseWriter, r *http.Request) { http.ServeFile(w, r, options.WebDir+"/index.html") })
		mux.Handle("GET /", http.FileServer(http.Dir(options.WebDir)))
	}

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

	type destination struct{ name, url string }
	destinations := make(map[destination]bool, len(models))
	metadata := make(map[string]Model, len(models))
	for i, model := range models {
		if strings.TrimSpace(model.Name) == "" {
			return nil, fmt.Errorf("model %d: name is required", i)
		}
		if model.DisplayName != "" && strings.TrimSpace(model.DisplayName) == "" {
			return nil, fmt.Errorf("model %d: display_name must not be blank", i)
		}
		if model.Pricing != nil && (model.Pricing.InputRateMicroPerMillion < 0 || model.Pricing.OutputRateMicroPerMillion < 0) {
			return nil, fmt.Errorf("model %d: pricing rates must be nonnegative", i)
		}
		if previous, ok := metadata[model.Name]; ok && (previous.DisplayName != model.DisplayName || !samePricing(previous.Pricing, model.Pricing) || !slices.Equal(previous.Aliases, model.Aliases)) {
			return nil, fmt.Errorf("model %d: replicas of %q must share display_name, pricing, and aliases", i, model.Name)
		}
		metadata[model.Name] = model
		target, err := url.Parse(model.URL)
		if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Hostname() == "" {
			return nil, fmt.Errorf("model %d: url must be an absolute HTTP(S) URL", i)
		}
		if target.User != nil || (target.Path != "" && target.Path != "/") || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || strings.Contains(model.URL, "#") {
			return nil, fmt.Errorf("model %d: url must contain only an origin, without credentials, query, or fragment", i)
		}
		// Treat an origin with a trailing slash as the same destination.
		key := destination{model.Name, strings.TrimSuffix(target.String(), "/")}
		if destinations[key] {
			return nil, fmt.Errorf("model %d: duplicate destination for %q", i, model.Name)
		}
		destinations[key] = true
	}
	aliasOwners := make(map[string]string)
	for name, model := range metadata {
		for _, alias := range model.Aliases {
			if strings.TrimSpace(alias) == "" || alias != strings.TrimSpace(alias) || alias == name {
				return nil, fmt.Errorf("model %q: aliases must be nonempty, trimmed, and different from the model name", name)
			}
			if _, exists := metadata[alias]; exists {
				return nil, fmt.Errorf("model %q: alias %q conflicts with a model name", name, alias)
			}
			if owner, exists := aliasOwners[alias]; exists {
				return nil, fmt.Errorf("model %q: alias %q is already used by %q", name, alias, owner)
			}
			aliasOwners[alias] = name
		}
	}

	return models, nil
}

func samePricing(a, b *ModelPricing) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
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

// PricedModels returns one configured rate per model, regardless of replica count.
func (s *Server) PricedModels() []Model {
	seen := make(map[string]bool, len(s.proxies))
	var priced []Model
	for _, model := range s.models {
		if model.Pricing != nil && !seen[model.Name] {
			priced = append(priced, model)
			seen[model.Name] = true
		}
	}
	return priced
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
