// Package config loads and validates process configuration from environment
// variables. It deliberately has no service-specific side effects so every
// binary can share the same validation behavior.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var (
	benchmarkProviderPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	benchmarkDriverPattern   = regexp.MustCompile(`^[0-9]{3,4}\.[0-9]{1,3}(?:\.[0-9]{1,3})?$`)
	benchmarkCUDAPattern     = regexp.MustCompile(`^[0-9]{1,2}\.[0-9]{1,2}(?:\.[0-9]+)?$`)
)

type Config struct {
	Environment                 string
	LogLevel                    string
	HTTPAddr                    string
	GRPCAddr                    string
	MetricsAddr                 string
	DatabaseURL                 string
	ValkeyAddr                  string
	ValkeyURL                   string
	KubernetesAPICIDRs          []string
	KubernetesAPIPort           int
	PrometheusURL               string
	OTLPEndpoint                string
	Kubeconfig                  string
	ControlNamespace            string
	GatewayName                 string
	GatewayNamespace            string
	MonitoringNamespace         string
	PublicBaseURL               string
	ModelCacheRoot              string
	EngineCacheRoot             string
	ImagePullSecret             string
	HFTokenSecret               string
	VLLMImage                   string
	VLLMVersion                 string
	TRTLLMImage                 string
	TRTLLMVersion               string
	PrefetchImage               string
	EPPImage                    string
	BenchmarkImage              string
	BenchmarkCallbackURL        string
	BenchmarkCallbackSigningKey string
	BenchmarkResultsClaim       string
	BenchmarkPollInterval       time.Duration
	BenchmarkProvider           string
	BenchmarkInferenceBaseURL   string
	BenchmarkPrometheusURL      string
	BenchmarkDriverVersion      string
	BenchmarkCUDAVersion        string
	UsageAggregationInterval    time.Duration
	UsageLookbackHours          int
	GPUNodeSelectors            map[string]map[string]string
	DriverCUDAFingerprint       string
	SelectionScenarioDigest     string
	FakeRuntime                 bool
	ShutdownTimeout             time.Duration
	Features                    Features
}

