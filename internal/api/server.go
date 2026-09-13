package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/inferscale/inferscale/internal/api/middleware"
	"github.com/inferscale/inferscale/internal/auth"
	"github.com/inferscale/inferscale/internal/benchmark"
	"github.com/inferscale/inferscale/internal/deployment"
)

type HealthCheck func(context.Context) error

type BenchmarkResultIngester interface {
	IngestResult(context.Context, string, benchmark.ResultReport) (benchmark.Run, error)
}

type BenchmarkCallbackVerifier interface {
	Verify(runID, token string) error
}

type IdempotencyResponseStore interface {
	GetIdempotency(context.Context, string, string) ([]byte, bool, error)
	PutIdempotency(context.Context, string, string, []byte, time.Duration) (bool, error)
}

type Dependencies struct {
	Deployments        *deployment.Service
	Benchmarks         *deployment.BenchmarkService
	BenchmarkResults   BenchmarkResultIngester
	BenchmarkCallbacks BenchmarkCallbackVerifier
	Metrics            deployment.MetricsReader
	Auth               *auth.Service
	Idempotency        IdempotencyResponseStore
	Readiness          map[string]HealthCheck
	Logger             *slog.Logger
}

type Server struct {
	deployments        *deployment.Service
	benchmarks         *deployment.BenchmarkService
	benchmarkResults   BenchmarkResultIngester
	benchmarkCallbacks BenchmarkCallbackVerifier
	metrics            deployment.MetricsReader
	idempotency        IdempotencyResponseStore
	readiness          map[string]HealthCheck
	logger             *slog.Logger
	handler            http.Handler
}

func NewServer(deps Dependencies) *Server {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	server := &Server{
		deployments: deps.Deployments, benchmarks: deps.Benchmarks, metrics: deps.Metrics,
		benchmarkResults: deps.BenchmarkResults, benchmarkCallbacks: deps.BenchmarkCallbacks,
		idempotency: deps.Idempotency,
		readiness:   deps.Readiness, logger: deps.Logger,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.health)
	mux.HandleFunc("GET /readyz", server.ready)
	mux.HandleFunc("POST /internal/v1/benchmarks/{id}/result", server.ingestBenchmarkResult)

	protected := http.NewServeMux()
	protected.Handle("POST /v1/deployments", server.require(auth.ScopeDeploymentsWrite, http.HandlerFunc(server.createDeployment)))
	protected.Handle("GET /v1/deployments", server.require(auth.ScopeDeploymentsRead, http.HandlerFunc(server.listDeployments)))
	protected.Handle("GET /v1/deployments/{id}", server.require(auth.ScopeDeploymentsRead, http.HandlerFunc(server.getDeployment)))
	protected.Handle("PATCH /v1/deployments/{id}", server.require(auth.ScopeDeploymentsWrite, http.HandlerFunc(server.patchDeployment)))
	protected.Handle("DELETE /v1/deployments/{id}", server.require(auth.ScopeDeploymentsWrite, http.HandlerFunc(server.deleteDeployment)))
	protected.Handle("POST /v1/deployments/{id}/benchmarks", server.require(auth.ScopeBenchmarksRun, http.HandlerFunc(server.createBenchmark)))
	protected.Handle("GET /v1/deployments/{id}/benchmarks", server.require(auth.ScopeBenchmarksRead, http.HandlerFunc(server.listBenchmarks)))
	protected.Handle("GET /v1/deployments/{id}/metrics", server.require(auth.ScopeDeploymentsRead, http.HandlerFunc(server.getMetrics)))
	protected.Handle("GET /v1/operations/{id}", server.require(auth.ScopeDeploymentsRead, http.HandlerFunc(server.getOperation)))
	if deps.Auth != nil {
		mux.Handle("/v1/", middleware.Authenticate(deps.Auth, server.writeError, protected))
	} else {
		mux.Handle("/v1/", protected)
	}
	server.handler = middleware.RequestIDMiddleware(middleware.Recover(server.logger, server.writeError, mux))
	return server
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) require(scope string, next http.Handler) http.Handler {
	return middleware.RequireScope(scope, s.writeError, next)
}
