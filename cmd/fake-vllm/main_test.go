package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

const (
	privatePrompt    = "NEVER_LOG_THIS_PROMPT"
	validRequest     = `{"model":"example","messages":[{"role":"user","content":"` + privatePrompt + `"}]}`
	streamingRequest = `{"model":"example","messages":[{"role":"user","content":"` + privatePrompt + `"}],"stream":true}`
)

func TestParseConfig(t *testing.T) {
	cfg, err := parseConfig(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want := config{addr: "127.0.0.1:8000", model: "example", chunkDelay: 100 * time.Millisecond}
	if cfg != want {
		t.Fatalf("defaults = %+v, want %+v", cfg, want)
	}
	cfg, err = parseConfig([]string{
		"-addr", "127.0.0.1:9000", "-model", "test-model", "-startup-delay", "1s",
		"-response-delay", "2s", "-chunk-delay", "3ms", "-error-status", "429",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	want = config{
		addr: "127.0.0.1:9000", model: "test-model", startupDelay: time.Second,
		responseDelay: 2 * time.Second, chunkDelay: 3 * time.Millisecond, errorStatus: 429,
	}
	if cfg != want {
		t.Fatalf("flags = %+v, want %+v", cfg, want)
	}
	for _, args := range [][]string{
		{"-startup-delay", "-1ns"}, {"-response-delay", "-1ms"}, {"-chunk-delay", "-1s"},
		{"-startup-delay", "invalid"}, {"-error-status", "-1"}, {"-error-status", "200"},
		{"-error-status", "399"}, {"-error-status", "600"}, {"-error-status", "invalid"},
		{"-model", ""}, {"-model", " \t"}, {"unexpected"}, {"-unknown"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if _, err := parseConfig(args, io.Discard); err == nil {
				t.Fatal("invalid flags succeeded")
			}
		})
	}
	for _, status := range []string{"0", "400", "499", "500", "599"} {
		if _, err := parseConfig([]string{"-error-status", status}, io.Discard); err != nil {
			t.Errorf("status %s: %v", status, err)
		}
	}
	if _, err := parseConfig([]string{"-help"}, io.Discard); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("help error = %v", err)
	}
}

func TestDeterministicCompletion(t *testing.T) {
	handler := newHandler(config{model: "example"}, nil)
	want := `{"id":"chatcmpl-fake-vllm","object":"chat.completion","created":1700000000,"model":"example","choices":[{"index":0,"message":{"role":"assistant","content":"Hello from fake vLLM."},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":5,"total_tokens":5}}` + "\n"
	for _, body := range []string{
		validRequest, validRequest,
		`{"model":"example","Model":"another-model","stream":false,"Stream":true}`,
		`{"model":"example","messages":[{"role":"user","content":"a different prompt"}],"stream":false}`,
		`{"model":"example","messages":[{"role":"user","content":[{"type":"text","text":"rich content"}]}],"temperature":0.5}`,
	} {
		response := performRequest(handler, http.MethodPost, "/v1/chat/completions", body)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", response.Code, response.Body.String())
		}
		if got := response.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		if got := response.Body.String(); got != want {
			t.Errorf("response = %s, want %s", got, want)
		}
	}

	handler = newHandler(config{model: "another-model"}, nil)
	response := performRequest(handler, http.MethodPost, "/v1/chat/completions", `{"model":"another-model","messages":[]}`)
	var got completion
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || got.Model != "another-model" {
		t.Fatalf("configured model response = %d %s", response.Code, response.Body.String())
	}
}