func Load() (Config, error) {
	cfg := Config{
		Environment:                 env("INFERSCALE_ENV", "development"),
		LogLevel:                    env("INFERSCALE_LOG_LEVEL", "info"),
		HTTPAddr:                    env("INFERSCALE_HTTP_ADDR", ":8080"),
		GRPCAddr:                    env("INFERSCALE_GRPC_ADDR", ":9001"),
		MetricsAddr:                 env("INFERSCALE_METRICS_ADDR", ":9090"),
		DatabaseURL:                 os.Getenv("INFERSCALE_DATABASE_URL"),
		ValkeyAddr:                  os.Getenv("INFERSCALE_VALKEY_ADDR"),
		ValkeyURL:                   os.Getenv("INFERSCALE_VALKEY_URL"),
		KubernetesAPIPort:           443,
		PrometheusURL:               env("INFERSCALE_PROMETHEUS_URL", "http://localhost:9090"),
		OTLPEndpoint:                os.Getenv("INFERSCALE_OTLP_ENDPOINT"),
		Kubeconfig:                  os.Getenv("INFERSCALE_KUBECONFIG"),
		ControlNamespace:            env("INFERSCALE_CONTROL_NAMESPACE", "inferscale-system"),
		GatewayName:                 env("INFERSCALE_GATEWAY_NAME", "inferscale-gateway"),
		GatewayNamespace:            env("INFERSCALE_GATEWAY_NAMESPACE", "inferscale-system"),
		MonitoringNamespace:         env("INFERSCALE_MONITORING_NAMESPACE", "inferscale-monitoring"),
		PublicBaseURL:               os.Getenv("INFERSCALE_PUBLIC_BASE_URL"),
		ModelCacheRoot:              env("INFERSCALE_MODEL_CACHE_ROOT", "/var/lib/inferscale/models"),
		EngineCacheRoot:             env("INFERSCALE_ENGINE_CACHE_ROOT", "/var/lib/inferscale/engines"),
		ImagePullSecret:             os.Getenv("INFERSCALE_IMAGE_PULL_SECRET"),
		HFTokenSecret:               os.Getenv("INFERSCALE_HF_TOKEN_SECRET"),
		VLLMImage:                   os.Getenv("INFERSCALE_VLLM_IMAGE"),
		VLLMVersion:                 env("INFERSCALE_VLLM_VERSION", "0.23.0"),
		TRTLLMImage:                 os.Getenv("INFERSCALE_TRTLLM_IMAGE"),
		TRTLLMVersion:               env("INFERSCALE_TRTLLM_VERSION", "1.0.0"),
		PrefetchImage:               os.Getenv("INFERSCALE_PREFETCH_IMAGE"),
		EPPImage:                    os.Getenv("INFERSCALE_EPP_IMAGE"),
		BenchmarkImage:              os.Getenv("INFERSCALE_BENCHMARK_IMAGE"),
		BenchmarkCallbackURL:        os.Getenv("INFERSCALE_BENCHMARK_CALLBACK_URL"),
		BenchmarkCallbackSigningKey: os.Getenv("INFERSCALE_BENCHMARK_CALLBACK_SIGNING_KEY"),
		BenchmarkResultsClaim:       os.Getenv("INFERSCALE_BENCHMARK_RESULTS_PVC"),
		BenchmarkProvider:           os.Getenv("INFERSCALE_BENCHMARK_PROVIDER"),
		BenchmarkInferenceBaseURL:   os.Getenv("INFERSCALE_BENCHMARK_INFERENCE_BASE_URL"),
		BenchmarkPrometheusURL:      os.Getenv("INFERSCALE_BENCHMARK_PROMETHEUS_URL"),
		BenchmarkDriverVersion:      os.Getenv("INFERSCALE_BENCHMARK_DRIVER_VERSION"),
		BenchmarkCUDAVersion:        os.Getenv("INFERSCALE_BENCHMARK_CUDA_VERSION"),
		BenchmarkPollInterval:       5 * time.Second,
		UsageAggregationInterval:    5 * time.Minute,
		UsageLookbackHours:          2,
		DriverCUDAFingerprint:       os.Getenv("INFERSCALE_DRIVER_CUDA_FINGERPRINT"),
		SelectionScenarioDigest:     os.Getenv("INFERSCALE_SELECTION_SCENARIO_DIGEST"),
		ShutdownTimeout:             20 * time.Second,
	}
	if cfg.BenchmarkCallbackURL == "" {
		cfg.BenchmarkCallbackURL = "http://inferscale-api." + cfg.ControlNamespace + ".svc.cluster.local:8080"
	}
	if cfg.BenchmarkInferenceBaseURL == "" {
		cfg.BenchmarkInferenceBaseURL = cfg.PublicBaseURL
	}
	if cfg.BenchmarkPrometheusURL == "" {
		cfg.BenchmarkPrometheusURL = cfg.PrometheusURL
	}

	var err error
	if raw := os.Getenv("INFERSCALE_KUBERNETES_API_CIDRS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.KubernetesAPICIDRs); err != nil {
			return Config{}, errors.New("INFERSCALE_KUBERNETES_API_CIDRS must be a JSON array of API endpoint CIDRs")
		}
	}
	if raw := os.Getenv("INFERSCALE_KUBERNETES_API_PORT"); raw != "" {
		cfg.KubernetesAPIPort, err = strconv.Atoi(raw)
		if err != nil || cfg.KubernetesAPIPort < 1 || cfg.KubernetesAPIPort > 65535 {
			return Config{}, errors.New("INFERSCALE_KUBERNETES_API_PORT must be an integer")
		}
	}
	if raw := os.Getenv("INFERSCALE_SHUTDOWN_TIMEOUT"); raw != "" {
		cfg.ShutdownTimeout, err = time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("parse INFERSCALE_SHUTDOWN_TIMEOUT: %w", err)
		}
	}
	if raw := os.Getenv("INFERSCALE_BENCHMARK_POLL_INTERVAL"); raw != "" {
		cfg.BenchmarkPollInterval, err = time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("parse INFERSCALE_BENCHMARK_POLL_INTERVAL: %w", err)
		}
	}
	if raw := os.Getenv("INFERSCALE_USAGE_AGGREGATION_INTERVAL"); raw != "" {
		cfg.UsageAggregationInterval, err = time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("parse INFERSCALE_USAGE_AGGREGATION_INTERVAL: %w", err)
		}
	}
	if raw := os.Getenv("INFERSCALE_USAGE_LOOKBACK_HOURS"); raw != "" {
		cfg.UsageLookbackHours, err = strconv.Atoi(raw)
		if err != nil {
			return Config{}, fmt.Errorf("parse INFERSCALE_USAGE_LOOKBACK_HOURS: %w", err)
		}
	}
	if cfg.Features, err = LoadFeatures(); err != nil {
		return Config{}, err
	}
	if cfg.FakeRuntime, err = envBool("INFERSCALE_FAKE_RUNTIME", false); err != nil {
		return Config{}, err
	}
	if raw := os.Getenv("INFERSCALE_GPU_NODE_SELECTORS"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &cfg.GPUNodeSelectors); err != nil {
			return Config{}, fmt.Errorf("parse INFERSCALE_GPU_NODE_SELECTORS: %w", err)
		}
	}
	return cfg, cfg.Validate()
}

