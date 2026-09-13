package gateway_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferscale/inferscale/internal/routing"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	goyaml "go.yaml.in/yaml/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	yaml "sigs.k8s.io/yaml"
)

// These checks prove that InferScale renders the fields required by the
// pinned integration contract. They deliberately do not claim implementation
// conformance by Envoy Gateway or llm-d; the live suite records that evidence.
func TestStaticInferenceRoutingContract(t *testing.T) {
	t.Parallel()
	renderer := routing.Renderer{Config: routing.Config{
		EndpointPickerImage: "ghcr.io/llm-d/epp@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		GatewayName:         "inferscale",
		GatewayNamespace:    "inferscale-gateway",
		MonitoringNamespace: "inferscale-monitoring",
	}}
	spec := platformruntime.Spec{
		DeploymentName: "chat", Namespace: "tenant-a", Tenant: "tenant-a", ModelName: "chat",
		ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ResolvedBackend: platformruntime.BackendVLLM, Precision: "bf16", Quantization: "none",
		TensorParallel: 1, AcceleratorCount: 1, MaxModelLen: 8192, PrefixCaching: true,
		MaxConcurrentRequests: 8, MaxQueuedRequests: 16, RoutingPolicy: "prefix-aware",
	}
	stableRevision := platformruntime.Revision{Name: "chat-stable"}
	candidateRevision := platformruntime.Revision{Name: "chat-candidate"}
	stable, err := renderer.RenderRevision(spec, stableRevision, "stable-runtime")
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := renderer.RenderRevision(spec, candidateRevision, "candidate-runtime")
	if err != nil {
		t.Fatal(err)
	}
	assertInferencePoolContract(t, stable.Objects)
	assertInferencePoolContract(t, candidate.Objects)

	route, err := renderer.RenderRoute(routing.RouteConfig{
		DeploymentName: "chat", Namespace: "tenant-a", GatewayName: "inferscale", GatewayNamespace: "inferscale-gateway",
		Path: "/v1/deployments/0198b9b7-22f0-7c6d-8f2e-7d1f9b9f1420/chat/completions",
		Stable: &routing.RouteRevision{
			Revision: stableRevision, Names: stable.Names, ServiceName: "stable-runtime", Weight: 75,
		},
		Candidate: &routing.RouteRevision{
			Revision: candidateRevision, Names: candidate.Names, ServiceName: "candidate-runtime", Weight: 25,
		},
		Shadow: true, ShadowPercent: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if route.GetAPIVersion() != "gateway.networking.k8s.io/v1" {
		t.Fatalf("HTTPRoute apiVersion=%q", route.GetAPIVersion())
	}
	rules, found, err := unstructured.NestedSlice(route.Object, "spec", "rules")
	if err != nil || !found || len(rules) != 4 {
		t.Fatalf("route rules=%d found=%v err=%v", len(rules), found, err)
	}
	for index, rawRule := range rules {
		rule := rawRule.(map[string]any)
		backends := rule["backendRefs"].([]any)
		if len(backends) != 2 {
			t.Fatalf("rule %d backend count=%d", index, len(backends))
		}
		assertPoolBackend(t, backends[0].(map[string]any), int64(75))
		assertPoolBackend(t, backends[1].(map[string]any), int64(25))
		filters := rule["filters"].([]any)
		if !hasFilter(filters, "URLRewrite") || !hasFilter(filters, "RequestHeaderModifier") {
			t.Errorf("rule %d omits URL rewrite or trusted-header cleanup", index)
		}
		mirror := filter(filters, "RequestMirror")
		if mirror == nil {
			t.Errorf("rule %d omits candidate shadow mirror", index)
			continue
		}
		mirrorConfig := mirror["requestMirror"].(map[string]any)
		if mirrorConfig["percent"] != int64(10) {
			t.Errorf("rule %d mirror percentage=%v", index, mirrorConfig["percent"])
		}
		backend := mirrorConfig["backendRef"].(map[string]any)
		if backend["kind"] != "Service" || backend["name"] != "candidate-runtime" {
			t.Errorf("rule %d shadow backend=%v", index, backend)
		}
	}
}

func TestStaticExternalAuthorizationContract(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "deploy/base/gateway/security-policy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var policy map[string]any
	if err := yaml.Unmarshal(content, &policy); err != nil {
		t.Fatal(err)
	}
	spec := policy["spec"].(map[string]any)
	extAuth := spec["extAuth"].(map[string]any)
	if extAuth["failOpen"] != false || extAuth["statusOnError"] != float64(503) {
		t.Fatalf("external authorization is not fail-closed: %v", extAuth)
	}
	body := extAuth["bodyToExtAuth"].(map[string]any)
	if body["maxRequestBytes"] != float64(1<<20) {
		t.Fatalf("external auth body cap=%v", body["maxRequestBytes"])
	}
	grpc := extAuth["grpc"].(map[string]any)
	refs := grpc["backendRefs"].([]any)
	backend := refs[0].(map[string]any)
	if backend["name"] != "inferscale-admission" || backend["port"] != float64(9001) {
		t.Fatalf("external authorization backend=%v", backend)
	}
}

func TestPublicGatewayDoesNotExposeDependencyReadiness(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile(filepath.Join(repositoryRoot(t), "deploy/base/gateway/routes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if strings.Contains(text, "value: /readyz") {
		t.Fatal("public Gateway must not route dependency readiness details")
	}
	if !strings.Contains(text, "value: /healthz") {
		t.Fatal("public liveness route is missing")
	}
}

func TestStaticGatewayTraceExportContract(t *testing.T) {
	t.Parallel()
	root := repositoryRoot(t)
	content, err := os.ReadFile(filepath.Join(root, "deploy/base/gateway/gateway.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	decoder := goyaml.NewDecoder(bytes.NewReader(content))
	var gatewayClass, envoyProxy map[string]any
	for {
		var document map[string]any
		if err := decoder.Decode(&document); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
		switch document["kind"] {
		case "GatewayClass":
			gatewayClass = document
		case "EnvoyProxy":
			envoyProxy = document
		}
	}
	if gatewayClass == nil || envoyProxy == nil {
		t.Fatal("GatewayClass or EnvoyProxy tracing configuration is missing")
	}
	parameters := gatewayClass["spec"].(map[string]any)["parametersRef"].(map[string]any)
	if parameters["name"] != "inferscale-tracing" || parameters["namespace"] != "inferscale-gateway" {
		t.Fatalf("GatewayClass tracing parameters=%v", parameters)
	}
	providerConfig := envoyProxy["spec"].(map[string]any)["provider"].(map[string]any)
	if providerConfig["type"] != "Kubernetes" {
		t.Fatalf("proxy infrastructure provider=%v", providerConfig)
	}
	envoyContainer := providerConfig["kubernetes"].(map[string]any)["envoyDeployment"].(map[string]any)["container"].(map[string]any)
	lockContent, err := os.ReadFile(filepath.Join(root, "versions.lock.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var lock map[string]any
	if err := yaml.Unmarshal(lockContent, &lock); err != nil {
		t.Fatal(err)
	}
	envoyLock := lock["components"].(map[string]any)["envoyGateway"].(map[string]any)
	wantEnvoyImage := envoyLock["dataPlaneImage"].(string) + "@" + envoyLock["dataPlaneImageDigest"].(string)
	if envoyContainer["image"] != wantEnvoyImage {
		t.Fatalf("generated Envoy data-plane image=%v, want locked %q", envoyContainer["image"], wantEnvoyImage)
	}
	tracing := envoyProxy["spec"].(map[string]any)["telemetry"].(map[string]any)["tracing"].(map[string]any)
	provider := tracing["provider"].(map[string]any)
	if provider["type"] != "OpenTelemetry" {
		t.Fatalf("proxy trace provider=%v", provider)
	}
	backend := provider["backendRefs"].([]any)[0].(map[string]any)
	if backend["name"] != "otel-collector" || backend["namespace"] != "inferscale-monitoring" || backend["port"] != 4317 {
		t.Fatalf("proxy trace backend=%v", backend)
	}
	encoded := string(content)
	if strings.Contains(strings.ToLower(encoded), "authorization") || strings.Contains(strings.ToLower(encoded), "request.body") {
		t.Fatal("proxy trace configuration includes a credential or request-body tag")
	}
	grantContent, err := os.ReadFile(filepath.Join(root, "deploy/base/monitoring/reference-grant.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var grant map[string]any
	if err := yaml.Unmarshal(grantContent, &grant); err != nil {
		t.Fatal(err)
	}
	grantSpec := grant["spec"].(map[string]any)
	from := grantSpec["from"].([]any)[0].(map[string]any)
	to := grantSpec["to"].([]any)[0].(map[string]any)
	if from["group"] != "gateway.envoyproxy.io" || from["kind"] != "EnvoyProxy" ||
		from["namespace"] != "inferscale-gateway" {
		t.Fatalf("trace ReferenceGrant source=%v", from)
	}
	if to["group"] != "" || to["kind"] != "Service" || to["name"] != "otel-collector" {
		t.Fatalf("trace ReferenceGrant target=%v", to)
	}
}

func assertInferencePoolContract(t *testing.T, objects []client.Object) {
	t.Helper()
	var pool *unstructured.Unstructured
	var configMap *corev1.ConfigMap
	for _, object := range objects {
		switch value := object.(type) {
		case *unstructured.Unstructured:
			if value.GetKind() == "InferencePool" {
				pool = value
			}
		case *corev1.ConfigMap:
			configMap = value
		}
	}
	if pool == nil || configMap == nil {
		t.Fatalf("revision resources omit InferencePool or endpoint-picker ConfigMap")
	}
	if pool.GetAPIVersion() != "inference.networking.k8s.io/v1" {
		t.Fatalf("InferencePool apiVersion=%q", pool.GetAPIVersion())
	}
	if got, _, _ := unstructured.NestedString(pool.Object, "spec", "endpointPickerRef", "failureMode"); got != "FailClose" {
		t.Fatalf("InferencePool failureMode=%q, want FailClose", got)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(configMap.Data["endpoint-picker-config.yaml"]), &config); err != nil {
		t.Fatalf("decode EPP config: %v", err)
	}
	profiles := config["schedulingProfiles"].([]any)
	plugins := profiles[0].(map[string]any)["plugins"].([]any)
	if len(plugins) != 2 {
		t.Fatalf("EPP scheduling profile plugins=%v", plugins)
	}
	for _, plugin := range plugins {
		if plugin.(map[string]any)["pluginRef"] == "" {
			t.Fatalf("EPP scheduling plugin is not a pluginRef object: %v", plugin)
		}
	}
	featureGates := config["featureGates"].([]any)
	if len(featureGates) != 1 || featureGates[0] != "flowControl" {
		t.Fatalf("EPP flow-control feature gate=%v", featureGates)
	}
}

func assertPoolBackend(t *testing.T, backend map[string]any, weight int64) {
	t.Helper()
	if backend["group"] != "inference.networking.k8s.io" || backend["kind"] != "InferencePool" || backend["weight"] != weight {
		t.Fatalf("weighted InferencePool backend=%v", backend)
	}
	filters := backend["filters"].([]any)
	modifier := filter(filters, "RequestHeaderModifier")
	if modifier == nil {
		t.Fatal("InferencePool backend omits objective header modifier")
	}
	requestHeaders := modifier["requestHeaderModifier"].(map[string]any)
	set := requestHeaders["set"].([]any)
	header := set[0].(map[string]any)
	if header["name"] != routing.HeaderObjective || header["value"] == "" {
		t.Fatalf("objective header=%v", header)
	}
}

func hasFilter(filters []any, filterType string) bool { return filter(filters, filterType) != nil }

func filter(filters []any, filterType string) map[string]any {
	for _, rawFilter := range filters {
		candidate := rawFilter.(map[string]any)
		if candidate["type"] == filterType {
			return candidate
		}
	}
	return nil
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	current := workingDirectory
	for {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			t.Fatal("repository root not found")
		}
		current = parent
	}
}
