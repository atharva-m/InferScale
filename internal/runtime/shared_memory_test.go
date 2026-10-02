package runtime_test

import (
	"fmt"
	"strings"
	"testing"

	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	"github.com/inferscale/inferscale/internal/runtime/trtllm"
	"github.com/inferscale/inferscale/internal/runtime/vllm"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestGPUWorkersHaveBoundedPrivateSharedMemory(t *testing.T) {
	t.Parallel()
	for _, adapter := range []platformruntime.Adapter{vllm.New(), trtllm.New()} {
		for _, gpuCount := range []int32{1, 2, 4} {
			t.Run(fmt.Sprintf("%s/TP%d", adapter.Name(), gpuCount), func(t *testing.T) {
				t.Parallel()
				image := "example/runtime@sha256:" + strings.Repeat("a", 64)
				spec := platformruntime.Spec{
					DeploymentName: "chat", Namespace: "tenant-a", Tenant: "a", ModelName: "qwen3-8b",
					ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: strings.Repeat("b", 40),
					ResolvedBackend: adapter.Name(), RuntimeVersion: image,
					Precision: "bf16", Quantization: "none", TensorParallel: gpuCount,
					MaxModelLen: 8192, AcceleratorCount: gpuCount, AcceleratorType: "RTX_5090",
					MinReplicas: 1, MaxReplicas: 4, PrefixCaching: true, RoutingPolicy: "load-aware",
				}
				resources, err := adapter.Render(t.Context(), platformruntime.RenderContext{
					Spec: spec, Revision: platformruntime.Revision{Name: "chat-a8f32"},
					Images:    platformruntime.Images{VLLM: image, TensorRTLLM: image, TensorRTBuild: image},
					ModelPath: "/var/lib/inferscale/models/abc", EngineCacheHostPath: "/var/lib/inferscale/engines",
					GPUNodeSelector: map[string]string{"kubernetes.io/hostname": "gpu-server"},
				})
				if err != nil {
					t.Fatal(err)
				}
				worker := resources.Serving[0].(*appsv1.Deployment)
				assertPrivateSharedMemory(t, worker.Spec.Template.Spec, gpuCount)
				for _, prerequisite := range resources.Prerequisites {
					if job, ok := prerequisite.(*batchv1.Job); ok {
						assertPrivateSharedMemory(t, job.Spec.Template.Spec, gpuCount)
					}
				}
			})
		}
	}
}

func assertPrivateSharedMemory(t *testing.T, pod corev1.PodSpec, gpuCount int32) {
	t.Helper()
	if pod.HostIPC || pod.HostNetwork || pod.HostPID {
		t.Fatal("GPU IPC setup exposes a host namespace")
	}
	if pod.NodeSelector["kubernetes.io/hostname"] != "gpu-server" {
		t.Fatalf("GPU worker escaped the selected model-cache host: %#v", pod.NodeSelector)
	}
	var sharedMemory *corev1.Volume
	for index := range pod.Volumes {
		if pod.Volumes[index].Name == "shm" {
			sharedMemory = &pod.Volumes[index]
		}
	}
	if sharedMemory == nil || sharedMemory.EmptyDir == nil || sharedMemory.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Fatalf("shared memory must be a Pod-local memory-backed emptyDir: %#v", sharedMemory)
	}
	if sharedMemory.EmptyDir.SizeLimit == nil || sharedMemory.EmptyDir.SizeLimit.Value() != int64(gpuCount)*2*1024*1024*1024 {
		t.Fatalf("shared memory must have a 2 GiB per GPU bound: %#v", sharedMemory.EmptyDir)
	}
	worker := pod.Containers[0]
	for _, resourceList := range []corev1.ResourceList{worker.Resources.Requests, worker.Resources.Limits} {
		allocated := resourceList[corev1.ResourceName("nvidia.com/gpu")]
		if allocated.Value() != int64(gpuCount) {
			t.Fatalf("GPU allocation = %s, want %d", allocated.String(), gpuCount)
		}
	}
	for _, mount := range worker.VolumeMounts {
		if mount.Name == "shm" && mount.MountPath == "/dev/shm" && !mount.ReadOnly {
			return
		}
	}
	t.Fatalf("GPU worker has no writable /dev/shm mount: %#v", worker.VolumeMounts)
}
