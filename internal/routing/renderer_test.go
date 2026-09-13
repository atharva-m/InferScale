package routing

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestInferencePoolUsesStableV1API(t *testing.T) {
	t.Parallel()
	renderer := Renderer{Config: Config{EndpointPickerImage: "epp@sha256:deadbeef", GatewayNamespace: "gateway"}}
	spec := platformruntime.Spec{
		DeploymentName: "chat", Namespace: "tenant-a", Tenant: "a", ModelName: "qwen",
		ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: "abc", ResolvedBackend: platformruntime.BackendVLLM,
		Precision: "bf16", TensorParallel: 1, AcceleratorCount: 1, MaxModelLen: 8192,
		MaxConcurrentRequests: 32, MaxQueuedRequests: 128, RoutingPolicy: "load-aware",
	}
	rendered, err := renderer.RenderRevision(spec, platformruntime.Revision{Name: "chat-a8f32"}, "chat-a8f32-runtime")
	if err != nil {
		t.Fatal(err)
	}
	var pool *unstructured.Unstructured
	for _, object := range rendered.Objects {
		if item, ok := object.(*unstructured.Unstructured); ok && item.GetKind() == "InferencePool" {
			pool = item
		}
	}
	if pool == nil {
		t.Fatal("InferencePool was not rendered")
	}
	if pool.GetAPIVersion() != "inference.networking.k8s.io/v1" {
		t.Fatalf("apiVersion = %q", pool.GetAPIVersion())
	}
	if got, _, _ := unstructured.NestedString(pool.Object, "spec", "endpointPickerRef", "failureMode"); got != "FailClose" {
		t.Fatalf("failureMode = %q, want FailClose", got)
	}
}

func TestEndpointPickerProfilesUsePluginReferences(t *testing.T) {
	t.Parallel()
	spec := platformruntime.Spec{
		DeploymentName: "chat", Namespace: "tenant-a", Tenant: "a", ModelName: "qwen",
		ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: "abc", ResolvedBackend: platformruntime.BackendVLLM,
		Precision: "bf16", TensorParallel: 1, AcceleratorCount: 1, MaxModelLen: 8192,
		MaxConcurrentRequests: 32, MaxQueuedRequests: 128, RoutingPolicy: "load-aware",
	}
	rendered, err := (Renderer{Config: Config{EndpointPickerImage: "epp@sha256:deadbeef"}}).
		RenderRevision(spec, platformruntime.Revision{Name: "chat-a8f32"}, "chat-a8f32-runtime")
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	for _, object := range rendered.Objects {
		if configMap, ok := object.(*corev1.ConfigMap); ok {
			raw = configMap.Data["endpoint-picker-config.yaml"]
		}
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		t.Fatal(err)
	}
	// The pinned EPP decodes this non-resource configuration strictly and
	// embeds TypeMeta only; Kubernetes ObjectMeta would prevent startup.
	if _, exists := config["metadata"]; exists {
		t.Fatal("EndpointPickerConfig must not contain unsupported metadata")
	}
	profiles := config["schedulingProfiles"].([]any)
	plugins := profiles[0].(map[string]any)["plugins"].([]any)
	for index, plugin := range plugins {
		if _, ok := plugin.(map[string]any)["pluginRef"]; !ok {
			t.Fatalf("plugins[%d] = %#v, want pluginRef object", index, plugin)
		}
	}
}

func TestEndpointPickerPortsAndReadinessMatchServingConfiguration(t *testing.T) {
	t.Parallel()
	for _, customPorts := range []bool{false, true} {
		config := Config{EndpointPickerImage: "epp@sha256:deadbeef"}
		if customPorts {
			config.EndpointPickerPort, config.MetricsPort = 9102, 9190
		}
		rendered, err := (Renderer{Config: config}).RenderRevision(platformruntime.Spec{
			DeploymentName: "chat", Namespace: "tenant-a", Tenant: "a", RoutingPolicy: "load-aware",
		}, platformruntime.Revision{Name: "chat-rev"}, "chat-rev-runtime")
		if err != nil {
			t.Fatal(err)
		}
		var deployment *appsv1.Deployment
		var service *corev1.Service
		for _, object := range rendered.Objects {
			switch typed := object.(type) {
			case *appsv1.Deployment:
				deployment = typed
			case *corev1.Service:
				service = typed
			}
		}
		if deployment == nil || service == nil {
			t.Fatal("endpoint-picker Deployment and Service are required")
		}
		container := deployment.Spec.Template.Spec.Containers[0]
		flags := map[string]string{}
		for _, arg := range container.Args {
			key, value, found := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
			if found {
				flags[key] = value
			}
		}
		ports := map[string]int32{}
		for _, port := range container.Ports {
			ports[port.Name] = port.ContainerPort
		}
		for _, port := range service.Spec.Ports {
			if got := ports[port.TargetPort.StrVal]; got != port.Port {
				t.Fatalf("service port %s targets %d, exposes %d", port.Name, got, port.Port)
			}
			if flags[port.Name+"-port"] != strconv.FormatInt(int64(port.Port), 10) {
				t.Fatalf("service port %s does not match the EPP listener flag: %#v", port.Name, flags)
			}
		}
		probe := container.ReadinessProbe
		if probe == nil || probe.GRPC == nil || probe.GRPC.Service == nil || *probe.GRPC.Service != "readiness" ||
			probe.GRPC.Port != ports["health"] || flags["grpc-health-port"] != strconv.FormatInt(int64(probe.GRPC.Port), 10) {
			t.Fatalf("readiness must check the EPP pool-sync health service: %#v", probe)
		}
		if probe.GRPC.Port == ports["grpc"] {
			t.Fatal("the plain gRPC health probe cannot use the TLS inference listener")
		}
		if flags["metrics-endpoint-auth"] != "false" || flags["secure-serving"] != "true" || flags["enable-pprof"] != "false" {
			t.Fatalf("EPP must preserve TLS inference and isolated plain metrics without profiling: %#v", flags)
		}
	}
}

