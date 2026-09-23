package gateway

import (
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Options configures telemetry and optional account metering. An empty
// MetricsAddr disables the extra listener; MetricsHandler remains available for
// embedding. Logger defaults to JSON on stderr.
type Options struct {
	MetricsAddr     string
	Logger          *slog.Logger
	AccountStore    AccountStore
	PortalStore     PortalStore
	InsecureCookies bool
	PortalOrigin    string
	WebDir          string
	BackendAPIKey   string
	MaxInflight     int
}

type observability struct {
	registry       *prometheus.Registry
	logger         *slog.Logger
	requests       *prometheus.CounterVec
	duration       *prometheus.HistogramVec
	headers        *prometheus.HistogramVec
	inflight       *prometheus.GaugeVec
	routed         *prometheus.CounterVec
	meteringErrors *prometheus.CounterVec
	metered        *prometheus.CounterVec
}

func newObservability(logger *slog.Logger) *observability {
	o := &observability{
		registry: prometheus.NewRegistry(),
		logger:   logger,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vire_http_requests_total",
			Help: "Finished HTTP requests by committed status (0 if none) and transport outcome, not generation success.",
		}, []string{"route", "model", "backend", "status", "outcome"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "vire_http_request_duration_seconds",
			Help:    "Whole request lifetime, including the entire response stream.",
			Buckets: []float64{0.005, 0.025, 0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600},
		}, []string{"route", "model", "backend"}),
		headers: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "vire_upstream_header_duration_seconds",
			Help:    "Dispatch to upstream response headers, including connection acquisition; successful RoundTrips only, not time to first token.",
			Buckets: []float64{0.005, 0.025, 0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"model", "backend"}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vire_backend_requests_in_flight",
			Help: "Active proxied requests, including response streaming.",
		}, []string{"model", "backend"}),
		routed: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vire_routed_requests_total",
			Help: "Backend selections by routing mode; does not imply successful completion.",
		}, []string{"model", "backend", "routing"}),
		meteringErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vire_metering_errors_total",
			Help: "Failures in the durable usage recording path by stage.",
		}, []string{"stage"}),
		metered: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vire_metered_requests_total",
			Help: "Durably recorded inference requests by usage outcome.",
		}, []string{"outcome"}),
	}
	o.registry.MustRegister(o.requests, o.duration, o.headers, o.inflight, o.routed, o.meteringErrors, o.metered,
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return o
}

// Request state belongs to the serving goroutine. The transport's synchronous
// RoundTrip and the proxy error handler use it; no httptrace callbacks mutate it.
// Only the writer needs a mutex: ReverseProxy may flush from a timer goroutine.
type requestObservation struct {
	id, model, backend, routing, outcome string
	usage                                *usageCapture
}

type observationKey struct{}

func observation(r *http.Request) *requestObservation {
	o, _ := r.Context().Value(observationKey{}).(*requestObservation)
	return o
}

func (o *observability) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		state := &requestObservation{id: rand.Text(), routing: "none"}
		r = r.WithContext(context.WithValue(r.Context(), observationKey{}, state))
		writer := &observedWriter{ResponseWriter: w, requestID: state.id}
		writer.Header().Set("X-Request-ID", state.id)
		defer func() {
			panicked := recover()
			status, size, writeFailed := writer.snapshot()
			outcome := state.outcome
			switch {
			case r.Context().Err() != nil:
				outcome = "canceled"
			case panicked == http.ErrAbortHandler:
				outcome = "stream_aborted"
			case panicked != nil:
				outcome = "panic"
			case writeFailed:
				outcome = "write_error"
			case outcome == "":
				outcome = "completed"
				if status >= 400 {
					outcome = "http_error"
				}
			}
			// net/http supplies an implicit 200 for an ordinary empty response,
			// but do not invent one for cancellation or an aborted handler.
			if status == 0 && outcome == "completed" {
				status = http.StatusOK
			}
			route := "unmatched"
			switch r.Pattern {
			case "GET /health", "GET /models", "POST /v1/chat/completions":
				route = r.Pattern
			}
			duration := time.Since(started).Seconds()
			o.requests.WithLabelValues(route, state.model, state.backend, strconv.Itoa(status), outcome).Inc()
			o.duration.WithLabelValues(route, state.model, state.backend).Observe(duration)
			level := slog.LevelInfo
			if outcome != "completed" {
				level = slog.LevelWarn
			}
			o.logger.LogAttrs(r.Context(), level, "request completed",
				slog.String("request_id", state.id), slog.String("route", route),
				slog.String("model", state.model), slog.String("backend", state.backend),
				slog.String("routing", state.routing), slog.Int("status", status),
				slog.String("outcome", outcome), slog.Float64("duration_seconds", duration),
				slog.Int64("response_bytes", size))
			if panicked != nil {
				panic(panicked) // In particular, preserve net/http's stream-abort semantics.
			}
		}()
		next.ServeHTTP(writer, r)
	})
}

// observedWriter keeps the response streaming: it neither buffers nor parses
// response bytes. Unwrap preserves ResponseController's optional capabilities.
// FlushError records headers committed by a flush even before the first Write.
type observedWriter struct {
	http.ResponseWriter
	mu          sync.Mutex
	requestID   string
	status      int
	bytes       int64
	writeFailed bool
}

func (w *observedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *observedWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status != 0 {
		return
	}

	w.Header().Set("X-Request-ID", w.requestID)
	w.ResponseWriter.WriteHeader(status)
	if status >= 200 || status == http.StatusSwitchingProtocols {
		w.status = status
	}
}

func (w *observedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.Header().Set("X-Request-ID", w.requestID)
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	w.writeFailed = w.writeFailed || err != nil || n != len(p)
	return n, err
}

func (w *observedWriter) Flush() { _ = w.FlushError() }

func (w *observedWriter) FlushError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		w.Header().Set("X-Request-ID", w.requestID)
		w.status = http.StatusOK
	}
	err := http.NewResponseController(w.ResponseWriter).Flush()
	w.writeFailed = w.writeFailed || err != nil
	return err
}

func (w *observedWriter) snapshot() (int, int64, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status, w.bytes, w.writeFailed
}

type observedTransport struct {
	base http.RoundTripper
	o    *observability
}

func (t observedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	started := time.Now()
	response, err := t.base.RoundTrip(r)
	if state := observation(r); state != nil && err == nil {
		t.o.headers.WithLabelValues(state.model, state.backend).Observe(time.Since(started).Seconds())
	}
	if response != nil {
		response.Request = r // Make request-local usage state available to ModifyResponse.
	}
	return response, err
}

// MetricsHandler exposes only telemetry, not the public inference API. It uses
// an instance-local registry, so embedding multiple gateways is safe.
func (s *Server) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.observability.registry, promhttp.HandlerOpts{}))
	return mux
}

// The proxy's free-form error logger can contain upstream-controlled text. Its
// transport failures and aborted copies are instead captured by request logs.
var discardedProxyLog = slog.NewLogLogger(slog.NewTextHandler(io.Discard, nil), slog.LevelError)
