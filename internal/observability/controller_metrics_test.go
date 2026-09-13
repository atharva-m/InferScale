package observability

import (
	"strings"
	"testing"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestDeploymentCollectorProjectsStatusWithoutConditionMessages(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resource := &platformv1alpha1.InferenceDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "chat", Namespace: "tenant-acme",
			Labels: map[string]string{kubeutil.LabelTenant: "tenant-1"},
		},
		Spec: platformv1alpha1.InferenceDeploymentSpec{Runtime: platformv1alpha1.RuntimeSpec{Backend: platformv1alpha1.RuntimeBackendVLLM}},
		Status: platformv1alpha1.InferenceDeploymentStatus{
			Phase:    platformv1alpha1.DeploymentPhaseReady,
			Runtime:  platformv1alpha1.RuntimeStatus{Backend: platformv1alpha1.RuntimeBackendVLLM},
			Replicas: platformv1alpha1.ReplicaStatus{Desired: 2, Ready: 1},
			Revision: platformv1alpha1.RevisionStatus{Stable: "rev-1", Candidate: "rev-2"},
			Rollout:  platformv1alpha1.RolloutStatus{Stage: "Canary25", CandidateWeight: 25},
			Conditions: []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionFalse, Reason: "SecretReason", Message: "prompt text must not leak",
			}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(resource).Build()
	registry := prometheus.NewRegistry()
	registry.MustRegister(NewDeploymentCollector(kubeClient))

	want := `
# HELP inferscale_deployment_ready Whether the deployment status is Ready.
# TYPE inferscale_deployment_ready gauge
inferscale_deployment_ready{backend="vllm",deployment="chat",namespace="tenant-acme",revision="rev-2",tenant="tenant-1"} 1
# HELP inferscale_deployment_replicas Deployment replicas by desired or ready state.
# TYPE inferscale_deployment_replicas gauge
inferscale_deployment_replicas{backend="vllm",deployment="chat",namespace="tenant-acme",revision="rev-2",state="desired",tenant="tenant-1"} 2
inferscale_deployment_replicas{backend="vllm",deployment="chat",namespace="tenant-acme",revision="rev-2",state="ready",tenant="tenant-1"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), "inferscale_deployment_ready", "inferscale_deployment_replicas"); err != nil {
		t.Fatal(err)
	}

	metrics, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range metrics {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if strings.Contains(label.GetValue(), "prompt text") || strings.Contains(label.GetValue(), "SecretReason") {
					t.Fatalf("condition content leaked into metric %s", family.GetName())
				}
			}
		}
	}
}

func TestDeploymentCollectorAccountsScheduledGPUsByBillingScope(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	deployment := &platformv1alpha1.InferenceDeployment{
		ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "tenant-acme"},
		Status: platformv1alpha1.InferenceDeploymentStatus{
			Revision: platformv1alpha1.RevisionStatus{Stable: "rev-stable", Candidate: "rev-candidate"},
		},
	}
	pod := func(name, revision string, gpus int64) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "tenant-acme", Labels: map[string]string{
				kubeutil.LabelManagedBy: kubeutil.ManagedByValue, kubeutil.LabelComponent: "model-server",
				kubeutil.LabelTenant: "tenant-1", kubeutil.LabelDeployment: "chat",
				kubeutil.LabelRevision: revision, kubeutil.LabelBackend: "vllm",
			}},
			Spec: corev1.PodSpec{
				NodeName: "gpu-node", Containers: []corev1.Container{{
					Name: "runtime", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
						corev1.ResourceName("nvidia.com/gpu"): *resource.NewQuantity(gpus, resource.DecimalSI),
					}},
				}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		}
	}
	build := pod("build", "rev-stable", 1)
	build.Labels[kubeutil.LabelComponent] = "engine-build"
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(deployment).WithObjects(
		deployment, pod("stable", "rev-stable", 1), pod("candidate", "rev-candidate", 2),
		pod("superseded", "rev-superseded", 1), pod("warming", "rev-warming", 1), build,
	).Build()
	collector := NewDeploymentCollector(kubeClient)
	now := time.Unix(1000, 0)
	collector.now = func() time.Time { return now }
	registry := prometheus.NewRegistry()
	registry.MustRegister(collector)
	if _, err := registry.Gather(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	want := `
# HELP inferscale_gpu_seconds_total Cumulative scheduled exclusive GPU allocation in seconds.
# TYPE inferscale_gpu_seconds_total counter
inferscale_gpu_seconds_total{backend="vllm",billing_scope="platform",deployment="chat",namespace="tenant-acme",revision="rev-candidate",tenant="tenant-1"} 20
inferscale_gpu_seconds_total{backend="vllm",billing_scope="platform",deployment="chat",namespace="tenant-acme",revision="rev-stable",tenant="tenant-1"} 10
inferscale_gpu_seconds_total{backend="vllm",billing_scope="platform",deployment="chat",namespace="tenant-acme",revision="rev-superseded",tenant="tenant-1"} 10
inferscale_gpu_seconds_total{backend="vllm",billing_scope="platform",deployment="chat",namespace="tenant-acme",revision="rev-warming",tenant="tenant-1"} 10
inferscale_gpu_seconds_total{backend="vllm",billing_scope="tenant",deployment="chat",namespace="tenant-acme",revision="rev-stable",tenant="tenant-1"} 10
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), "inferscale_gpu_seconds_total"); err != nil {
		t.Fatal(err)
	}
	// A serving candidate becomes tenant allocation when weighted canary
	// traffic starts. The shadow counter remains available without charging
	// the same interval twice, and unrelated/build Pods remain platform cost.
	deployment.Status.Rollout.CandidateWeight = 5
	if err := kubeClient.Status().Update(t.Context(), deployment); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Gather(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	want = `
# HELP inferscale_gpu_seconds_total Cumulative scheduled exclusive GPU allocation in seconds.
# TYPE inferscale_gpu_seconds_total counter
inferscale_gpu_seconds_total{backend="vllm",billing_scope="platform",deployment="chat",namespace="tenant-acme",revision="rev-candidate",tenant="tenant-1"} 20
inferscale_gpu_seconds_total{backend="vllm",billing_scope="platform",deployment="chat",namespace="tenant-acme",revision="rev-stable",tenant="tenant-1"} 20
inferscale_gpu_seconds_total{backend="vllm",billing_scope="platform",deployment="chat",namespace="tenant-acme",revision="rev-superseded",tenant="tenant-1"} 20
inferscale_gpu_seconds_total{backend="vllm",billing_scope="platform",deployment="chat",namespace="tenant-acme",revision="rev-warming",tenant="tenant-1"} 20
inferscale_gpu_seconds_total{backend="vllm",billing_scope="tenant",deployment="chat",namespace="tenant-acme",revision="rev-candidate",tenant="tenant-1"} 20
inferscale_gpu_seconds_total{backend="vllm",billing_scope="tenant",deployment="chat",namespace="tenant-acme",revision="rev-stable",tenant="tenant-1"} 20
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), "inferscale_gpu_seconds_total"); err != nil {
		t.Fatal(err)
	}
	deployment.Status.Rollout.CandidateWeight = 0
	if err := kubeClient.Status().Update(t.Context(), deployment); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Gather(); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Second)
	want = `
# HELP inferscale_gpu_seconds_total Cumulative scheduled exclusive GPU allocation in seconds.
# TYPE inferscale_gpu_seconds_total counter
inferscale_gpu_seconds_total{backend="vllm",billing_scope="platform",deployment="chat",namespace="tenant-acme",revision="rev-candidate",tenant="tenant-1"} 40
inferscale_gpu_seconds_total{backend="vllm",billing_scope="platform",deployment="chat",namespace="tenant-acme",revision="rev-stable",tenant="tenant-1"} 30
inferscale_gpu_seconds_total{backend="vllm",billing_scope="platform",deployment="chat",namespace="tenant-acme",revision="rev-superseded",tenant="tenant-1"} 30
inferscale_gpu_seconds_total{backend="vllm",billing_scope="platform",deployment="chat",namespace="tenant-acme",revision="rev-warming",tenant="tenant-1"} 30
inferscale_gpu_seconds_total{backend="vllm",billing_scope="tenant",deployment="chat",namespace="tenant-acme",revision="rev-candidate",tenant="tenant-1"} 20
inferscale_gpu_seconds_total{backend="vllm",billing_scope="tenant",deployment="chat",namespace="tenant-acme",revision="rev-stable",tenant="tenant-1"} 30
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), "inferscale_gpu_seconds_total"); err != nil {
		t.Fatal(err)
	}
}

