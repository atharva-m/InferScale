package fake

import (
	"context"
	"testing"

	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	appsv1 "k8s.io/api/apps/v1"
)

func TestAdapterRendersCPUOnlyWorkerWithoutCacheMount(t *testing.T) {
	spec := platformruntime.Spec{
		DeploymentName: "chat", Namespace: "tenant-acme", Tenant: "acme",
		ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ModelName: "qwen3-8b",
		ResolvedBackend: platformruntime.BackendVLLM, Precision: "bf16", TensorParallel: 1,
		MaxModelLen: 1024, AcceleratorCount: 1, MinReplicas: 1, MaxReplicas: 2,
		RoutingPolicy: "load-aware",
	}
	resources, err := New().Render(context.Background(), platformruntime.RenderContext{
		Spec: spec, Revision: platformruntime.Revision{Name: "chat-deadbeef"},
		Images: platformruntime.Images{VLLM: "ghcr.io/inferscale/fake-runtime:dev"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resources.Prerequisites) != 0 || len(resources.Serving) != 2 {
		t.Fatalf("resources=%+v", resources)
	}
	workload, ok := resources.Serving[0].(*appsv1.Deployment)
	if !ok {
		t.Fatalf("serving[0]=%T", resources.Serving[0])
	}
	pod := workload.Spec.Template.Spec
	if len(pod.Containers) != 1 || len(pod.Volumes) != 0 || len(pod.Containers[0].VolumeMounts) != 0 {
		t.Fatalf("fake worker unexpectedly uses runtime cache volumes: %+v", pod)
	}
	if len(pod.Containers[0].Resources.Requests) != 0 || len(pod.Containers[0].Resources.Limits) != 0 {
		t.Fatalf("fake worker unexpectedly requests accelerators: %+v", pod.Containers[0].Resources)
	}
	if resources.WorkloadName != "chat-deadbeef-vllm" {
		t.Fatalf("workload name=%q, want canonical vLLM controller identity", resources.WorkloadName)
	}
	if New().Capabilities().RequiresModelCache {
		t.Fatal("fake runtime must not require the GPU model cache")
	}
}

func TestAdapterRejectsPrefixRouting(t *testing.T) {
	spec := platformruntime.Spec{
		DeploymentName: "chat", Namespace: "tenant-acme", ModelURI: "hf://Qwen/Qwen3-8B",
		ModelRevision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ResolvedBackend: platformruntime.BackendVLLM,
		TensorParallel: 1, AcceleratorCount: 1, MaxModelLen: 1024, MinReplicas: 1, MaxReplicas: 1,
		RoutingPolicy: "prefix-aware",
	}
	if err := New().Validate(spec); err == nil {
		t.Fatal("prefix-aware fake runtime contract was accepted")
	}
}
