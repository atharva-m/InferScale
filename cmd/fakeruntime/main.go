// Command fakeruntime is a deterministic CPU-only OpenAI-compatible server
// used exclusively by the local-wsl control-plane acceptance path. It is not
// included in release images and must never be used for benchmark claims.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const maxRequestBytes = 1 << 20

type chatRequest struct {
	Model         string          `json:"model"`
	Messages      []chatMessage   `json:"messages"`
	Stream        bool            `json:"stream"`
	MaxTokens     *int            `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	Stop          json.RawMessage `json:"stop,omitempty"`
	Seed          *int64          `json:"seed,omitempty"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type server struct {
	model     string
	sequence  atomic.Uint64
	requests  atomic.Uint64
	active    atomic.Int64
	canceled  atomic.Uint64
	completed atomic.Uint64
	test      *testControls
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	model := strings.TrimSpace(os.Getenv("SERVED_MODEL_NAME"))
	if model == "" {
		model = "inferscale-local-fake"
	}
	address := strings.TrimSpace(os.Getenv("LISTEN_ADDR"))
	if address == "" {
		address = ":8000"
	}
	application := &server{model: model}
	test, err := testControlsFromEnvironment()
	if err != nil {
		return err
	}
	application.test = test
	mux := application.handler()

	httpServer := &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		slog.Info("local fake runtime started", "address", address, "model", model)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /metrics", s.metrics)
	mux.HandleFunc("POST /v1/chat/completions", s.chatCompletions)
	if s.test != nil {
		mux.HandleFunc("GET /__inferscale_test/state", s.testState)
		mux.HandleFunc("POST /__inferscale_test/control", s.testControl)
	}
	return mux
}

func (s *server) health(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(writer, "ok\n")
}

func (s *server) metrics(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = fmt.Fprintf(writer, "# HELP inferscale_fake_requests_total Local fake chat requests.\n")
	_, _ = fmt.Fprintf(writer, "# TYPE inferscale_fake_requests_total counter\n")
	_, _ = fmt.Fprintf(writer, "inferscale_fake_requests_total %d\n", s.requests.Load())
	_, _ = fmt.Fprintf(writer, "# TYPE vllm:num_requests_running gauge\nvllm:num_requests_running %d\n", s.active.Load())
	_, _ = io.WriteString(writer, "# TYPE vllm:num_requests_waiting gauge\nvllm:num_requests_waiting 0\n")
	_, _ = fmt.Fprintf(writer, "# TYPE inferscale_fake_canceled_total counter\ninferscale_fake_canceled_total %d\n", s.canceled.Load())
}

func (s *server) chatCompletions(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var input chatRequest
	if err := decoder.Decode(&input); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_request", "request must be valid text-only chat JSON")
		return
	}
	if err := requireEOF(decoder); err != nil || input.Model != s.model || len(input.Messages) == 0 {
		writeError(writer, http.StatusBadRequest, "invalid_request", "model and at least one message are required")
		return
	}
	if input.MaxTokens != nil && *input.MaxTokens < 1 ||
		input.Temperature != nil && (*input.Temperature < 0 || *input.Temperature > 2) ||
		input.TopP != nil && (*input.TopP <= 0 || *input.TopP > 1) ||
		input.StreamOptions != nil && !input.Stream || !validStop(input.Stop) {
		writeError(writer, http.StatusBadRequest, "invalid_request", "generation parameters are outside the v1 text-chat subset")
		return
	}
	for _, message := range input.Messages {
		if (message.Role != "system" && message.Role != "user" && message.Role != "assistant") || message.Content == "" {
			writeError(writer, http.StatusBadRequest, "invalid_request", "messages must use text system, user, or assistant roles")
			return
		}
	}
	s.requests.Add(1)
	s.active.Add(1)
	defer func() {
		s.active.Add(-1)
		if request.Context().Err() != nil {
			s.canceled.Add(1)
		}
	}()
	if s.test != nil {
		s.test.observe(request)
		writer.Header().Set("X-InferScale-Test-Identity", s.test.identity)
		if !waitTestDelay(request.Context(), s.test.responseDelayMS.Load()) {
			return
		}
	}
	id := "chatcmpl-local-" + strconv.FormatUint(s.sequence.Add(1), 10)
	created := time.Now().Unix()
	content := "InferScale local fake runtime response."
	usage := map[string]int{"prompt_tokens": 1, "completion_tokens": 6, "total_tokens": 7}
	if !input.Stream {
		writeJSON(writer, http.StatusOK, map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": s.model,
			"choices": []any{map[string]any{
				"index": 0, "message": map[string]string{"role": "assistant", "content": content}, "finish_reason": "stop",
			}},
			"usage": usage,
		})
		s.completed.Add(1)
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, http.StatusInternalServerError, "stream_unavailable", "streaming is unavailable")
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("X-Accel-Buffering", "no")
	writeSSE(writer, map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"role": "assistant", "content": content}, "finish_reason": nil}},
	})
	flusher.Flush()
	if s.test != nil && !waitTestDelay(request.Context(), s.test.streamDelayMS.Load()) {
		return
	}
	select {
	case <-request.Context().Done():
		return
	default:
	}
	writeSSE(writer, map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]string{}, "finish_reason": "stop"}},
	})
	if input.StreamOptions != nil && input.StreamOptions.IncludeUsage {
		writeSSE(writer, map[string]any{
			"id": id, "object": "chat.completion.chunk", "created": created, "model": s.model,
			"choices": []any{}, "usage": usage,
		})
	}
	_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	flusher.Flush()
	s.completed.Add(1)
}

func writeSSE(writer io.Writer, value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(writer, "data: %s\n\n", payload)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]string{"type": code, "message": message}})
}

func requireEOF(decoder *json.Decoder) error {
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("multiple JSON values")
	}
	return err
}

func validStop(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return single != ""
	}
	var multiple []string
	if json.Unmarshal(raw, &multiple) != nil || len(multiple) == 0 || len(multiple) > 4 {
		return false
	}
	for _, value := range multiple {
		if value == "" {
			return false
		}
	}
	return true
}
