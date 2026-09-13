package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"github.com/inferscale/inferscale/internal/admission"
	"github.com/inferscale/inferscale/internal/auth"
	"github.com/inferscale/inferscale/internal/benchmark"
	"github.com/inferscale/inferscale/internal/config"
	"github.com/inferscale/inferscale/internal/observability"
	"github.com/inferscale/inferscale/internal/storage/postgres"
	"github.com/inferscale/inferscale/internal/storage/valkey"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.DatabaseURL == "" || cfg.ValkeyAddr == "" || cfg.BenchmarkCallbackSigningKey == "" {
		return errors.New("INFERSCALE_DATABASE_URL, INFERSCALE_VALKEY_ADDR, and INFERSCALE_BENCHMARK_CALLBACK_SIGNING_KEY are required")
	}
	logger := observability.NewLogger(cfg.LogLevel)
	shutdownTracing, err := observability.InitTracing(ctx, "inferscale-admission", cfg.OTLPEndpoint)
	if err != nil {
		return fmt.Errorf("initialize tracing: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()

	store, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer store.Close()
	cache, err := valkey.Open(ctx, valkey.Config{Address: cfg.ValkeyAddr, Prefix: "inferscale"})
	if err != nil {
		return err
	}
	defer cache.Close()

	authService := auth.NewService(postgres.NewAuthRepository(store))
	callbackTokens, err := benchmark.NewCallbackTokens([]byte(cfg.BenchmarkCallbackSigningKey))
	if err != nil {
		return err
	}
	authenticator := admission.RunScopedAuthenticator{
		Primary: admission.AuthServiceAdapter{Service: authService},
		Tokens:  callbackTokens,
		Runs:    postgres.NewBenchmarkRepository(store),
	}
	authorizer := admission.PolicyRepository{
		Deployments: postgres.NewDeploymentRepository(store),
		Tenants:     postgres.NewTenantRepository(store),
	}
	registry := prometheus.NewRegistry()
	admissionMetrics := observability.NewAdmissionMetrics(registry)
	extAuth := admission.NewServer(
		authenticator,
		authorizer,
		admission.ValkeyRateLimiter{Client: cache},
		admissionMetrics,
	)
	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(admission.MaxExtAuthGRPCMessageBytes),
		grpc.MaxSendMsgSize(1<<20),
	)
	authv3.RegisterAuthorizationServer(grpcServer, extAuth)
	listener, err := net.Listen("tcp", cfg.GRPCAddr)
	if err != nil {
		return fmt.Errorf("listen on admission gRPC address: %w", err)
	}

	healthMux := http.NewServeMux()
	healthMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	healthMux.Handle("GET /readyz", observability.HealthHandler(map[string]observability.Check{
		"postgres": store.Ping,
		"valkey":   cache.Ping,
	}, 3*time.Second))
	healthServer := &http.Server{Addr: cfg.HTTPAddr, Handler: healthMux, ReadHeaderTimeout: 5 * time.Second}
	metricsServer := &http.Server{
		Addr: cfg.MetricsAddr, Handler: promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 3)
	go func() { errCh <- grpcServer.Serve(listener) }()
	go func() { errCh <- healthServer.ListenAndServe() }()
	go func() { errCh <- metricsServer.ListenAndServe() }()
	logger.Info("InferScale admission started", "grpc_addr", cfg.GRPCAddr, "health_addr", cfg.HTTPAddr)
	select {
	case <-ctx.Done():
	case runErr := <-errCh:
		if runErr != nil && !errors.Is(runErr, http.ErrServerClosed) && !errors.Is(runErr, grpc.ErrServerStopped) {
			stop()
			return runErr
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	grpcDone := make(chan struct{})
	go func() { grpcServer.GracefulStop(); close(grpcDone) }()
	select {
	case <-grpcDone:
	case <-shutdownCtx.Done():
		grpcServer.Stop()
	}
	return errors.Join(healthServer.Shutdown(shutdownCtx), metricsServer.Shutdown(shutdownCtx))
}
