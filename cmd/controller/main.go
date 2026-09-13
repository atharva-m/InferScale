package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/benchmark"
	"github.com/inferscale/inferscale/internal/config"
	controllerdeployment "github.com/inferscale/inferscale/internal/controller/deployment"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	platformmetrics "github.com/inferscale/inferscale/internal/metrics"
	"github.com/inferscale/inferscale/internal/observability"
	"github.com/inferscale/inferscale/internal/rollout"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	"github.com/inferscale/inferscale/internal/runtime/fake"
	"github.com/inferscale/inferscale/internal/runtime/trtllm"
	"github.com/inferscale/inferscale/internal/runtime/vllm"
	"github.com/inferscale/inferscale/internal/storage/postgres"
	"github.com/inferscale/inferscale/internal/tenant"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
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
	if err := cfg.ValidateController(); err != nil {
		return err
	}
	if cfg.DatabaseURL == "" {
		return fmt.Errorf("INFERSCALE_DATABASE_URL is required by the controller")
	}
	store, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open controller PostgreSQL connection: %w", err)
	}
	defer store.Close()
	logger := observability.NewLogger(cfg.LogLevel)
	ctrl.SetLogger(logr.FromSlogHandler(logger.Handler()))
	shutdownTracing, err := observability.InitTracing(ctx, "inferscale-controller", cfg.OTLPEndpoint)
	if err != nil {
		return fmt.Errorf("initialize tracing: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	restConfig, err := kubeutil.RESTConfig(cfg.Kubeconfig)
	if err != nil {
		return err
	}
	manager, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                        scheme,
		Metrics:                       metricsserver.Options{BindAddress: cfg.MetricsAddr},
		HealthProbeBindAddress:        cfg.HTTPAddr,
		LeaderElection:                true,
		LeaderElectionID:              "inferscale-controller.platform.inferscale.io",
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		return fmt.Errorf("create controller manager: %w", err)
	}
	ctrlmetrics.Registry.MustRegister(observability.NewDeploymentCollector(manager.GetClient()))
	registry, err := platformruntime.NewRegistry(runtimeAdapters(cfg)...)
	if err != nil {
		return err
	}
	deploymentRepository := postgres.NewDeploymentRepository(store)
	runtimeIdentities := map[benchmark.Backend]benchmark.RuntimeIdentity{
		benchmark.BackendVLLM: {
			BackendVersion:     cfg.VLLMVersion,
			RuntimeImageDigest: controllerdeployment.RuntimeImageIdentity(cfg.VLLMImage),
		},
	}
	if cfg.Features.TensorRTLLM {
		runtimeIdentities[benchmark.BackendTensorRTLLM] = benchmark.RuntimeIdentity{
			BackendVersion:     cfg.TRTLLMVersion,
			RuntimeImageDigest: controllerdeployment.RuntimeImageIdentity(cfg.TRTLLMImage),
		}
	}
	profileSource := controllerdeployment.ApprovedProfileSource{
		Repository:              postgres.NewRuntimeProfileRepository(store),
		DriverCUDAFingerprint:   cfg.DriverCUDAFingerprint,
		SelectionScenarioDigest: cfg.SelectionScenarioDigest,
		ExpectedRuntimes:        runtimeIdentities,
	}
	prometheusClient, err := platformmetrics.NewPrometheusClient(cfg.PrometheusURL, nil)
	if err != nil {
		return fmt.Errorf("initialize rollout Prometheus client: %w", err)
	}
	reconciler := &controllerdeployment.Reconciler{
		Client:   manager.GetClient(),
		Scheme:   manager.GetScheme(),
		Registry: registry,
		Resolver: controllerdeployment.MeasuredBackendResolver{
			Source: profileSource, Store: deploymentRepository,
			ExpectedRuntimes: runtimeIdentities,
			AutoEnabled:      cfg.Features.BackendAuto,
			TensorRTEnabled:  cfg.Features.TensorRTLLM,
		},
		Projector: deploymentRepository,
		RolloutMetrics: rollout.PrometheusProvider{
			Client: prometheusClient, Kubernetes: manager.GetClient(),
			Window: time.Minute, Freshness: 2 * time.Minute,
		},
		Config: controllerdeployment.Config{
			Images: platformruntime.Images{
				VLLM:           cfg.VLLMImage,
				TensorRTLLM:    cfg.TRTLLMImage,
				TensorRTBuild:  cfg.TRTLLMImage,
				ModelPrefetch:  cfg.PrefetchImage,
				EndpointPicker: cfg.EPPImage,
			},
			ControlNamespace:      cfg.ControlNamespace,
			ModelCacheRoot:        cfg.ModelCacheRoot,
			EngineCacheRoot:       cfg.EngineCacheRoot,
			ImagePullSecret:       cfg.ImagePullSecret,
			HFTokenSecret:         cfg.HFTokenSecret,
			RuntimeServiceAccount: tenant.RuntimeServiceAccount,
			GatewayName:           cfg.GatewayName,
			GatewayNamespace:      cfg.GatewayNamespace,
			MonitoringNamespace:   cfg.MonitoringNamespace,
			PrometheusURL:         cfg.PrometheusURL,
			OTLPEndpoint:          cfg.OTLPEndpoint,
			PublicBaseURL:         cfg.PublicBaseURL,
			GPUNodeSelectors:      cfg.GPUNodeSelectors,
			ProgressiveRollout:    cfg.Features.ProgressiveRollout,
			ScaleToZero:           cfg.Features.ScaleToZero,
			BackendAutoEnabled:    cfg.Features.BackendAuto,
			TensorRTEnabled:       cfg.Features.TensorRTLLM,
		},
	}
	if err := reconciler.SetupWithManager(manager); err != nil {
		return fmt.Errorf("register InferenceDeployment controller: %w", err)
	}
	if err := manager.AddHealthzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	if err := manager.AddReadyzCheck("ping", healthz.Ping); err != nil {
		return err
	}
	logger.Info(
		"InferScale controller started",
		"health_addr", cfg.HTTPAddr,
		"metrics_addr", cfg.MetricsAddr,
		"local_fake_runtime", cfg.FakeRuntime,
	)
	if err := manager.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// runtimeAdapters keeps the fake server behind a controller-process setting.
// It replaces the vLLM renderer while retaining the public vLLM backend value,
// so neither the API nor the CRD gains a development-only backend contract.
func runtimeAdapters(cfg config.Config) []platformruntime.Adapter {
	vllmAdapter := platformruntime.Adapter(vllm.New())
	if cfg.FakeRuntime {
		vllmAdapter = fake.New()
	}
	return []platformruntime.Adapter{vllmAdapter, trtllm.New()}
}
