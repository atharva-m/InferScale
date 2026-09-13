package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("INFERSCALE_HTTP_ADDR", "")
	t.Setenv("INFERSCALE_GRPC_ADDR", "")
	t.Setenv("INFERSCALE_FEATURE_SCALE_TO_ZERO", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddr != ":8080" || cfg.GRPCAddr != ":9001" {
		t.Fatalf("unexpected default addresses: %#v", cfg)
	}
	if cfg.Features.ScaleToZero {
		t.Fatal("scale-to-zero must default off")
	}
}

func TestLoadRejectsInvalidFeature(t *testing.T) {
	t.Setenv("INFERSCALE_FEATURE_TRTLLM", "sometimes")
	if _, err := Load(); err == nil {
		t.Fatal("Load() expected an error")
	}
}

func TestFakeRuntimeIsControllerScopedAndDevelopmentOnly(t *testing.T) {
	t.Setenv("INFERSCALE_FAKE_RUNTIME", "true")
	t.Setenv("INFERSCALE_ENV", "local-wsl")
	t.Setenv("INFERSCALE_VLLM_IMAGE", "")
	cfg, err := Load()
	if err != nil || !cfg.FakeRuntime {
		t.Fatalf("Load() cfg=%+v err=%v", cfg, err)
	}
	if err := cfg.ValidateController(); err == nil || !strings.Contains(err.Error(), "INFERSCALE_VLLM_IMAGE") {
		t.Fatalf("controller without a fake runtime image error=%v", err)
	}

	cfg.VLLMImage = "ghcr.io/inferscale/fake-runtime:0.1.0-dev"
	cfg.EPPImage = "ghcr.io/llm-d/epp:local"
	cfg.PublicBaseURL = "http://127.0.0.1:8080"
	cfg.OTLPEndpoint = "http://otel:4317"
	if err := cfg.ValidateController(); err != nil {
		t.Fatalf("complete local fake controller config was rejected: %v", err)
	}

	cfg.Environment = "remote-benchmark"
	cfg.GPUNodeSelectors = map[string]map[string]string{"RTX_5090": {"inferscale.io/gpu-sku": "RTX_5090"}}
	if err := cfg.ValidateController(); err == nil || !strings.Contains(err.Error(), "fake runtime") {
		t.Fatalf("remote fake runtime controller error=%v", err)
	}
}

func TestLoadRejectsEveryPresentMutableRemoteImage(t *testing.T) {
	for _, variable := range []string{
		"INFERSCALE_VLLM_IMAGE",
		"INFERSCALE_TRTLLM_IMAGE",
		"INFERSCALE_PREFETCH_IMAGE",
		"INFERSCALE_EPP_IMAGE",
		"INFERSCALE_BENCHMARK_IMAGE",
	} {
		t.Run(variable, func(t *testing.T) {
			t.Setenv("INFERSCALE_ENV", "remote-benchmark")
			t.Setenv(variable, "ghcr.io/inferscale/component:1.0.0")
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), variable) {
				t.Fatalf("Load() error=%v, want mutable %s rejection", err, variable)
			}
		})
	}
}

func TestImmutableImageReference(t *testing.T) {
	digest := "ghcr.io/inferscale/benchmark@sha256:" + strings.Repeat("a", 64)
	if !immutableImageReference(digest) {
		t.Fatal("valid digest reference was rejected")
	}
	if immutableImageReference("ghcr.io/inferscale/benchmark:1.0.0") {
		t.Fatal("tag-only image was accepted")
	}
}

