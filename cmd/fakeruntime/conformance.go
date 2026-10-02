package main

// Diagnostic controls exist only in the development fake-runtime binary and
// require an explicit environment gate. The live harness reaches them through
// kubectl port-forward; no production runtime or API handler exposes them.
import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sync"
	"sync/atomic"
	"time"
)

type testControls struct {
	identity             string
	responseDelayMS      atomic.Int64
	streamDelayMS        atomic.Int64
	mu                   sync.Mutex
	headers              map[string]string
	authorizationPresent bool
}

func testControlsFromEnvironment() (*testControls, error) {
	switch os.Getenv("INFERSCALE_FAKE_TEST_CONTROLS") {
	case "", "false":
		return nil, nil
	case "true":
	default:
		return nil, fmt.Errorf("INFERSCALE_FAKE_TEST_CONTROLS must be true or false")
	}
	identity := os.Getenv("INFERSCALE_FAKE_TEST_IDENTITY")
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`).MatchString(identity) {
		return nil, fmt.Errorf("test identity must be a short label")
	}
	return &testControls{identity: identity}, nil
}

func (c *testControls) observe(request *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Retain only the scheduling fields being qualified. Never capture a
	// bearer value, message body, generated text, arbitrary header or cookie.
	c.headers = map[string]string{}
	for _, key := range []string{
		"x-llm-d-inference-fairness-id", "x-llm-d-inference-objective",
		"x-llm-d-model-name-rewrite", "x-llm-d-slo-ttft-ms", "x-llm-d-slo-tpot-ms",
		"x-inferscale-tenant-id", "x-inferscale-deployment-id", "x-inferscale-priority-class",
		"x-gateway-inference-fairness-id", "x-gateway-inference-objective", "x-gateway-model-name-rewrite",
	} {
		if value := request.Header.Get(key); value != "" {
			c.headers[key] = value
		}
	}
	c.authorizationPresent = request.Header.Get("Authorization") != ""
}

func (s *server) testState(writer http.ResponseWriter, _ *http.Request) {
	s.test.mu.Lock()
	defer s.test.mu.Unlock()
	writeJSON(writer, http.StatusOK, map[string]any{
		"identity": s.test.identity, "model": s.model,
		"requests": s.requests.Load(), "active": s.active.Load(),
		"canceled": s.canceled.Load(), "completed": s.completed.Load(),
		"headers": s.test.headers, "authorization_present": s.test.authorizationPresent,
	})
}

func (s *server) testControl(writer http.ResponseWriter, request *http.Request) {
	var control struct {
		ResponseDelayMS int64 `json:"response_delay_ms"`
		StreamDelayMS   int64 `json:"stream_delay_ms"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&control); err != nil || requireEOF(decoder) != nil ||
		control.ResponseDelayMS < 0 || control.ResponseDelayMS > 10000 ||
		control.StreamDelayMS < 0 || control.StreamDelayMS > 10000 {
		writeError(writer, http.StatusBadRequest, "invalid_test_control", "test delays must be 0 to 10000 milliseconds")
		return
	}
	s.test.responseDelayMS.Store(control.ResponseDelayMS)
	s.test.streamDelayMS.Store(control.StreamDelayMS)
	writeJSON(writer, http.StatusOK, map[string]bool{"accepted": true})
}

func waitTestDelay(ctx context.Context, milliseconds int64) bool {
	if milliseconds <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(time.Duration(milliseconds) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