func TestInvalidRequests(t *testing.T) {
	handler := newHandler(config{model: "example"}, nil)
	for name, body := range map[string]string{
		"empty":           "",
		"malformed":       `{"model":`,
		"null":            `null`,
		"array":           `[]`,
		"missing model":   `{"messages":[]}`,
		"uppercase key":   `{"MODEL":"example"}`,
		"titlecase key":   `{"Model":"example"}`,
		"empty model":     `{"model":""}`,
		"wrong model":     `{"model":"other"}`,
		"model case":      `{"model":"Example"}`,
		"model spacing":   `{"model":" example "}`,
		"model type":      `{"model":42}`,
		"messages type":   `{"model":"example","messages":"not an array"}`,
		"string stream":   `{"model":"example","stream":"true"}`,
		"number stream":   `{"model":"example","stream":1}`,
		"null stream":     `{"model":"example","stream":null}`,
		"object stream":   `{"model":"example","stream":{}}`,
		"array stream":    `{"model":"example","stream":[]}`,
		"multiple JSON":   validRequest + `{}`,
		"trailing junk":   validRequest + `junk`,
		"trailing scalar": validRequest + `true`,
	} {
		t.Run(name, func(t *testing.T) {
			response := performRequest(handler, http.MethodPost, "/v1/chat/completions", body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			var result struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Error.Message == "" {
				t.Fatalf("missing JSON error: %s (%v)", response.Body.String(), err)
			}
		})
	}
}

func TestRequestBodyLimit(t *testing.T) {
	handler := newHandler(config{model: "example"}, nil)
	prefix := `{"model":"example","messages":[{"role":"user","content":"`
	suffix := `"}],"stream":false}`
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"at limit", prefix + strings.Repeat("x", maxRequestBytes-len(prefix)-len(suffix)) + suffix, http.StatusOK},
		{"over limit", prefix + strings.Repeat("x", maxRequestBytes-len(prefix)-len(suffix)+1) + suffix, http.StatusRequestEntityTooLarge},
		{"oversized trailing whitespace", validRequest + strings.Repeat(" ", maxRequestBytes-len(validRequest)+1), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := performRequest(handler, http.MethodPost, "/v1/chat/completions", tc.body)
			if response.Code != tc.want {
				t.Errorf("status = %d, want %d: %s", response.Code, tc.want, response.Body.String())
			}
		})
	}
}

func TestErrorStatus(t *testing.T) {
	for _, status := range []int{400, 429, 500, 503, 599} {
		handler := newHandler(config{model: "example", errorStatus: status}, nil)
		for _, body := range []string{validRequest, streamingRequest} {
			response := performRequest(handler, http.MethodPost, "/v1/chat/completions", body)
			if response.Code != status || response.Header().Get("Content-Type") != "application/json" {
				t.Errorf("forced status %d: got %d, headers %v", status, response.Code, response.Header())
			}
			if !strings.Contains(response.Body.String(), "synthetic fake vLLM error") {
				t.Errorf("missing synthetic error: %s", response.Body.String())
			}
		}
		if response := performRequest(handler, http.MethodGet, "/health", ""); response.Code != http.StatusOK {
			t.Errorf("forced status %d affected health: %d", status, response.Code)
		}
	}
}

func TestReadiness(t *testing.T) {
	for _, tc := range []struct {
		name        string
		errorStatus int
		readyStatus int
	}{
		{"normal", 0, http.StatusOK},
		{"forced error", http.StatusTooManyRequests, http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const delay = time.Hour
				handler := newHandler(config{model: "example", startupDelay: delay, errorStatus: tc.errorStatus}, nil)
				for _, step := range []struct {
					advance    time.Duration
					health     int
					completion int
				}{
					{0, http.StatusServiceUnavailable, http.StatusServiceUnavailable},
					{delay - time.Nanosecond, http.StatusServiceUnavailable, http.StatusServiceUnavailable},
					{time.Nanosecond, http.StatusOK, tc.readyStatus},
				} {
					// synctest advances virtual time, not wall time.
					time.Sleep(step.advance)
					if response := performRequest(handler, http.MethodGet, "/health", ""); response.Code != step.health {
						t.Errorf("health status = %d, want %d", response.Code, step.health)
					}
					if response := performRequest(handler, http.MethodPost, "/v1/chat/completions", validRequest); response.Code != step.completion {
						t.Errorf("completion status = %d, want %d", response.Code, step.completion)
					}
				}
			})
		})
	}
}