func TestRemoteEnvironmentRequiresVerifiedGPUInventoryMapping(t *testing.T) {
	t.Parallel()
	cfg := Config{
		Environment: "remote-benchmark", HTTPAddr: ":8080", GRPCAddr: ":9001",
		ControlNamespace: "system", GatewayNamespace: "gateway", GatewayName: "gateway",
		ShutdownTimeout: time.Second, BenchmarkPollInterval: time.Second,
		UsageAggregationInterval: time.Minute, UsageLookbackHours: 1,
		VLLMImage:     "registry.example/vllm@sha256:" + strings.Repeat("a", 64),
		EPPImage:      "registry.example/epp@sha256:" + strings.Repeat("b", 64),
		PrefetchImage: "registry.example/modelcache@sha256:" + strings.Repeat("c", 64),
		PublicBaseURL: "https://inference.example", OTLPEndpoint: "http://otel:4317",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("general process validation depends on controller inventory: %v", err)
	}
	if err := cfg.ValidateController(); err == nil || !strings.Contains(err.Error(), "GPU_NODE_SELECTORS") {
		t.Fatalf("ValidateController() error=%v, want missing inventory mapping", err)
	}
	cfg.GPUNodeSelectors = map[string]map[string]string{"RTX_5090": {"inferscale.io/gpu-sku": "RTX_5090"}}
	if err := cfg.ValidateController(); err != nil {
		t.Fatalf("ValidateController() rejected verified inventory mapping: %v", err)
	}
}

func TestControllerValidationRequiresRenderableImages(t *testing.T) {
	base := Config{
		Environment: "development",
		VLLMImage:   "vllm", EPPImage: "epp", PrefetchImage: "prefetch",
		PublicBaseURL: "http://127.0.0.1:8080", OTLPEndpoint: "http://otel:4317",
	}
	if err := base.ValidateController(); err != nil {
		t.Fatalf("complete controller config was rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"vllm":     func(config *Config) { config.VLLMImage = "" },
		"epp":      func(config *Config) { config.EPPImage = "" },
		"prefetch": func(config *Config) { config.PrefetchImage = "" },
	} {
		t.Run(name, func(t *testing.T) {
			config := base
			mutate(&config)
			if err := config.ValidateController(); err == nil {
				t.Fatal("missing controller image was accepted")
			}
		})
	}

	fake := base
	fake.FakeRuntime = true
	fake.PrefetchImage = ""
	if err := fake.ValidateController(); err != nil {
		t.Fatalf("local fake runtime unnecessarily requires prefetch image: %v", err)
	}

	trt := base
	trt.Features.TensorRTLLM = true
	if err := trt.ValidateController(); err == nil || !strings.Contains(err.Error(), "TRTLLM_IMAGE") {
		t.Fatalf("TensorRT controller without image error=%v", err)
	}
}

func TestRemotePublicOriginRequiresHTTPSAndNoCredentials(t *testing.T) {
	base := Config{
		Environment: "remote-benchmark", HTTPAddr: ":8080", GRPCAddr: ":9001",
		ControlNamespace: "system", GatewayNamespace: "gateway", GatewayName: "gateway",
		ShutdownTimeout: time.Second, BenchmarkPollInterval: time.Second,
		UsageAggregationInterval: time.Minute, UsageLookbackHours: 1,
		GPUNodeSelectors: map[string]map[string]string{"RTX_5090": {"inferscale.io/gpu-sku": "RTX_5090"}},
	}
	for _, origin := range []string{
		"http://inference.example",
		"https://user:secret@inference.example",
		"https://inference.example/base/path",
		"https://inference.example?secret=value",
		"https://inference.example?",
		"https://inference.example#",
		"https://:443",
		"https://inference.example:0",
		"https://inference.example:65536",
		" https://inference.example",
	} {
		config := base
		config.PublicBaseURL = origin
		if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "PUBLIC_BASE_URL") {
			t.Fatalf("Validate() accepted unsafe remote origin %q: %v", origin, err)
		}
	}
	base.PublicBaseURL = "https://inference.example:8443"
	if err := base.Validate(); err != nil {
		t.Fatalf("Validate() rejected safe remote origin: %v", err)
	}
}

