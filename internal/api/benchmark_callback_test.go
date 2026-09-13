package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/inferscale/inferscale/internal/benchmark"
)

type resultIngesterFunc func(context.Context, string, benchmark.ResultReport) (benchmark.Run, error)

func (f resultIngesterFunc) IngestResult(ctx context.Context, id string, report benchmark.ResultReport) (benchmark.Run, error) {
	return f(ctx, id, report)
}

func TestBenchmarkCallbackRequiresRunScopedBearerToken(t *testing.T) {
	tokens, err := benchmark.NewCallbackTokens([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	called := false
	server := NewServer(Dependencies{
		BenchmarkCallbacks: tokens,
		BenchmarkResults: resultIngesterFunc(func(_ context.Context, id string, report benchmark.ResultReport) (benchmark.Run, error) {
			called = true
			return benchmark.Run{ID: id, State: report.State}, nil
		}),
	})
	body := []byte(`{"state":"failed","error":"runner exited"}`)
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/benchmarks/run-a/result", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized || called {
		t.Fatalf("unauthenticated status=%d called=%v body=%s", recorder.Code, called, recorder.Body.String())
	}

	token, _ := tokens.Issue("run-a")
	request = httptest.NewRequest(http.MethodPost, "/internal/v1/benchmarks/run-a/result", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	recorder = httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || !called {
		t.Fatalf("authenticated status=%d called=%v body=%s", recorder.Code, called, recorder.Body.String())
	}
}

func TestBenchmarkCallbackRejectsTokenForAnotherRun(t *testing.T) {
	tokens, _ := benchmark.NewCallbackTokens([]byte("0123456789abcdef0123456789abcdef"))
	token, _ := tokens.Issue("run-b")
	server := NewServer(Dependencies{BenchmarkCallbacks: tokens, BenchmarkResults: resultIngesterFunc(func(context.Context, string, benchmark.ResultReport) (benchmark.Run, error) {
		t.Fatal("cross-run token reached ingester")
		return benchmark.Run{}, nil
	})})
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/benchmarks/run-a/result", bytes.NewBufferString(`{"state":"failed","error":"x"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
