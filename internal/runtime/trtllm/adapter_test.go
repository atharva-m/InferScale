package trtllm

import (
	"context"
	"slices"
	"strings"
	"testing"

	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestRenderAddsJSONMetricsExporterWithoutGPUAllocation(t *testing.T) {
	t.Parallel()
	spec := platformruntime.Spec{
		DeploymentName: "chat", Namespace: "tenant-a", Tenant: "a", ModelName: "qwen3-8b",
		ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: "abc", ResolvedBackend: platformruntime.BackendTRTLLM,
		Precision: "bf16", Quantization: "none", TensorParallel: 2, MaxModelLen: 8192, AcceleratorCount: 2,
		RuntimeVersion: "example/trtllm:1.0.0@sha256:" + strings.Repeat("a", 64), AcceleratorType: "RTX_5090",
		MinReplicas: 1, MaxReplicas: 4, PrefixCaching: true, RoutingPolicy: "load-aware",
	}
	resources, err := New().Render(context.Background(), platformruntime.RenderContext{
		Spec: spec, Revision: platformruntime.Revision{Name: "chat-a8f32"},
		Images: platformruntime.Images{
			TensorRTLLM: "example/trtllm@sha256:" + strings.Repeat("a", 64), TensorRTBuild: "example/trtllm@sha256:" + strings.Repeat("a", 64),
		},
		ModelPath: "/var/lib/inferscale/models/abc", EngineCacheHostPath: "/var/lib/inferscale/engines",
	})
	if err != nil {
		t.Fatal(err)
	}
	deployment, ok := resources.Serving[0].(*appsv1.Deployment)
	if !ok {
		t.Fatalf("first serving object is %T, want Deployment", resources.Serving[0])
	}
	containers := deployment.Spec.Template.Spec.Containers
	if len(containers) != 2 || containers[1].Name != "metrics-exporter" {
		t.Fatalf("containers = %#v, want runtime plus metrics-exporter", containers)
	}
	exporter := containers[1]
	if len(exporter.Command) != 2 || exporter.Command[1] != "/opt/inferscale/metrics_exporter.py" {
		t.Fatalf("exporter command = %v", exporter.Command)
	}
	if len(exporter.Resources.Requests) != 0 || len(exporter.Resources.Limits) != 0 {
		t.Fatalf("metrics exporter must not allocate a GPU: %#v", exporter.Resources)
	}
	runtimeEnvironment := map[string]string{}
	for _, variable := range containers[0].Env {
		runtimeEnvironment[variable.Name] = variable.Value
	}
	for _, name := range []string{"PRECISION", "QUANTIZATION", "MAX_MODEL_LEN", "RUNTIME_VERSION", "RUNTIME_IMAGE_DIGEST", "GPU_ARCHITECTURE"} {
		if runtimeEnvironment[name] == "" {
			t.Fatalf("runtime is missing engine-verification input %s: %#v", name, runtimeEnvironment)
		}
	}
	if len(exporter.Ports) != 1 || exporter.Ports[0].Name != "metrics" || exporter.Ports[0].ContainerPort != metricsExporterPort {
		t.Fatalf("exporter ports = %#v", exporter.Ports)
	}

	service, ok := resources.Serving[1].(*corev1.Service)
	if !ok {
		t.Fatalf("second serving object is %T, want Service", resources.Serving[1])
	}
	if len(service.Spec.Ports) != 2 || service.Spec.Ports[1].Name != "metrics" || service.Spec.Ports[1].Port != metricsExporterPort {
		t.Fatalf("service ports = %#v", service.Spec.Ports)
	}
	engineVolume := deployment.Spec.Template.Spec.Volumes[1].HostPath
	if engineVolume == nil || engineVolume.Path == "/var/lib/inferscale/engines" || !strings.HasPrefix(engineVolume.Path, "/var/lib/inferscale/engines/") {
		t.Fatalf("serving engine cache path is not a derived compatibility key: %#v", engineVolume)
	}
	job := resources.Prerequisites[0].(*batchv1.Job)
	buildVolume := job.Spec.Template.Spec.Volumes[1].HostPath
	if buildVolume == nil || buildVolume.Path != "/var/lib/inferscale/engines" {
		t.Fatalf("builder must mount the cache parent for atomic publication: %#v", buildVolume)
	}
	buildMount := job.Spec.Template.Spec.Containers[0].VolumeMounts[1]
	if buildMount.Name != "engine" || buildMount.MountPath != "/engines" || buildMount.ReadOnly {
		t.Fatalf("builder engine mount = %#v, want writable host cache root at /engines", buildMount)
	}
	args := job.Spec.Template.Spec.Containers[0].Args
	outputIndex := slices.Index(args, "--output")
	if outputIndex < 0 || outputIndex+1 >= len(args) || !strings.HasPrefix(args[outputIndex+1], "/engines/") {
		t.Fatalf("builder output must be published below the mounted /engines root: %v", args)
	}
	for _, expected := range []string{"--runtime-version", "--runtime-image-digest", "--gpu-architecture"} {
		if !slices.Contains(args, expected) {
			t.Fatalf("builder args %v do not contain %s", args, expected)
		}
	}
}

func TestRenderUsesNativeEndpointDrainBeforeRuntimeShutdown(t *testing.T) {
	t.Parallel()
	spec := platformruntime.Spec{
		DeploymentName: "chat", Namespace: "tenant-a", Tenant: "a", ModelName: "qwen3-8b",
		ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: strings.Repeat("a", 40), ResolvedBackend: platformruntime.BackendTRTLLM,
		Precision: "bf16", Quantization: "none", TensorParallel: 1, MaxModelLen: 8192, AcceleratorCount: 1,
		RuntimeVersion: "example/trtllm:1.0.0@sha256:" + strings.Repeat("a", 64), AcceleratorType: "RTX_5090",
		MinReplicas: 1, MaxReplicas: 2, RoutingPolicy: "load-aware",
	}
	resources, err := New().Render(t.Context(), platformruntime.RenderContext{
		Spec: spec, Revision: platformruntime.Revision{Name: "chat-a8f32"},
		Images: platformruntime.Images{
			TensorRTLLM:   "example/trtllm@sha256:" + strings.Repeat("a", 64),
			TensorRTBuild: "example/trtllm@sha256:" + strings.Repeat("a", 64),
		},
		ModelPath: "/var/lib/inferscale/models/abc", EngineCacheHostPath: "/var/lib/inferscale/engines",
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
	if pod.Containers[1].Lifecycle != nil {
		t.Fatalf("metrics exporter must not hold runtime termination: %#v", pod.Containers[1].Lifecycle)
	}
}

func TestCapabilitiesMatchPinnedBF16OnlyValidation(t *testing.T) {
	capabilities := New().Capabilities()
	if !capabilities.BF16 || capabilities.FP8 {
		t.Fatalf("capabilities = %#v", capabilities)
	}
}