func TestDeploymentCollectorExportsObservedRuntimeSafetyEvents(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := platformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker", Namespace: "tenant-acme", UID: "pod-uid-1",
			Labels: map[string]string{
				kubeutil.LabelManagedBy: kubeutil.ManagedByValue, kubeutil.LabelComponent: "model-server",
				kubeutil.LabelTenant: "tenant-1", kubeutil.LabelDeployment: "chat",
				kubeutil.LabelRevision: "rev-2", kubeutil.LabelBackend: "vllm",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodFailed, Reason: "Evicted", Message: "Preempted by a higher priority workload",
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "runtime", RestartCount: 1,
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}},
			}},
		},
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	registry := prometheus.NewRegistry()
	registry.MustRegister(NewDeploymentCollector(kubeClient))
	want := `
# HELP inferscale_runtime_oom_total Observed OOM terminations in managed inference worker Pods.
# TYPE inferscale_runtime_oom_total counter
inferscale_runtime_oom_total{backend="vllm",deployment="chat",namespace="tenant-acme",revision="rev-2",tenant="tenant-1"} 1
# HELP inferscale_runtime_preemptions_total Observed Kubernetes preemptions of managed inference worker Pods.
# TYPE inferscale_runtime_preemptions_total counter
inferscale_runtime_preemptions_total{backend="vllm",deployment="chat",namespace="tenant-acme",revision="rev-2",tenant="tenant-1"} 1
`
	if err := testutil.GatherAndCompare(
		registry, strings.NewReader(want), "inferscale_runtime_oom_total", "inferscale_runtime_preemptions_total",
	); err != nil {
		t.Fatal(err)
	}
	// A subsequent scrape of the same Pod status must not double count either
	// observed transition.
	if err := testutil.GatherAndCompare(
		registry, strings.NewReader(want), "inferscale_runtime_oom_total", "inferscale_runtime_preemptions_total",
	); err != nil {
		t.Fatal(err)
	}
}
