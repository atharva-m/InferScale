package modelcache

import (
	"strings"
	"testing"

	platformruntime "github.com/inferscale/inferscale/internal/runtime"
	corev1 "k8s.io/api/core/v1"
)

func TestCacheKeyIsRevisionAddressed(t *testing.T) {
	t.Parallel()
	first := CacheKey("hf://Qwen/Qwen3-8B", "abc")
	second := CacheKey("hf://Qwen/Qwen3-8B", "def")
	if first == second {
		t.Fatal("different immutable revisions share a cache key")
	}
	if !strings.HasPrefix(first, "sha256-") {
		t.Fatalf("cache key %q is not content-address shaped", first)
	}
}

func TestCacheKeyCanonicalizesCommitSHA(t *testing.T) {
	t.Parallel()
	if CacheKey("hf://Qwen/Qwen3-8B", "ABCDEF") != CacheKey("hf://Qwen/Qwen3-8B", "abcdef") {
		t.Fatal("cache key must canonicalize an immutable commit SHA to lowercase")
	}
}

func TestJobPinsImmutableRevision(t *testing.T) {
	t.Parallel()
	renderer := Renderer{Config: Config{Image: "prefetch@sha256:deadbeef", CacheRoot: "/var/lib/inferscale/models"}}
	spec := platformruntime.Spec{
		DeploymentName: "chat", Namespace: "tenant-a", ModelURI: "hf://Qwen/Qwen3-8B",
		ModelRevision: "abc", ModelName: "qwen", ResolvedBackend: platformruntime.BackendVLLM,
	}
	job, err := renderer.Job(spec, platformruntime.Revision{Name: "chat-a8f32"})
	if err != nil {
		t.Fatal(err)
	}
	args := job.Spec.Template.Spec.Containers[0].Args
	if strings.Join(args, " ") != "--uri hf://Qwen/Qwen3-8B --revision abc --cache-root /cache --full-verification" {
		t.Fatalf("unexpected prefetch arguments: %#v", args)
	}
	if job.Spec.Template.Spec.AutomountServiceAccountToken == nil || *job.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("model prefetch Job must not receive a Kubernetes API token")
	}
}

