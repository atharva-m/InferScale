package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConformanceControlsRequireExplicitEnvironmentGate(t *testing.T) {
	t.Setenv("INFERSCALE_FAKE_TEST_CONTROLS", "false")
	t.Setenv("INFERSCALE_FAKE_TEST_IDENTITY", "stable")
	controls, err := testControlsFromEnvironment()
	if err != nil || controls != nil {
		t.Fatalf("test controls unexpectedly enabled: %#v %v", controls, err)
	}
	recorder := httptest.NewRecorder()
	(&server{model: "chat"}).handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/__inferscale_test/state", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatal("ungated fake runtime exposes diagnostic controls")
	}
	t.Setenv("INFERSCALE_FAKE_TEST_CONTROLS", "true")
	controls, err = testControlsFromEnvironment()
	if err != nil || controls == nil || controls.identity != "stable" {
		t.Fatalf("explicit gate failed: %#v %v", controls, err)
	}
	t.Setenv("INFERSCALE_FAKE_TEST_IDENTITY", "bad\nidentity")
	if _, err := testControlsFromEnvironment(); err == nil {
		t.Fatal("unsafe response identity accepted")
	}
}

func TestConformanceIdentityAndSchedulingCaptureExcludeSensitiveData(t *testing.T) {
	application := &server{model: "chat", test: &testControls{identity: "stable"}}
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"private-prompt"}]}`))
	request.Header.Set("Authorization", "Bearer private-token")
	request.Header.Set("Cookie", "private-cookie")
	request.Header.Set("x-llm-d-inference-objective", "stable-standard")
	recorder := httptest.NewRecorder()
	application.handler().ServeHTTP(recorder, request)
	if recorder.Code != 200 || recorder.Header().Get("X-InferScale-Test-Identity") != "stable" {
		t.Fatalf("missing stable response identity: %d", recorder.Code)
	}
	state := httptest.NewRecorder()
	application.handler().ServeHTTP(state, httptest.NewRequest("GET", "/__inferscale_test/state", nil))
	for _, secret := range []string{"private-token", "private-prompt", "private-cookie", "runtime response"} {
		if strings.Contains(state.Body.String(), secret) {
			t.Fatalf("diagnostics leaked %q", secret)
		}
	}
	var parsed struct {
		Requests, Completed  int
		AuthorizationPresent bool `json:"authorization_present"`
		Headers              map[string]string
	}
	if err := json.Unmarshal(state.Body.Bytes(), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Requests != 1 || parsed.Completed != 1 || !parsed.AuthorizationPresent || parsed.Headers["x-llm-d-inference-objective"] != "stable-standard" {
		t.Fatalf("incorrect diagnostic state: %#v", parsed)
	}
}

type flushSignalRecorder struct {
	*httptest.ResponseRecorder
	once    sync.Once
	flushed chan struct{}
}

func (w *flushSignalRecorder) Flush() {
	w.ResponseRecorder.Flush()
	w.once.Do(func() { close(w.flushed) })
}

func TestConformanceStreamCancellationReleasesActiveRequest(t *testing.T) {
	application := &server{model: "chat", test: &testControls{identity: "stable"}}
	application.test.streamDelayMS.Store(10000)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"chat","messages":[{"role":"user","content":"hello"}],"stream":true}`)).WithContext(ctx)
	recorder := &flushSignalRecorder{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
	done := make(chan struct{})
	go func() { application.handler().ServeHTTP(recorder, request); close(done) }()
	select {
	case <-recorder.flushed:
	case <-time.After(time.Second):
		t.Fatal("first SSE frame was buffered")
	}
	if application.active.Load() != 1 || application.completed.Load() != 0 {
		t.Fatal("stream did not remain active after its first frame")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("canceled stream retained its runtime slot")
	}
	if application.active.Load() != 0 || application.canceled.Load() != 1 || application.completed.Load() != 0 || strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Fatal("canceled stream was counted as a completed request")
	}
}

func TestConformanceDelayControlsAreBounded(t *testing.T) {
	application := &server{model: "chat", test: &testControls{identity: "stable"}}
	for _, body := range []string{`{"response_delay_ms":-1}`, `{"stream_delay_ms":10001}`, `{"response_delay_ms":10,"unknown":true}`, `{} {}`} {
		recorder := httptest.NewRecorder()
		application.handler().ServeHTTP(recorder, httptest.NewRequest("POST", "/__inferscale_test/control", strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("unsafe test control accepted: %s", body)
		}
	}
	recorder := httptest.NewRecorder()
	application.handler().ServeHTTP(recorder, httptest.NewRequest("POST", "/__inferscale_test/control", strings.NewReader(`{"response_delay_ms":100,"stream_delay_ms":250}`)))
	if recorder.Code != http.StatusOK || application.test.responseDelayMS.Load() != 100 || application.test.streamDelayMS.Load() != 250 {
		t.Fatal("bounded control was not applied")
	}
}