func TestResponseDelayBeforeHeaders(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		errorStatus int
		wantStatus  int
	}{
		{"JSON", validRequest, 0, http.StatusOK},
		{"SSE", streamingRequest, 0, http.StatusOK},
		{"error", validRequest, http.StatusTooManyRequests, http.StatusTooManyRequests},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const delay = time.Hour
				handler := newHandler(config{model: "example", responseDelay: delay, errorStatus: tc.errorStatus}, nil)
				start := time.Now()
				if response := performRequest(handler, http.MethodGet, "/health", ""); response.Code != http.StatusOK {
					t.Fatalf("health status = %d", response.Code)
				}
				if time.Since(start) != 0 {
					t.Fatal("response delay affected health")
				}
				writer := &headerObserver{ResponseRecorder: httptest.NewRecorder(), headers: make(chan struct{})}
				done := make(chan struct{})
				go func() {
					defer close(done)
					handler.ServeHTTP(writer, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body)))
				}()
				synctest.Wait()
				assertNotSignaled(t, writer.headers, "headers before response delay")
				time.Sleep(delay - time.Nanosecond)
				synctest.Wait()
				assertNotSignaled(t, writer.headers, "headers before full response delay")
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				select {
				case <-done:
				default:
					t.Fatal("completion did not finish at response delay")
				}
				if writer.Code != tc.wantStatus {
					t.Errorf("status = %d, want %d", writer.Code, tc.wantStatus)
				}
			})
		})
	}
}

func TestStreamingOverHTTP(t *testing.T) {
	server := httptest.NewServer(newHandler(config{model: "example"}, nil))
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	response, err := client.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(streamingRequest))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get("Cache-Control") != "no-cache" {
		t.Fatalf("stream response = %d, headers = %v", response.StatusCode, response.Header)
	}

	var chunks []completion
	done := false
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		if done || !strings.HasPrefix(line, "data: ") {
			t.Fatalf("unexpected SSE line: %q", line)
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			done = true
			continue
		}
		var chunk completion
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			t.Fatalf("invalid SSE JSON %q: %v", data, err)
		}
		chunks = append(chunks, chunk)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !done || len(chunks) != 7 {
		t.Fatalf("stream: done = %v, chunk count = %d", done, len(chunks))
	}
	var text strings.Builder
	for i, chunk := range chunks {
		if chunk.ID != syntheticID || chunk.Object != "chat.completion.chunk" || chunk.Created != syntheticTime || chunk.Model != "example" {
			t.Errorf("chunk %d metadata = %+v", i, chunk)
		}
		if len(chunk.Choices) != 1 || chunk.Choices[0].Index != 0 || chunk.Choices[0].Delta == nil || chunk.Choices[0].Message != nil {
			t.Fatalf("chunk %d choices = %+v", i, chunk.Choices)
		}
		choice := chunk.Choices[0]
		if i == len(chunks)-1 {
			if choice.FinishReason == nil || *choice.FinishReason != "stop" || *choice.Delta != (message{}) {
				t.Errorf("finish chunk = %+v", choice)
			}
			if chunk.Usage == nil || *chunk.Usage != (usage{PromptTokens: 0, CompletionTokens: 5, TotalTokens: 5}) {
				t.Errorf("synthetic stream usage = %+v", chunk.Usage)
			}
			continue
		}
		if choice.FinishReason != nil || chunk.Usage != nil {
			t.Errorf("chunk %d finished early", i)
		}
		if i == 0 {
			if *choice.Delta != (message{Role: "assistant"}) {
				t.Errorf("role chunk = %+v", choice.Delta)
			}
		} else {
			if choice.Delta.Role != "" || choice.Delta.Content == "" {
				t.Errorf("content chunk %d = %+v", i, choice.Delta)
			}
			text.WriteString(choice.Delta.Content)
		}
	}
	if text.String() != syntheticText {
		t.Errorf("stream text = %q", text.String())
	}
}