// Validate checks configuration that is meaningful to every executable. The
// deployable services share one ConfigMap, so controller-only feature gates may
// be present in API/admission environments even though those processes neither
// receive nor consume runtime images, GPU inventory, or selection identities.
func (c Config) Validate() error {
	var problems []string
	if c.KubernetesAPIPort < 0 || c.KubernetesAPIPort > 65535 {
		problems = append(problems, "INFERSCALE_KUBERNETES_API_PORT must be between 1 and 65535")
	}
	if c.KubernetesAPIPort != 0 && c.KubernetesAPIPort != 443 && len(c.KubernetesAPICIDRs) == 0 {
		problems = append(problems, "INFERSCALE_KUBERNETES_API_CIDRS is required for a non-default API port")
	}
	for _, raw := range c.KubernetesAPICIDRs {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Bits() != prefix.Addr().BitLen() || !prefix.Addr().IsGlobalUnicast() || prefix.String() != raw {
			problems = append(problems, "INFERSCALE_KUBERNETES_API_CIDRS must contain canonical /32 IPv4 or /128 IPv6 API endpoint addresses")
			break
		}
	}
	if c.HTTPAddr == "" {
		problems = append(problems, "HTTP address is required")
	}
	if c.GRPCAddr == "" {
		problems = append(problems, "gRPC address is required")
	}
	if c.ControlNamespace == "" || c.GatewayNamespace == "" || c.GatewayName == "" {
		problems = append(problems, "Kubernetes namespaces and gateway name are required")
	}
	if c.PublicBaseURL != "" {
		parsed, err := url.Parse(c.PublicBaseURL)
		validPort := true
		if err == nil {
			if port := parsed.Port(); port != "" {
				value, portErr := strconv.Atoi(port)
				validPort = portErr == nil && value >= 1 && value <= 65535
			}
		}
		if err != nil || c.PublicBaseURL != strings.TrimSpace(c.PublicBaseURL) || strings.ContainsAny(c.PublicBaseURL, "?#") ||
			parsed.Host == "" || parsed.Hostname() == "" || strings.HasSuffix(parsed.Host, ":") || !validPort ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.Opaque != "" ||
			(parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			problems = append(problems, "INFERSCALE_PUBLIC_BASE_URL must be an absolute HTTP(S) origin without credentials, query, or fragment")
		} else if c.Environment != "development" && c.Environment != "local-wsl" && parsed.Scheme != "https" {
			problems = append(problems, "INFERSCALE_PUBLIC_BASE_URL must use HTTPS outside development")
		}
	}
	for sku, selector := range c.GPUNodeSelectors {
		if strings.TrimSpace(sku) == "" || len(selector) == 0 {
			problems = append(problems, "GPU node selector entries require a SKU and at least one label")
			break
		}
	}
	if c.ShutdownTimeout <= 0 {
		problems = append(problems, "shutdown timeout must be positive")
	}
	if c.BenchmarkPollInterval <= 0 {
		problems = append(problems, "benchmark poll interval must be positive")
	}
	if c.UsageAggregationInterval <= 0 {
		problems = append(problems, "usage aggregation interval must be positive")
	}
	if c.UsageLookbackHours < 1 || c.UsageLookbackHours > 24 {
		problems = append(problems, "usage lookback hours must be between 1 and 24")
	}
	if c.Environment != "development" && c.Environment != "local-wsl" {
		for name, image := range map[string]string{
			"INFERSCALE_VLLM_IMAGE":      c.VLLMImage,
			"INFERSCALE_TRTLLM_IMAGE":    c.TRTLLMImage,
			"INFERSCALE_PREFETCH_IMAGE":  c.PrefetchImage,
			"INFERSCALE_EPP_IMAGE":       c.EPPImage,
			"INFERSCALE_BENCHMARK_IMAGE": c.BenchmarkImage,
		} {
			if image != "" && !immutableImageReference(image) {
				problems = append(problems, name+" must use an immutable @sha256 digest outside development")
			}
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// ValidateController rejects a process configuration that can start a healthy
// controller but cannot render the first serving deployment. It is separate
// from Validate because the API and admission services intentionally do not
// need runtime image credentials.
func (c Config) ValidateController() error {
	var problems []string
	for name, image := range map[string]string{
		"INFERSCALE_VLLM_IMAGE": c.VLLMImage,
		"INFERSCALE_EPP_IMAGE":  c.EPPImage,
	} {
		if strings.TrimSpace(image) == "" {
			problems = append(problems, name+" is required by the controller")
		}
	}
	if strings.TrimSpace(c.PublicBaseURL) == "" {
		problems = append(problems, "INFERSCALE_PUBLIC_BASE_URL is required by the controller")
	}
	if strings.TrimSpace(c.OTLPEndpoint) == "" {
		problems = append(problems, "INFERSCALE_OTLP_ENDPOINT is required because v1 tracing defaults on")
	}
	if !c.FakeRuntime && strings.TrimSpace(c.PrefetchImage) == "" {
		problems = append(problems, "INFERSCALE_PREFETCH_IMAGE is required by the controller")
	}
	if c.Features.TensorRTLLM && strings.TrimSpace(c.TRTLLMImage) == "" {
		problems = append(problems, "INFERSCALE_TRTLLM_IMAGE is required when TensorRT-LLM is enabled")
	}
	if c.Environment != "development" && c.Environment != "local-wsl" {
		if len(c.GPUNodeSelectors) == 0 {
			problems = append(problems, "INFERSCALE_GPU_NODE_SELECTORS must map every admitted remote GPU SKU to verified node labels")
		}
		// The v1 model and engine caches are node-local. A SKU label alone may
		// match several machines: one successful prefetch cannot establish that
		// another machine has the same bytes, and cache repair is not a
		// multi-node protocol. Keep every configured SKU on one chosen host.
		selectedHost := ""
		for sku, selector := range c.GPUNodeSelectors {
			hostname := selector["kubernetes.io/hostname"]
			if hostname == "" || strings.TrimSpace(hostname) != hostname {
				problems = append(problems, fmt.Sprintf("INFERSCALE_GPU_NODE_SELECTORS entry %q must include the verified kubernetes.io/hostname for the single v1 GPU host", sku))
				continue
			}
			if selectedHost != "" && selectedHost != hostname {
				problems = append(problems, "INFERSCALE_GPU_NODE_SELECTORS must select the same kubernetes.io/hostname for every SKU; v1 does not replicate caches across GPU hosts")
				break
			}
			selectedHost = hostname
		}
	}
	if c.Features.BackendAuto {
		if strings.TrimSpace(c.DriverCUDAFingerprint) == "" || strings.TrimSpace(c.SelectionScenarioDigest) == "" {
			problems = append(problems, "backend:auto requires exact driver/CUDA and selection-scenario digests")
		}
		if strings.TrimSpace(c.VLLMVersion) == "" || !immutableImageReference(c.VLLMImage) {
			problems = append(problems, "backend:auto requires a vLLM version and digest-pinned image")
		}
		if c.Features.TensorRTLLM && (strings.TrimSpace(c.TRTLLMVersion) == "" || !immutableImageReference(c.TRTLLMImage)) {
			problems = append(problems, "backend:auto with TensorRT-LLM requires a TensorRT-LLM version and digest-pinned image")
		}
	}
	if c.FakeRuntime {
		if c.Environment != "development" && c.Environment != "local-wsl" {
			problems = append(problems, "the CPU fake runtime is permitted only in development or local-wsl")
		}
		if c.Features.BackendAuto || c.Features.TensorRTLLM {
			problems = append(problems, "the CPU fake runtime cannot be combined with backend:auto or TensorRT-LLM")
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// ValidateAPI checks the operator-owned evidence required by the remote
// benchmark scheduler. Development keeps this optional so the local control
// plane can exercise API/outbox/inference behavior without claiming publishable
// GPU evidence. Other services deliberately do not call this method even though
// they consume the shared ConfigMap.
func (c Config) ValidateAPI() error {
	if c.Environment == "development" || c.Environment == "local-wsl" {
		return nil
	}

	var problems []string
	if !benchmarkProviderPattern.MatchString(c.BenchmarkProvider) || c.BenchmarkProvider == "set-at-run-time" {
		problems = append(problems, "INFERSCALE_BENCHMARK_PROVIDER must be a lowercase provider identifier")
	}
	if !validBenchmarkOrigin(c.BenchmarkInferenceBaseURL, false) {
		problems = append(problems, "INFERSCALE_BENCHMARK_INFERENCE_BASE_URL must be an HTTPS origin without credentials, query, or fragment")
	}
	if !validBenchmarkOrigin(c.BenchmarkPrometheusURL, true) {
		problems = append(problems, "INFERSCALE_BENCHMARK_PROMETHEUS_URL must be an HTTP(S) origin without credentials, query, or fragment")
	}
	if !benchmarkDriverPattern.MatchString(c.BenchmarkDriverVersion) {
		problems = append(problems, "INFERSCALE_BENCHMARK_DRIVER_VERSION must be an explicit NVIDIA driver version")
	}
	if !benchmarkCUDAPattern.MatchString(c.BenchmarkCUDAVersion) {
		problems = append(problems, "INFERSCALE_BENCHMARK_CUDA_VERSION must be an explicit CUDA version")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func validBenchmarkOrigin(raw string, allowHTTP bool) bool {
	parsed, err := url.Parse(raw)
	if err != nil || raw != strings.TrimSpace(raw) || strings.ContainsAny(raw, "?#") ||
		parsed.Host == "" || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" ||
		strings.HasSuffix(parsed.Host, ":") || strings.ContainsAny(parsed.Host, `\%`) ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	for _, character := range raw {
		if character <= 31 || character == 127 || unicode.IsSpace(character) {
			return false
		}
	}
	if parsed.Scheme != "https" && (!allowHTTP || parsed.Scheme != "http") {
		return false
	}
	if port := parsed.Port(); port != "" {
		value, portErr := strconv.Atoi(port)
		if portErr != nil || value < 1 || value > 65535 {
			return false
		}
	}
	return true
}

func immutableImageReference(value string) bool {
	const marker = "@sha256:"
	index := strings.LastIndex(value, marker)
	if index <= 0 || len(value[index+len(marker):]) != 64 {
		return false
	}
	for _, character := range value[index+len(marker):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) (bool, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("parse %s: %w", name, err)
	}
	return value, nil
}
