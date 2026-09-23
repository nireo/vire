package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	registryPollInterval = 2 * time.Second
	backendPollInterval  = 5 * time.Second
	// Bump this when the ring or key construction changes.
	routingVersion = "consistent-xxhash-4093-20-1.25-v1"
)

// A routing snapshot is published in one atomic operation. Existing requests
// retain their selected proxy while new requests see the next membership set.
type routingSnapshot struct {
	models      []Model
	proxies     map[string]*modelRoute
	fingerprint string
}

func modelCatalog(models []Model) map[string]Model {
	catalog := make(map[string]Model)
	for _, model := range models {
		catalog[model.Name] = model
	}
	return catalog
}

func (s *Server) sameCatalog(models []Model) bool {
	next := modelCatalog(models)
	if len(next) != len(s.catalog) {
		return false
	}
	for name, previous := range s.catalog {
		model, ok := next[name]
		if !ok || previous.DisplayName != model.DisplayName ||
			!samePricing(previous.Pricing, model.Pricing) {
			return false
		}
	}
	return true
}

func membershipFingerprint(models []Model) string {
	members := make([]string, len(models))
	for i, model := range models {
		target, _ := url.Parse(model.URL)
		members[i] = model.Name + "\x00" + strings.TrimSuffix(target.String(), "/")
	}
	slices.Sort(members)
	digest := sha256.Sum256([]byte(routingVersion + "\n" + strings.Join(members, "\n")))
	return hex.EncodeToString(digest[:8])
}

// buildRouting is called at construction or with reloadMu held. It reuses
// backend health and connection pools when a member remains in the registry.
func (s *Server) buildRouting(models []Model) (*routingSnapshot, []*routeBackend) {
	snapshot := &routingSnapshot{
		models: models, proxies: make(map[string]*modelRoute),
		fingerprint: membershipFingerprint(models),
	}
	var added []*routeBackend
	for _, model := range models {
		target, _ := url.Parse(model.URL) // loadModels validated it.
		origin := strings.TrimSuffix(target.String(), "/")
		key := model.Name + "\x00" + origin
		backend := s.backends[key]
		if backend == nil {
			backend = &routeBackend{
				proxy: newProxy(target, observedTransport{base: s.transport, o: s.observability},
					s.accountStore != nil, s.backendAPIKey),
				origin: origin, model: model.Name,
			}
			backend.healthy.Store(true) // The running server probes before accepting traffic.
			s.backends[key] = backend
			added = append(added, backend)
			s.observability.inflight.WithLabelValues(model.Name, origin).Set(0)
			s.observability.headers.WithLabelValues(model.Name, origin)
			for _, mode := range []string{"single", "affinity", "round_robin", "single_failover", "affinity_failover", "round_robin_failover"} {
				s.observability.routed.WithLabelValues(model.Name, origin, mode)
			}
		}
		route := snapshot.proxies[model.Name]
		if route == nil {
			route = &modelRoute{model: model.Name, observability: s.observability}
			snapshot.proxies[model.Name] = route
		}
		route.backends = append(route.backends, backend)
	}
	for _, route := range snapshot.proxies {
		route.buildRing()
	}
	return snapshot, added
}

// ReloadRegistry applies membership changes for the existing model catalog.
// A new model or metadata change still requires a gateway restart so pricing
// and public model metadata remain consistent with the account database.
func (s *Server) ReloadRegistry(ctx context.Context) error {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	models, err := loadModels(s.registryPath)
	if err != nil {
		s.observability.registryReloadErrors.Inc()
		return err
	}
	if !s.sameCatalog(models) {
		s.observability.registryReloadErrors.Inc()
		return fmt.Errorf("model names and metadata must remain unchanged during membership reload")
	}
	old := s.routing.Load()
	if fingerprint := membershipFingerprint(models); fingerprint == old.fingerprint {
		return nil
	}
	next, added := s.buildRouting(models)
	for _, backend := range added {
		if !s.probeBackend(ctx, backend) {
			for _, candidate := range added {
				delete(s.backends, candidate.model+"\x00"+candidate.origin)
				s.observability.backendHealthy.DeleteLabelValues(candidate.model, candidate.origin)
			}
			s.observability.registryReloadErrors.Inc()
			return fmt.Errorf("new backend %s is not healthy", backend.origin)
		}
	}
	s.routing.Store(next)
	s.observability.registryInfo.DeleteLabelValues(old.fingerprint)
	s.observability.registryInfo.WithLabelValues(next.fingerprint).Set(1)
	active := make(map[string]bool, len(models))
	for _, model := range models {
		active[model.Name+"\x00"+strings.TrimSuffix(model.URL, "/")] = true
	}
	for key, backend := range s.backends {
		if !active[key] {
			delete(s.backends, key)
			s.observability.backendHealthy.DeleteLabelValues(backend.model, backend.origin)
		}
	}
	s.observability.logger.Info("model membership updated", "fingerprint", next.fingerprint)
	return nil
}

func (s *Server) watchRegistry(ctx context.Context) {
	ticker := time.NewTicker(registryPollInterval)
	defer ticker.Stop()
	lastError := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.ReloadRegistry(ctx); err != nil && ctx.Err() == nil {
				if err.Error() != lastError {
					s.observability.logger.Warn("model registry reload rejected", "error", err)
				}
				lastError = err.Error()
			} else {
				lastError = ""
			}
		}
	}
}

func (s *Server) probeBackend(ctx context.Context, backend *routeBackend) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, backend.origin+"/health", nil)
	if err != nil {
		return false
	}
	if s.backendAPIKey != "" {
		request.Header.Set("Authorization", "Bearer "+s.backendAPIKey)
	}
	response, err := s.healthClient.Do(request)
	if err == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	if ctx.Err() != nil {
		return false
	}
	healthy := err == nil && response.StatusCode == http.StatusOK
	previous := backend.healthy.Swap(healthy)
	value := 0.0
	if healthy {
		value = 1
	}
	s.observability.backendHealthy.WithLabelValues(backend.model, backend.origin).Set(value)
	if previous != healthy {
		s.observability.logger.Warn("backend health changed", "model", backend.model,
			"backend", backend.origin, "healthy", healthy)
	}
	return healthy
}

func (s *Server) probeBackends(ctx context.Context) {
	s.reloadMu.Lock()
	defer s.reloadMu.Unlock()
	snapshot := s.routing.Load()
	var group sync.WaitGroup
	slots := make(chan struct{}, 8)
	for _, route := range snapshot.proxies {
		for _, backend := range route.backends {
			group.Add(1)
			go func() {
				defer group.Done()
				select {
				case slots <- struct{}{}:
					defer func() { <-slots }()
					s.probeBackend(ctx, backend)
				case <-ctx.Done():
				}
			}()
		}
	}
	group.Wait()
}

func (s *Server) watchBackends(ctx context.Context) {
	ticker := time.NewTicker(backendPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.probeBackends(ctx)
		}
	}
}
