package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	platformapi "github.com/inferscale/inferscale/internal/api"
	"github.com/inferscale/inferscale/internal/auth"
	"github.com/inferscale/inferscale/internal/benchmark"
	"github.com/inferscale/inferscale/internal/config"
	"github.com/inferscale/inferscale/internal/deployment"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	platformmetrics "github.com/inferscale/inferscale/internal/metrics"
	"github.com/inferscale/inferscale/internal/observability"
	"github.com/inferscale/inferscale/internal/storage/postgres"
	"github.com/inferscale/inferscale/internal/storage/valkey"
	platformsync "github.com/inferscale/inferscale/internal/sync"
	"github.com/inferscale/inferscale/internal/usage"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	if err := cfg.ValidateAPI(); err != nil {
		return err
	}
	if cfg.DatabaseURL == "" || cfg.ValkeyAddr == "" {
		return errors.New("INFERSCALE_DATABASE_URL and INFERSCALE_VALKEY_ADDR are required")
	}
	if cfg.BenchmarkImage == "" || cfg.BenchmarkCallbackSigningKey == "" {
		return errors.New("INFERSCALE_BENCHMARK_IMAGE and INFERSCALE_BENCHMARK_CALLBACK_SIGNING_KEY are required")
	}
	logger := observability.NewLogger(cfg.LogLevel)
	shutdownTracing, err := observability.InitTracing(ctx, "inferscale-api", cfg.OTLPEndpoint)
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

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	restConfig, err := kubeutil.RESTConfig(cfg.Kubeconfig)
	if err != nil {
		return err
	}
	kubeClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		return fmt.Errorf("create Kubernetes client: %w", err)
	}

	deployments := postgres.NewDeploymentRepository(store)
	deploymentService := deployment.NewService(deployments)
	benchmarkRepository := postgres.NewBenchmarkRepository(store)
	benchmarkService := deployment.NewBenchmarkService(deployments, benchmarkRepository)
	benchmarkResults := benchmark.NewService(benchmarkRepository, benchmark.NewID)
	callbackTokens, err := benchmark.NewCallbackTokens([]byte(cfg.BenchmarkCallbackSigningKey))
	if err != nil {
		return err
	}
	benchmarkSubmitter, err := benchmark.NewKubernetesJobSubmitter(kubeClient, callbackTokens, benchmark.KubernetesJobSubmitterOptions{
		Image: cfg.BenchmarkImage, CallbackBaseURL: cfg.BenchmarkCallbackURL,
		ResultsClaimName: cfg.BenchmarkResultsClaim,
		Authority: benchmark.ExecutionAuthority{
			Provider: cfg.BenchmarkProvider, InferenceBaseURL: cfg.BenchmarkInferenceBaseURL,
			PrometheusURL: cfg.BenchmarkPrometheusURL, DriverVersion: cfg.BenchmarkDriverVersion,
			CUDAVersion: cfg.BenchmarkCUDAVersion,
			RuntimeVersions: map[benchmark.Backend]string{
				benchmark.BackendVLLM:        cfg.VLLMVersion,
				benchmark.BackendTensorRTLLM: cfg.TRTLLMVersion,
			},
		},
	})
	if err != nil {
		return err
	}
	workerID, err := os.Hostname()
	if err != nil || workerID == "" {
		workerID = "inferscale-api"
	}
	benchmarkScheduler := benchmark.NewScheduler(benchmarkRepository, benchmarkSubmitter, workerID, cfg.BenchmarkPollInterval, logger)
	authService := auth.NewService(postgres.NewAuthRepository(store))
	prometheusClient, err := platformmetrics.NewPrometheusClient(cfg.PrometheusURL, nil)
	if err != nil {
		return err
	}
	metricsReader := platformmetrics.DeploymentReader{Prometheus: prometheusClient, Deployments: deployments}
	usageRepository := postgres.NewUsageRepository(store)
	usageAggregator := usage.NewAggregator(usage.PrometheusSource{
		Prometheus: prometheusClient, Deployments: usageRepository,
	}, usageRepository)
	outboxRepository := postgres.NewOutboxRepository(store)
	outbox := platformsync.NewWorker(
		outboxRepository,
		deployments,
		platformsync.KubernetesApplier{Client: kubeClient, FieldOwner: "inferscale-api"},
		logger,
	)

	registry := prometheus.NewRegistry()
	apiMetrics := observability.NewAPIMetrics(registry)
	server := platformapi.NewServer(platformapi.Dependencies{
		Deployments:        deploymentService,
		Benchmarks:         benchmarkService,
		BenchmarkResults:   benchmarkResults,
		BenchmarkCallbacks: callbackTokens,
		Metrics:            metricsReader,
		Auth:               authService,
		Idempotency:        cache,
		Logger:             logger,
		Readiness: map[string]platformapi.HealthCheck{
			"postgres": store.Ping,
			"valkey":   cache.Ping,
			"kubernetes": func(checkCtx context.Context) error {
				return kubeClient.Get(checkCtx, client.ObjectKey{Name: cfg.ControlNamespace}, &corev1.Namespace{})
			},
		},
	})
	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           observability.TraceHTTP("inferscale-api", apiMetrics.InstrumentHTTP(server.Handler())),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	metricsServer := &http.Server{
		Addr:              cfg.MetricsAddr,
		Handler:           promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 6)
	go func() { errCh <- outbox.Run(ctx, time.Second) }()
	go func() { errCh <- benchmarkScheduler.Run(ctx) }()
	go func() {
		errCh <- usageAggregator.Run(ctx, cfg.UsageAggregationInterval, cfg.UsageLookbackHours, logger, apiMetrics)
	}()
	go func() { errCh <- observeOutboxDepth(ctx, outboxRepository, apiMetrics.OutboxPending, logger) }()
	go func() { errCh <- httpServer.ListenAndServe() }()
	go func() { errCh <- metricsServer.ListenAndServe() }()
	logger.Info("InferScale API started", "http_addr", cfg.HTTPAddr, "metrics_addr", cfg.MetricsAddr)

	select {
	case <-ctx.Done():
	case runErr := <-errCh:
		if runErr != nil && !errors.Is(runErr, http.ErrServerClosed) && !errors.Is(runErr, context.Canceled) {
			stop()
			return runErr
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	return errors.Join(httpServer.Shutdown(shutdownCtx), metricsServer.Shutdown(shutdownCtx))
}

type pendingOutboxCounter interface {
	PendingCount(context.Context) (int64, error)
}

func observeOutboxDepth(ctx context.Context, repository pendingOutboxCounter, gauge prometheus.Gauge, logger interface {
	Error(string, ...any)
}) error {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	observe := func() {
		count, err := repository.PendingCount(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logger.Error("query pending outbox depth", "error", err)
			}
			return
		}
		gauge.Set(float64(count))
	}
	observe()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			observe()
		}
	}
}