func TestCancellationOverHTTP(t *testing.T) {
	for _, phase := range []string{"response delay", "chunk delay"} {
		t.Run(phase, func(t *testing.T) {
			var logs bytes.Buffer
			cfg := config{model: "example"}
			if phase == "response delay" {
				cfg.responseDelay = time.Hour
			} else {
				cfg.chunkDelay = time.Hour
			}
			handler := newHandler(cfg, log.New(&logs, "", 0))
			entered, finished := make(chan struct{}), make(chan struct{})
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(finished)
				close(entered)
				handler.ServeHTTP(w, r)
			}))
			serverCtx, stopHandlers := context.WithCancel(context.Background())
			server.Config.BaseContext = func(net.Listener) context.Context { return serverCtx }
			server.Start()
			defer func() {
				// Ensure a failed cancellation assertion cannot hang test cleanup.
				stopHandlers()
				server.Close()
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(streamingRequest))
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Content-Type", "application/json")
			client := server.Client()
			client.Timeout = 5 * time.Second
			type result struct {
				response *http.Response
				err      error
			}
			results := make(chan result, 1)
			go func() {
				response, err := client.Do(request)
				results <- result{response, err}
			}()
			awaitSignal(t, entered)
			if phase == "chunk delay" {
				got := <-results // The client timeout also bounds this wait.
				if got.err != nil {
					t.Fatal(got.err)
				}
				defer got.response.Body.Close()
				line, err := bufio.NewReader(got.response.Body).ReadString('\n')
				if err != nil || !strings.Contains(line, `"role":"assistant"`) {
					t.Fatalf("initial chunk not flushed before delay: %q (%v)", line, err)
				}
			}
			cancel()
			awaitSignal(t, finished)
			if phase == "response delay" {
				got := <-results
				if got.response != nil {
					got.response.Body.Close()
				}
				if !errors.Is(got.err, context.Canceled) {
					t.Errorf("canceled HTTP request error = %v", got.err)
				}
			}
			if got := logs.String(); !strings.Contains(got, "completion request canceled: context canceled") || strings.Contains(got, privatePrompt) {
				t.Errorf("cancellation log = %q", got)
			}
		})
	}
}

func TestStreamFlushesAndStopsOnErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failWrite  int
		failFlush  int
		wantWrites int
		wantFlush  int
	}{
		{"success", 0, 0, 8, 8},
		{"first write", 1, 0, 1, 0},
		{"content write", 3, 0, 3, 2},
		{"finish write", 7, 0, 7, 6},
		{"done write", 8, 0, 8, 7},
		{"first flush", 0, 1, 1, 1},
		{"content flush", 0, 3, 3, 3},
		{"finish flush", 0, 7, 7, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writer := &failingStreamWriter{header: make(http.Header), failWrite: tc.failWrite, failFlush: tc.failFlush}
			handler := newHandler(config{model: "example"}, nil)
			handler.ServeHTTP(writer, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(streamingRequest)))
			if writer.writes != tc.wantWrites || writer.flushes != tc.wantFlush {
				t.Errorf("writes/flushes = %d/%d, want %d/%d", writer.writes, writer.flushes, tc.wantWrites, tc.wantFlush)
			}
		})
	}
}

func TestRoutes(t *testing.T) {
	handler := newHandler(config{model: "example"}, nil)
	for _, tc := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodGet, "/v1/chat/completions", http.StatusMethodNotAllowed},
		{http.MethodPost, "/health", http.StatusMethodNotAllowed},
		{http.MethodGet, "/missing", http.StatusNotFound},
	} {
		if response := performRequest(handler, tc.method, tc.path, ""); response.Code != tc.status {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, response.Code, tc.status)
		}
	}
}

func performRequest(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(response, request)
	return response
}

func awaitSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for handler synchronization")
	}
}

func assertNotSignaled(t *testing.T, signal <-chan struct{}, reason string) {
	t.Helper()
	select {
	case <-signal:
		t.Fatal(reason)
	default:
	}
}

type headerObserver struct {
	*httptest.ResponseRecorder
	headers chan struct{}
	once    sync.Once
}

func (w *headerObserver) WriteHeader(status int) {
	w.once.Do(func() { close(w.headers) })
	w.ResponseRecorder.WriteHeader(status)
}

func (w *headerObserver) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.headers) })
	return w.ResponseRecorder.Write(p)
}

func (w *headerObserver) Flush() {
	w.once.Do(func() { close(w.headers) })
	w.ResponseRecorder.Flush()
}

type failingStreamWriter struct {
	header    http.Header
	failWrite int
	failFlush int
	writes    int
	flushes   int
}

func (w *failingStreamWriter) Header() http.Header { return w.header }
func (w *failingStreamWriter) WriteHeader(int)     {}
func (w *failingStreamWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failWrite {
		return 0, errors.New("synthetic write failure")
	}
	return len(p), nil
}
func (w *failingStreamWriter) FlushError() error {
	w.flushes++
	if w.flushes == w.failFlush {
		return errors.New("synthetic flush failure")
	}
	return nil
}
