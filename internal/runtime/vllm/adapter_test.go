package vllm

import (
	"context"
	"strings"
	"testing"

	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestRenderUsesExclusiveGPUsAndReadOnlyCache(t *testing.T) {
	t.Parallel()
	spec := platformruntime.Spec{
		DeploymentName: "chat", Namespace: "tenant-a", Tenant: "a", ModelName: "qwen3-8b",
		ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: "abc", ResolvedBackend: platformruntime.BackendVLLM,
		Precision: "bf16", TensorParallel: 2, MaxModelLen: 8192, AcceleratorCount: 2,
		MinReplicas: 1, MaxReplicas: 4, PrefixCaching: true, RoutingPolicy: "prefix-aware",
	}
	resources, err := New().Render(context.Background(), platformruntime.RenderContext{
		Spec: spec, Revision: platformruntime.Revision{Name: "chat-a8f32"},
		Images: platformruntime.Images{VLLM: "example/vllm@sha256:deadbeef"}, ModelPath: "/var/lib/inferscale/models/abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	deployment, ok := resources.Serving[0].(*appsv1.Deployment)
	if !ok {
		t.Fatalf("first serving object is %T, want Deployment", resources.Serving[0])
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	quantity := container.Resources.Limits[corev1.ResourceName("nvidia.com/gpu")]
	if got := quantity.Value(); got != 2 {
		t.Fatalf("GPU limit = %d, want 2", got)
	}
	if !container.VolumeMounts[0].ReadOnly {
		t.Fatal("model cache mount is writable")
	}
	for _, arg := range container.Args {
		if strings.Contains(arg, "$(") {
			t.Fatalf("container argument contains an unexpanded Kubernetes env reference: %q", arg)
		}
	}
	environment := make(map[string]string, len(container.Env))
	for _, variable := range container.Env {
		environment[variable.Name] = variable.Value
	}
	if environment["PRECISION"] != "bf16" {
		t.Fatalf("PRECISION=%q, want bf16", environment["PRECISION"])
	}
	if environment["QUANTIZATION"] != "none" {
		t.Fatalf("QUANTIZATION=%q, want none", environment["QUANTIZATION"])
	}
	if environment["KV_EVENTS_PORT"] != "5557" {
		t.Fatalf("KV_EVENTS_PORT=%q, want 5557", environment["KV_EVENTS_PORT"])
	}
	if environment["TRACING_ENABLED"] != "false" {
		t.Fatalf("TRACING_ENABLED=%q, want false", environment["TRACING_ENABLED"])
	}
}

func TestRenderUsesNativeEndpointDrainBeforeRuntimeShutdown(t *testing.T) {
	t.Parallel()
	spec := platformruntime.Spec{
		DeploymentName: "chat", Namespace: "tenant-a", Tenant: "a", ModelName: "qwen3-8b",
		ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: strings.Repeat("a", 40), ResolvedBackend: platformruntime.BackendVLLM,
		Precision: "bf16", Quantization: "none", TensorParallel: 1, MaxModelLen: 8192,
		AcceleratorCount: 1, MinReplicas: 1, MaxReplicas: 2, RoutingPolicy: "load-aware",
	}
	resources, err := New().Render(t.Context(), platformruntime.RenderContext{
		Spec: spec, Revision: platformruntime.Revision{Name: "chat-a8f32"},
		Images: platformruntime.Images{VLLM: "example/vllm@sha256:deadbeef"}, ModelPath: "/var/lib/inferscale/models/abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	deployment := resources.Serving[0].(*appsv1.Deployment)
	pod := deployment.Spec.Template.Spec
	if pod.TerminationGracePeriodSeconds == nil || *pod.TerminationGracePeriodSeconds != platformruntime.WorkerTerminationGracePeriodSeconds {
		t.Fatalf("termination grace = %v, want %d seconds", pod.TerminationGracePeriodSeconds, platformruntime.WorkerTerminationGracePeriodSeconds)
	}
	runtimeContainer := pod.Containers[0]
	if runtimeContainer.ReadinessProbe == nil {
		t.Fatal("runtime has no readiness probe for endpoint eligibility")
	}
	if runtimeContainer.Lifecycle == nil || runtimeContainer.Lifecycle.PreStop == nil || runtimeContainer.Lifecycle.PreStop.Sleep == nil {
		t.Fatalf("runtime preStop = %#v, want native Kubernetes sleep action", runtimeContainer.Lifecycle)
	}
	if got := runtimeContainer.Lifecycle.PreStop.Sleep.Seconds; got != platformruntime.WorkerEndpointPropagationDelaySeconds {
		t.Fatalf("endpoint propagation delay = %d, want %d seconds", got, platformruntime.WorkerEndpointPropagationDelaySeconds)
	}
	if runtimeContainer.Lifecycle.PreStop.Exec != nil || runtimeContainer.Lifecycle.PreStop.HTTPGet != nil {
		t.Fatalf("runtime preStop executes image/application code: %#v", runtimeContainer.Lifecycle.PreStop)
	}
}

func TestRenderRequiresAndPropagatesOTLPEndpointWhenTracingEnabled(t *testing.T) {
	t.Parallel()
	spec := platformruntime.Spec{
		DeploymentName: "chat", Namespace: "tenant-a", Tenant: "a", ModelName: "qwen3-8b",
		ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: strings.Repeat("a", 40), ResolvedBackend: platformruntime.BackendVLLM,
		Precision: "bf16", Quantization: "none", TensorParallel: 1, MaxModelLen: 8192,
		AcceleratorCount: 1, MinReplicas: 1, MaxReplicas: 1, Tracing: true,
	}
	context := platformruntime.RenderContext{
		Spec: spec, Revision: platformruntime.Revision{Name: "chat-a8f32"},
		Images: platformruntime.Images{VLLM: "example/vllm@sha256:deadbeef"}, ModelPath: "/var/lib/inferscale/models/abc",
	}
	if _, err := New().Render(t.Context(), context); err == nil || !strings.Contains(err.Error(), "OTLP endpoint") {
		t.Fatalf("missing trace endpoint error = %v", err)
	}
	context.OTLPEndpoint = "http://otel-collector.inferscale-monitoring.svc.cluster.local:4317"
	resources, err := New().Render(t.Context(), context)
	if err != nil {
		t.Fatal(err)
	}
	deployment := resources.Serving[0].(*appsv1.Deployment)
	environment := map[string]string{}
	for _, variable := range deployment.Spec.Template.Spec.Containers[0].Env {
		environment[variable.Name] = variable.Value
	}
	if environment["TRACING_ENABLED"] != "true" || environment["OTLP_TRACES_ENDPOINT"] != context.OTLPEndpoint {
		t.Fatalf("trace environment = %#v", environment)
	}
}
