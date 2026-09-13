// fake-vllm is a deterministic, GPU-free test backend. It performs no inference.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

const (
	maxRequestBytes = 1 << 20
	syntheticText   = "Hello from fake vLLM."
	syntheticID     = "chatcmpl-fake-vllm"
	syntheticTime   = 1700000000
)

type config struct {
	addr          string
	model         string
	startupDelay  time.Duration
	responseDelay time.Duration
	chunkDelay    time.Duration
	errorStatus   int
}

func parseConfig(args []string, output io.Writer) (config, error) {
	var cfg config
	flags := flag.NewFlagSet("fake-vllm", flag.ContinueOnError)
	flags.SetOutput(output)
	flags.StringVar(&cfg.addr, "addr", "127.0.0.1:8000", "HTTP listen address")
	flags.StringVar(&cfg.model, "model", "example", "exact model name accepted by completions")
	flags.DurationVar(&cfg.startupDelay, "startup-delay", 0, "delay before health and completions become ready")
	flags.DurationVar(&cfg.responseDelay, "response-delay", 0, "completion delay before sending headers")
	flags.DurationVar(&cfg.chunkDelay, "chunk-delay", 100*time.Millisecond, "delay between SSE events")
	flags.IntVar(&cfg.errorStatus, "error-status", 0, "synthetic completion error status: 0 or HTTP 400-599")
	if err := flags.Parse(args); err != nil {
		return cfg, err
	}
	if flags.NArg() != 0 {
		return cfg, errors.New("unexpected positional arguments")
	}
	if strings.TrimSpace(cfg.model) == "" {
		return cfg, errors.New("-model must not be empty")
	}
	if cfg.startupDelay < 0 || cfg.responseDelay < 0 || cfg.chunkDelay < 0 {
		return cfg, errors.New("delay flags must not be negative")
	}
	if cfg.errorStatus != 0 && (cfg.errorStatus < 400 || cfg.errorStatus > 599) {
		return cfg, errors.New("-error-status must be 0 or HTTP 400-599")
	}
	return cfg, nil
}

type completionRequest struct {
	Model string `json:"model"`
	// Prompt contents are accepted but never used or logged, including rich content.
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Stream json.RawMessage `json:"stream"`
}

type message struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type choice struct {
	Index        int      `json:"index"`
	Message      *message `json:"message,omitempty"`
	Delta        *message `json:"delta,omitempty"`
	FinishReason *string  `json:"finish_reason"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type completion struct {
	ID      string   `json:"id"`
	Object  string   `json:"object"`
	Created int64    `json:"created"`
	Model   string   `json:"model"`
	Choices []choice `json:"choices"`
	Usage   *usage   `json:"usage,omitempty"`
}

func newHandler(cfg config, logger *log.Logger) http.Handler {
	if logger == nil {
		logger = log.Default()
	}
	readyAt := time.Now().Add(cfg.startupDelay)
	ready := func(w http.ResponseWriter) bool {
		if time.Now().Before(readyAt) {
			writeError(w, http.StatusServiceUnavailable, "fake vLLM is starting")
			return false
		}
		return true
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if ready(w) {
			writeJSON(w, http.StatusOK, struct {
				Status string `json:"status"`
			}{Status: "ok"})
		}
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := r.Context().Err(); err != nil {
				logger.Printf("completion request canceled: %v", err)
			}
		}()
		// Readiness takes priority over simulated latency and errors.
		if !ready(w) {
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		decoder := json.NewDecoder(r.Body)
		var fields map[string]json.RawMessage
		if err := decoder.Decode(&fields); err != nil {
			writeDecodeError(w, err)
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			writeDecodeError(w, err)
			return
		}
		// Match JSON keys exactly, like vLLM, rather than using Go's
		// case-insensitive struct field matching.
		var req completionRequest
		if err := json.Unmarshal(fields["model"], &req.Model); err != nil {
			writeDecodeError(w, err)
			return
		}
		if raw, ok := fields["messages"]; ok {
			if err := json.Unmarshal(raw, &req.Messages); err != nil {
				writeDecodeError(w, err)
				return
			}
		}
		req.Stream = fields["stream"]
		// Consume the body first so net/http can detect HTTP/1 client disconnects
		// during the delay. No response headers have been sent yet.
		if wait(r.Context(), cfg.responseDelay) != nil {
			return
		}
		if req.Model == "" || req.Model != cfg.model {
			writeError(w, http.StatusBadRequest, "model must be nonempty and exactly match the configured model")
			return
		}
		stream := false
		// A missing stream field means false; null and other JSON types are invalid.
		switch string(req.Stream) {
		case "", "false":
		case "true":
			stream = true
		default:
			writeError(w, http.StatusBadRequest, "stream must be a boolean")
			return
		}
		if r.Context().Err() != nil {
			return
		}
		if cfg.errorStatus != 0 {
			writeError(w, cfg.errorStatus, "synthetic fake vLLM error")
			return
		}

		stop := "stop"
		// These fixed counts are synthetic, not measurements from a tokenizer.
		tokens := &usage{PromptTokens: 0, CompletionTokens: 5, TotalTokens: 5}
		response := completion{
			ID: syntheticID, Object: "chat.completion", Created: syntheticTime, Model: cfg.model,
			Choices: []choice{{Index: 0, Message: &message{Role: "assistant", Content: syntheticText}, FinishReason: &stop}},
			Usage:   tokens,
		}
		if !stream {
			writeJSON(w, http.StatusOK, response)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		response.Object = "chat.completion.chunk"
		response.Usage = nil
		response.Choices = []choice{{Index: 0, Delta: &message{Role: "assistant"}}}
		send := func() error {
			if err := r.Context().Err(); err != nil {
				return err
			}
			data, err := json.Marshal(response)
			if err != nil {
				return err
			}
			return writeEvent(w, string(data))
		}
		if send() != nil {
			return
		}
		for _, part := range []string{"Hello", " from", " fake", " vLLM", "."} {
			if wait(r.Context(), cfg.chunkDelay) != nil {
				return
			}
			response.Choices[0].Delta = &message{Content: part}
			if send() != nil {
				return
			}
		}
		if wait(r.Context(), cfg.chunkDelay) != nil {
			return
		}
		response.Choices[0].Delta = &message{}
		response.Choices[0].FinishReason = &stop
		response.Usage = tokens
		if send() != nil || wait(r.Context(), cfg.chunkDelay) != nil {
			return
		}
		_ = writeEvent(w, "[DONE]")
	})
	return mux
}

func wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay == 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func writeEvent(w http.ResponseWriter, data string) error {
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	return http.NewResponseController(w).Flush()
}

func writeDecodeError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds 1 MiB")
		return
	}
	writeError(w, http.StatusBadRequest, "body must contain one valid JSON request")
}

func writeError(w http.ResponseWriter, status int, text string) {
	kind := "invalid_request_error"
	if status >= 500 {
		kind = "server_error"
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": text, "type": kind},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func serve(ctx context.Context, cfg config) error {
	// Bind before constructing the handler's readiness deadline; startup never sleeps.
	listener, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr:              cfg.addr,
		Handler:           newHandler(cfg, log.Default()),
		ReadHeaderTimeout: 5 * time.Second,
	}
	defer server.Close()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	log.Printf("fake vLLM synthetic test backend listening on %s; no inference", listener.Addr())
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			// The deferred Close forcibly closes connections after the grace period.
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		return nil
	}
}

func main() {
	cfg, err := parseConfig(os.Args[1:], os.Stderr)
	if errors.Is(err, flag.ErrHelp) {
		return
	}
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}