func TestBackendAutoIdentityIsControllerScoped(t *testing.T) {
	cfg := Config{
		Environment: "local-wsl", HTTPAddr: ":8080", GRPCAddr: ":9001",
		ControlNamespace: "system", GatewayNamespace: "gateway", GatewayName: "gateway",
		ShutdownTimeout: time.Second, BenchmarkPollInterval: time.Second,
		UsageAggregationInterval: time.Minute, UsageLookbackHours: 1,
		Features: Features{BackendAuto: true},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("general process validation depends on controller backend identity: %v", err)
	}
	if err := cfg.ValidateController(); err == nil || !strings.Contains(err.Error(), "backend:auto requires") {
		t.Fatalf("ValidateController() error=%v, want exact backend:auto identity rejection", err)
	}

	cfg.DriverCUDAFingerprint = strings.Repeat("d", 64)
	cfg.SelectionScenarioDigest = strings.Repeat("e", 64)
	cfg.VLLMVersion = "0.23.0"
	cfg.VLLMImage = "ghcr.io/inferscale/vllm@sha256:" + strings.Repeat("a", 64)
	cfg.EPPImage = "ghcr.io/llm-d/epp@sha256:" + strings.Repeat("b", 64)
	cfg.PrefetchImage = "ghcr.io/inferscale/modelcache@sha256:" + strings.Repeat("c", 64)
	cfg.PublicBaseURL = "http://127.0.0.1:8080"
	cfg.OTLPEndpoint = "http://otel:4317"
	if err := cfg.ValidateController(); err != nil {
		t.Fatalf("ValidateController() rejected exact backend:auto identity: %v", err)
	}
}

func TestRemoteBenchmarkAuthorityIsAPIScoped(t *testing.T) {
	cfg := Config{
		Environment: "remote-benchmark", HTTPAddr: ":8080", GRPCAddr: ":9001",
		ControlNamespace: "system", GatewayNamespace: "gateway", GatewayName: "gateway",
		ShutdownTimeout: time.Second, BenchmarkPollInterval: time.Second,
		UsageAggregationInterval: time.Minute, UsageLookbackHours: 1,
		PublicBaseURL: "https://inference.example",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("general process validation depends on API benchmark authority: %v", err)
	}
	if err := cfg.ValidateAPI(); err == nil || !strings.Contains(err.Error(), "INFERSCALE_BENCHMARK_PROVIDER") {
		t.Fatalf("ValidateAPI() error=%v, want missing benchmark authority rejection", err)
	}

	cfg.BenchmarkProvider = "vast"
	cfg.BenchmarkInferenceBaseURL = "https://inference.example"
	cfg.BenchmarkPrometheusURL = "http://prometheus.monitoring.svc.cluster.local:9090"
	cfg.BenchmarkDriverVersion = "580.82.07"
	cfg.BenchmarkCUDAVersion = "13.0"
	if err := cfg.ValidateAPI(); err != nil {
		t.Fatalf("ValidateAPI() rejected complete benchmark authority: %v", err)
	}
}

func TestRemoteBenchmarkAuthorityRejectsUnsafeValues(t *testing.T) {
	base := Config{
		Environment:               "remote-benchmark",
		BenchmarkProvider:         "vast",
		BenchmarkInferenceBaseURL: "https://inference.example",
		BenchmarkPrometheusURL:    "http://prometheus.monitoring.svc.cluster.local:9090",
		BenchmarkDriverVersion:    "580.82.07",
		BenchmarkCUDAVersion:      "13.0",
	}
	for name, mutate := range map[string]func(*Config){
		"provider sentinel":      func(config *Config) { config.BenchmarkProvider = "set-at-run-time" },
		"provider whitespace":    func(config *Config) { config.BenchmarkProvider = "vast cloud" },
		"plaintext inference":    func(config *Config) { config.BenchmarkInferenceBaseURL = "http://inference.example" },
		"credentialed inference": func(config *Config) { config.BenchmarkInferenceBaseURL = "https://user:secret@inference.example" },
		"Prometheus query":       func(config *Config) { config.BenchmarkPrometheusURL = "http://prometheus:9090?token=secret" },
		"driver placeholder":     func(config *Config) { config.BenchmarkDriverVersion = "set-at-run-time" },
		"CUDA placeholder":       func(config *Config) { config.BenchmarkCUDAVersion = "set-at-run-time" },
	} {
		t.Run(name, func(t *testing.T) {
			config := base
			mutate(&config)
			if err := config.ValidateAPI(); err == nil {
				t.Fatal("ValidateAPI() accepted unsafe benchmark authority")
			}
		})
	}
}

func TestDevelopmentAPIDoesNotRequirePublishableBenchmarkAuthority(t *testing.T) {
	for _, environment := range []string{"development", "local-wsl"} {
		t.Run(environment, func(t *testing.T) {
			if err := (Config{Environment: environment}).ValidateAPI(); err != nil {
				t.Fatalf("ValidateAPI() rejected development without remote evidence: %v", err)
			}
		})
	}
}
