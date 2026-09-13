package benchmark

import (
	"context"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKubernetesJobSubmitterIsIdempotent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	tokens, _ := NewCallbackTokens([]byte("0123456789abcdef0123456789abcdef"))
	submitter, err := NewKubernetesJobSubmitter(kubeClient, tokens, KubernetesJobSubmitterOptions{
		Image: "runner@sha256:" + strings.Repeat("b", 64), CallbackBaseURL: "http://inferscale-api.system.svc:8080",
		Authority: ExecutionAuthority{
			Provider: "vast", InferenceBaseURL: "https://gateway.example",
			PrometheusURL: "http://prometheus.monitoring.svc:9090",
			DriverVersion: "580.10", CUDAVersion: "13.0",
			RuntimeVersions: map[Backend]string{BackendVLLM: "0.23.0"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	run := Run{
		ID: "019c1234-1234-7123-8123-123456789abc", DeploymentID: "dep", DeploymentName: "qwen-chat",
		RevisionID: "rev", Namespace: "tenant-a", ScenarioName: "smoke", RentalPriceUSD: 1.75,
		RuntimePodPrefix: "qwen-chat-a8f32-vllm",
		EPPService:       "qwen-chat-a8f32-epp",
		Configuration:    []byte(`{"schema_version":1,"target":{"base_url":"https://attacker.example"},"deployment":{},"workload":{"input_tokens":128,"output_tokens":32,"concurrency":1,"requests":4,"dataset":"../workloads/smoke.jsonl","seed":7},"cache_state":"process-warm","routing":{"policy":"round-robin"}}`),
		Serving: ServingContract{
			ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: strings.Repeat("a", 40), Backend: BackendVLLM,
			RuntimeImageDigest: "sha256:" + strings.Repeat("c", 64), GPUSKU: "RTX_5090", GPUCount: 1,
			Precision: "bf16", Quantization: "none", TensorParallelism: 1, MaxContextBucket: 8192,
			RoutingPolicy: "round-robin",
		},
	}
	prepared, err := submitter.Prepare(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := submitter.Submit(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	if err := submitter.Submit(context.Background(), run); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	var jobs batchv1.JobList
	if err := kubeClient.List(context.Background(), &jobs, client.InNamespace(run.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 {
		t.Fatalf("jobs = %d", len(jobs.Items))
	}
	job := jobs.Items[0]
	if job.Spec.Template.Labels["app.kubernetes.io/name"] != "inferscale-benchmark" {
		t.Fatal("benchmark Pod is not selected by callback NetworkPolicies")
	}
	if job.Spec.Template.Spec.AutomountServiceAccountToken == nil || *job.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("benchmark Job received a service account token")
	}
	args := job.Spec.Template.Spec.Containers[0].Args
	wantArgs := []string{
		"--deployment-id", run.DeploymentID, "--gpu-hourly-price", "1.75",
		"--deployment-namespace", run.Namespace, "--runtime-pod-prefix", run.RuntimePodPrefix,
		"--epp-service", run.EPPService,
		"--provider", "vast", "--runtime-version", "0.23.0",
		"--container-image-digest", run.Serving.RuntimeImageDigest,
		"--driver-version", "580.10", "--cuda-version", "13.0",
		"--prometheus-url", "http://prometheus.monitoring.svc:9090",
		"--configuration-digest", job.Annotations["inferscale.io/execution-digest"],
		"--selection-scenario-digest", prepared.Execution.SelectionScenarioDigest,
	}
	podSecurity := job.Spec.Template.Spec.SecurityContext
	if podSecurity == nil || podSecurity.RunAsUser == nil || *podSecurity.RunAsUser != benchmarkRunnerIdentity ||
		podSecurity.RunAsGroup == nil || *podSecurity.RunAsGroup != benchmarkRunnerIdentity ||
		podSecurity.FSGroup == nil || *podSecurity.FSGroup != benchmarkRunnerIdentity {
		t.Fatalf("benchmark Pod cannot read its group-scoped Secret/PVC: %#v", podSecurity)
	}
	secretVolume := job.Spec.Template.Spec.Volumes[0].Secret
	if secretVolume == nil || secretVolume.DefaultMode == nil || *secretVolume.DefaultMode != 0440 {
		t.Fatalf("benchmark Secret mode = %#v, want 0440", secretVolume)
	}
	for index := 0; index < len(wantArgs); index += 2 {
		found := false
		for position := 0; position+1 < len(args); position++ {
			if args[position] == wantArgs[index] && args[position+1] == wantArgs[index+1] {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("benchmark Job args %v do not contain %v", args, wantArgs[index:index+2])
		}
	}
	var secret corev1.Secret
	if err := kubeClient.Get(context.Background(), client.ObjectKey{Namespace: run.Namespace, Name: "benchmark-" + run.ID + "-callback"}, &secret); err != nil {
		t.Fatal(err)
	}
	if err := tokens.Verify(run.ID, string(secret.Data["token"])); err != nil {
		t.Fatalf("stored callback token: %v", err)
	}
	if secret.Annotations["inferscale.io/execution-digest"] == "" {
		t.Fatal("Secret does not bind the server-authored execution configuration")
	}
	if strings.Contains(string(secret.Data["scenario.json"]), "attacker.example") ||
		!strings.Contains(string(secret.Data["scenario.json"]), "gateway.example") {
		t.Fatalf("scenario target was not replaced authoritatively: %s", secret.Data["scenario.json"])
	}
}