func TestVerificationJobIsBoundedAndReadOnly(t *testing.T) {
	t.Parallel()
	renderer := Renderer{Config: Config{Image: "prefetch@sha256:deadbeef", CacheRoot: "/var/lib/inferscale/models"}}
	spec := platformruntime.Spec{
		DeploymentName: "chat", Namespace: "tenant-a", ModelURI: "hf://Qwen/Qwen3-8B",
		ModelRevision: "abc", ModelName: "qwen", ResolvedBackend: platformruntime.BackendVLLM,
	}
	job, err := renderer.VerificationJob(spec, platformruntime.Revision{Name: "chat-a8f32"})
	if err != nil {
		t.Fatal(err)
	}
	container := job.Spec.Template.Spec.Containers[0]
	if container.Resources.Limits.Cpu().MilliValue() != 500 || container.Resources.Limits.Memory().Value() != 512*1024*1024 ||
		container.Resources.Requests.Cpu().MilliValue() != 100 || container.Resources.Requests.Memory().Value() != 128*1024*1024 {
		t.Fatalf("verifier must declare bounded CPU and memory independently of namespace defaults: %#v", container.Resources)
	}
	if strings.Join(container.Args, " ") != "--uri hf://Qwen/Qwen3-8B --revision abc --cache-root /cache --verify-only --full-verification" {
		t.Fatalf("unexpected verification arguments: %#v", container.Args)
	}
	if len(container.VolumeMounts) != 2 || container.VolumeMounts[0].Name != "cache" || !container.VolumeMounts[0].ReadOnly {
		t.Fatalf("verification cache mount is writable: %#v", container.VolumeMounts)
	}
	if container.VolumeMounts[1].Name != "tmp" || container.VolumeMounts[1].MountPath != "/tmp" || container.VolumeMounts[1].ReadOnly {
		t.Fatalf("verifier requires a writable temporary mount: %#v", container.VolumeMounts)
	}
	pod := job.Spec.Template.Spec
	if len(pod.Volumes) != 2 || pod.Volumes[0].HostPath == nil || pod.Volumes[0].HostPath.Path != renderer.Config.CacheRoot ||
		pod.Volumes[1].EmptyDir == nil || pod.Volumes[1].EmptyDir.SizeLimit == nil || pod.Volumes[1].EmptyDir.SizeLimit.Value() != 64*1024*1024 {
		t.Fatalf("verifier must mount the cache and bounded temporary storage: %#v", pod.Volumes)
	}
	security := pod.SecurityContext
	if security == nil || security.RunAsNonRoot == nil || !*security.RunAsNonRoot ||
		security.RunAsUser == nil || *security.RunAsUser != 65532 ||
		security.RunAsGroup == nil || *security.RunAsGroup != 65532 ||
		security.FSGroup == nil || *security.FSGroup != 65532 ||
		security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatalf("verifier must run as the unprivileged cache owner with default seccomp: %#v", security)
	}
	containerSecurity := container.SecurityContext
	if containerSecurity == nil || containerSecurity.ReadOnlyRootFilesystem == nil || !*containerSecurity.ReadOnlyRootFilesystem ||
		containerSecurity.AllowPrivilegeEscalation == nil || *containerSecurity.AllowPrivilegeEscalation ||
		containerSecurity.Capabilities == nil || len(containerSecurity.Capabilities.Add) != 0 ||
		len(containerSecurity.Capabilities.Drop) != 1 || containerSecurity.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("verifier must drop capabilities and forbid writes to its root filesystem or privilege escalation: %#v", containerSecurity)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds <= 0 || job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("verification Job is not bounded: deadline=%v backoff=%v", job.Spec.ActiveDeadlineSeconds, job.Spec.BackoffLimit)
	}
	if job.Spec.Template.Spec.AutomountServiceAccountToken == nil || *job.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("model-cache verification Job must not receive a Kubernetes API token")
	}
}

func TestSharedVerificationIdentityAndEvidenceRetention(t *testing.T) {
	t.Parallel()
	renderer := Renderer{Config: Config{Image: "prefetch@sha256:deadbeef", CacheRoot: "/var/lib/inferscale/models"}}
	first := platformruntime.Spec{DeploymentName: "first", Namespace: "tenant-a", ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: "abc"}
	job, err := renderer.SharedVerificationJob(first, "gpu-node", SharedVerificationNamespace)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.DeploymentName, second.Namespace = "second", "tenant-b"
	shared, err := renderer.SharedVerificationJob(second, "gpu-node", SharedVerificationNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if job.Name != shared.Name {
		t.Fatal("same physical cache entry has tenant-dependent verification identity")
	}
	otherNode, err := renderer.SharedVerificationJob(first, "other-node", SharedVerificationNamespace)
	if err != nil {
		t.Fatal(err)
	}
	second.ModelRevision = "def"
	otherRevision, err := renderer.SharedVerificationJob(second, "gpu-node", SharedVerificationNamespace)
	if err != nil {
		t.Fatal(err)
	}
	renderer.Config.CacheRoot = "/different/cache"
	otherPath, err := renderer.SharedVerificationJob(first, "gpu-node", SharedVerificationNamespace)
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{otherNode.Name, otherRevision.Name, otherPath.Name} {
		if other == job.Name {
			t.Fatal("different physical cache entries share verification identity")
		}
	}
	if job.Spec.TTLSecondsAfterFinished != nil {
		t.Fatal("unconsumed checksum failure can be erased by TTL cleanup")
	}
	if !job.Spec.Template.Spec.Containers[0].VolumeMounts[0].ReadOnly || job.Spec.Template.Spec.NodeName != "gpu-node" {
		t.Fatal("shared verifier must read the observed node's cache without mutating it")
	}
}
