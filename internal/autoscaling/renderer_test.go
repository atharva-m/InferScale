package autoscaling

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestRendererUsesTwoAverageValuePrometheusTriggers(t *testing.T) {
	t.Parallel()
	object, err := (Renderer{}).Render(RenderConfig{
		Namespace: "tenant-a", DeploymentName: "chat", Revision: "chat-a8f32",
		TargetDeployment: "chat-a8f32-vllm", InferencePool: "chat-a8f32-pool",
		EndpointPickerSvc: "chat-a8f32-epp", PrometheusURL: "http://prometheus.monitoring:9090",
		MinReplicas: 0, MaxReplicas: 8, Policy: DefaultPolicy(32, 128, 8),
	})
	if err != nil {
		t.Fatal(err)
	}
	triggers, found, err := unstructured.NestedSlice(object.Object, "spec", "triggers")
	if err != nil || !found || len(triggers) != 2 {
		t.Fatalf("triggers=%#v, found=%v, err=%v", triggers, found, err)
	}
	for _, raw := range triggers {
		trigger := raw.(map[string]any)
		if trigger["metricType"] != "AverageValue" {
			t.Fatalf("metricType=%v", trigger["metricType"])
		}
		query := trigger["metadata"].(map[string]any)["query"].(string)
		if !strings.HasPrefix(query, "sum(") {
			t.Fatalf("query %q does not reduce to a scalar", query)
		}
	}
	queueMetadata := triggers[0].(map[string]any)["metadata"].(map[string]any)
	if queueMetadata["activationThreshold"] != "0" {
		t.Fatalf("queue activation threshold = %v, want 0 so one queued request activates workers", queueMetadata["activationThreshold"])
	}
	queueQuery := queueMetadata["query"].(string)
	if !strings.Contains(queueQuery, `service="chat-a8f32-epp"`) || strings.Contains(queueQuery, "inference_pool=") {
		t.Fatalf("queue query must use the Prometheus scrape service label: %q", queueQuery)
	}
	fallback, found, err := unstructured.NestedMap(object.Object, "spec", "fallback")
	if err != nil || !found || fallback["replicas"] != int64(1) || fallback["failureThreshold"] != int64(3) {
		t.Fatalf("missing-metric fallback = %#v, found=%v, err=%v", fallback, found, err)
	}
}

func TestDefaultPolicyMatchesV1ScalingContract(t *testing.T) {
	t.Parallel()
	policy := DefaultPolicy(33, 128, 8)
	if policy.RunningTarget != 5 {
		t.Fatalf("running target = %d, want ceil(33/8)=5", policy.RunningTarget)
	}
	if policy.PollingIntervalSeconds != 5 || policy.CooldownPeriodSeconds != 300 || policy.ScaleDownStabilizationSeconds != 600 {
		t.Fatalf("timing policy = %#v", policy)
	}
}