func TestScaleToZeroQueueUsesActivationTimeout(t *testing.T) {
	t.Parallel()
	spec := platformruntime.Spec{
		DeploymentName: "chat", Namespace: "tenant-a", Tenant: "a", ModelName: "qwen",
		ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: "abc", ResolvedBackend: platformruntime.BackendVLLM,
		Precision: "bf16", TensorParallel: 1, AcceleratorCount: 1, MaxModelLen: 8192,
		MinReplicas: 0, MaxReplicas: 4, MaxConcurrentRequests: 32, MaxQueuedRequests: 128, RoutingPolicy: "load-aware",
	}
	rendered, err := (Renderer{Config: Config{EndpointPickerImage: "epp@sha256:deadbeef"}}).
		RenderRevision(spec, platformruntime.Revision{Name: "chat-a8f32"}, "chat-a8f32-runtime")
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range rendered.Objects {
		configMap, ok := object.(*corev1.ConfigMap)
		if !ok {
			continue
		}
		var config map[string]any
		if err := json.Unmarshal([]byte(configMap.Data["endpoint-picker-config.yaml"]), &config); err != nil {
			t.Fatal(err)
		}
		flowControl := config["flowControl"].(map[string]any)
		if got := flowControl["defaultRequestTTL"]; got != "900s" {
			t.Fatalf("scale-to-zero request TTL = %v, want 900s", got)
		}
		return
	}
	t.Fatal("endpoint-picker ConfigMap was not rendered")
}

func TestTensorRTRuntimeMonitorScrapesExporterPort(t *testing.T) {
	t.Parallel()
	spec := platformruntime.Spec{
		DeploymentName: "chat", Namespace: "tenant-a", Tenant: "a", ModelName: "qwen",
		ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: "abc", ResolvedBackend: platformruntime.BackendTRTLLM,
		Precision: "bf16", TensorParallel: 1, AcceleratorCount: 1, MaxModelLen: 8192,
		MaxConcurrentRequests: 32, MaxQueuedRequests: 128, RoutingPolicy: "load-aware",
	}
	revision := platformruntime.Revision{Name: "chat-a8f32"}
	rendered, err := (Renderer{Config: Config{EndpointPickerImage: "epp@sha256:deadbeef"}}).
		RenderRevision(spec, revision, "chat-a8f32-runtime")
	if err != nil {
		t.Fatal(err)
	}
	wantName := "chat-a8f32-runtime"
	for _, object := range rendered.Objects {
		monitor, ok := object.(*unstructured.Unstructured)
		if !ok || monitor.GetKind() != "ServiceMonitor" || monitor.GetName() != wantName {
			continue
		}
		port, found, err := unstructured.NestedString(monitor.Object, "spec", "endpoints", "0", "port")
		if err == nil && found {
			// NestedString cannot index slices; retain this branch as a guard if
			// unstructured gains path indexing in a future dependency.
			if port != "metrics" {
				t.Fatalf("runtime monitor port = %q, want metrics", port)
			}
			return
		}
		endpoints, found, err := unstructured.NestedSlice(monitor.Object, "spec", "endpoints")
		if err != nil || !found || len(endpoints) != 1 {
			t.Fatalf("runtime monitor endpoints = %#v, found=%v, err=%v", endpoints, found, err)
		}
		if got := endpoints[0].(map[string]any)["port"]; got != "metrics" {
			t.Fatalf("runtime monitor port = %v, want metrics", got)
		}
		return
	}
	t.Fatal("TensorRT runtime ServiceMonitor was not rendered")
}

func TestRouteUsesRevisionSpecificObjectiveAndServiceShadow(t *testing.T) {
	t.Parallel()
	renderer := Renderer{}
	stableRevision := platformruntime.Revision{Name: "chat-stable"}
	candidateRevision := platformruntime.Revision{Name: "chat-candidate"}
	route, err := renderer.RenderRoute(RouteConfig{
		DeploymentName: "chat", Namespace: "tenant-a", GatewayName: "inference",
		Stable:    &RouteRevision{Revision: stableRevision, Names: Names(stableRevision), ServiceName: "stable-runtime", Weight: 95},
		Candidate: &RouteRevision{Revision: candidateRevision, Names: Names(candidateRevision), ServiceName: "candidate-runtime", Weight: 5},
		Shadow:    true, ShadowPercent: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	rules, found, err := unstructured.NestedSlice(route.Object, "spec", "rules")
	if err != nil || !found || len(rules) != 4 {
		t.Fatalf("rules = %#v, found=%v, err=%v", rules, found, err)
	}
	first := rules[0].(map[string]any)
	backends := first["backendRefs"].([]any)
	candidate := backends[1].(map[string]any)
	filters := candidate["filters"].([]any)
	modifier := filters[0].(map[string]any)["requestHeaderModifier"].(map[string]any)
	set := modifier["set"].([]any)
	value := set[0].(map[string]any)["value"]
	if value != Names(candidateRevision).Interactive {
		t.Fatalf("candidate objective = %v, want %q", value, Names(candidateRevision).Interactive)
	}
}
