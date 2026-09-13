package rollout

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	platformmetrics "github.com/inferscale/inferscale/internal/metrics"
)

type recordingQuerier struct {
	now     time.Time
	queries []string
	result  func(string) []platformmetrics.Sample
}

func (q *recordingQuerier) Query(_ context.Context, query string, _ time.Time) ([]platformmetrics.Sample, error) {
	q.queries = append(q.queries, query)
	if q.result != nil {
		return q.result(query), nil
	}
	return []platformmetrics.Sample{{Timestamp: q.now, Value: 1}}, nil
}

func TestPrometheusProviderUsesStageSamplesAndDirectPodSafety(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_000, 0).UTC()
	querier := &recordingQuerier{now: now}
	querier.result = func(query string) []platformmetrics.Sample {
		value := 1.0
		if strings.Contains(query, "sum(increase(DCGM_FI_DEV_XID_ERRORS") {
			value = 0.01
		}
		return []platformmetrics.Sample{{Timestamp: now, Value: value}}
	}
	pod := candidatePod("tenant-a", "chat-candidate", "chat-candidate-worker-a")
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name:         "model-server",
		RestartCount: 2,
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Reason: "OOMKilled",
		}},
	}}
	provider := PrometheusProvider{
		Client: querier, Kubernetes: podReader(t, pod),
		Window: time.Minute, Freshness: 2 * time.Minute,
	}
	metrics, err := provider.SnapshotForStage(
		context.Background(), "tenant-a", "chat-stable", "chat-candidate", now.Add(-10*time.Minute), now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.CandidateOOMs != 1 || metrics.CandidateRestarts != 2 {
		t.Fatalf("direct pod safety metrics = %#v", metrics)
	}
	// Prometheus increases can be fractional at range boundaries. A non-zero
	// safety signal must never be truncated to zero.
	if metrics.CandidateXIDErrors != 1 {
		t.Fatalf("candidate XID errors = %d, want ceil to one", metrics.CandidateXIDErrors)
	}

	joined := strings.Join(querier.queries, "\n")
	for _, raw := range []string{
		"vllm:", "llm_d_epp_flow_control_request_queue_duration_seconds_bucket",
		"kube_pod_container_status_restarts_total", "kube_pod_container_status_last_terminated_reason",
	} {
		if strings.Contains(joined, raw) {
			t.Fatalf("rollout queries contain forbidden provider dependency %q:\n%s", raw, joined)
		}
	}
	for _, canonical := range []string{
		"inferscale_requests_total", "inferscale_ttft_seconds_bucket",
		"inferscale_tpot_seconds_bucket", "inferscale_router_flow_control_wait_seconds_bucket",
		`service="chat-candidate-epp"`, `outcome="total"`,
		`increase(inferscale_requests_total{namespace="tenant-a",service="chat-candidate-epp",outcome="total"}[600s])`,
		`rate(inferscale_ttft_seconds_bucket{namespace="tenant-a",service="chat-candidate-epp"}[60s])`,
		`DCGM_FI_DEV_XID_ERRORS{namespace="tenant-a",pod=~"^(chat-candidate-worker-a)$"}[600s]`,
	} {
		if !strings.Contains(joined, canonical) {
			t.Fatalf("rollout queries omit expected contract %q:\n%s", canonical, joined)
		}
	}
}

func TestPrometheusProviderUsesRuntimeServiceTelemetryDuringShadow(t *testing.T) {
	t.Parallel()
	now := time.Unix(1_500, 0).UTC()
	querier := &recordingQuerier{now: now}
	querier.result = func(query string) []platformmetrics.Sample {
		value := 1.0
		if strings.Contains(query, "candidate-runtime") && strings.Contains(query, "shadow_runtime_requests_total") && strings.Contains(query, "[600s]") {
			value = 207
		}
		if strings.Contains(query, "sum(increase(DCGM_FI_DEV_XID_ERRORS") {
			value = 0
		}
		return []platformmetrics.Sample{{Timestamp: now, Value: value}}
	}
	provider := PrometheusProvider{
		Client: querier, Kubernetes: podReader(t, candidatePod("tenant-a", "candidate", "candidate-worker")),
		Window: time.Minute, Freshness: 2 * time.Minute,
	}
	metrics, err := provider.SnapshotForRolloutStage(
		context.Background(), "tenant-a", "stable", "candidate", StageShadow,
		now.Add(-10*time.Minute), now,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !metrics.Available || metrics.Source != MetricsSourceShadowRuntime || metrics.CandidateRequests != 207 {
		t.Fatalf("shadow metrics=%#v", metrics)
	}
	joined := strings.Join(querier.queries, "\n")
	for _, expected := range []string{
		`service="stable-runtime"`, `service="candidate-runtime"`,
		"inferscale_shadow_runtime_requests_total", "inferscale_shadow_runtime_ttft_seconds_bucket",
		"inferscale_shadow_runtime_tpot_seconds_bucket",
		`increase(inferscale_shadow_runtime_requests_total{namespace="tenant-a",service="candidate-runtime",outcome="total"}[600s])`,
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("shadow queries omit %q:\n%s", expected, joined)
		}
	}
	for _, forbidden := range []string{"candidate-epp", "inferscale_router_flow_control_wait_seconds_bucket"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("shadow queries unexpectedly depend on %q:\n%s", forbidden, joined)
		}
	}
}

func TestPrometheusProviderRejectsStaleQuerySamples(t *testing.T) {
	t.Parallel()
	now := time.Unix(2_000, 0).UTC()
	querier := &recordingQuerier{now: now.Add(-3 * time.Minute)}
	provider := PrometheusProvider{
		Client: querier, Kubernetes: podReader(t, candidatePod("tenant-a", "candidate", "candidate-worker")),
		Freshness: time.Minute,
	}
	metrics, err := provider.SnapshotForStage(context.Background(), "tenant-a", "stable", "candidate", now.Add(-10*time.Minute), now)
	if err != nil || metrics.Available || !strings.Contains(metrics.UnavailableReason, "stale") {
		t.Fatalf("stale sample result = %#v, error = %v", metrics, err)
	}
}

func TestPrometheusProviderRejectsStaleSourceSeries(t *testing.T) {
	t.Parallel()
	now := time.Unix(3_000, 0).UTC()
	querier := &recordingQuerier{now: now}
	querier.result = func(query string) []platformmetrics.Sample {
		value := 1.0
		if strings.HasPrefix(query, "time()-min(") {
			value = 121
		}
		return []platformmetrics.Sample{{Timestamp: now, Value: value}}
	}
	provider := PrometheusProvider{
		Client: querier, Kubernetes: podReader(t, candidatePod("tenant-a", "candidate", "candidate-worker")),
		Freshness: 2 * time.Minute,
	}
	metrics, err := provider.SnapshotForStage(context.Background(), "tenant-a", "stable", "candidate", now.Add(-10*time.Minute), now)
	if err != nil || metrics.Available || !strings.Contains(metrics.UnavailableReason, "telemetry source is stale") {
		t.Fatalf("stale source result = %#v, error = %v", metrics, err)
	}
}

func TestPrometheusProviderFailsClosedWhenCandidateDCGMTelemetryIsMissing(t *testing.T) {
	t.Parallel()
	now := time.Unix(4_000, 0).UTC()
	querier := &recordingQuerier{now: now}
	querier.result = func(query string) []platformmetrics.Sample {
		if strings.Contains(query, "max(timestamp(DCGM_FI_DEV_XID_ERRORS") {
			return nil
		}
		return []platformmetrics.Sample{{Timestamp: now, Value: 1}}
	}
	provider := PrometheusProvider{
		Client: querier, Kubernetes: podReader(t, candidatePod("tenant-a", "candidate", "candidate-worker")),
	}
	metrics, err := provider.SnapshotForStage(context.Background(), "tenant-a", "stable", "candidate", now.Add(-10*time.Minute), now)
	if err != nil || metrics.Available || !strings.Contains(metrics.UnavailableReason, "DCGM XID telemetry is unavailable") {
		t.Fatalf("missing DCGM result = %#v, error = %v", metrics, err)
	}
}

func TestPrometheusProviderRequiresCandidateModelServerPods(t *testing.T) {
	t.Parallel()
	now := time.Unix(5_000, 0).UTC()
	provider := PrometheusProvider{
		Client: &recordingQuerier{now: now}, Kubernetes: podReader(t,
			candidatePod("tenant-a", "different-revision", "unrelated-worker"),
		),
	}
	_, err := provider.SnapshotForStage(context.Background(), "tenant-a", "stable", "candidate", now.Add(-10*time.Minute), now)
	if err == nil || !strings.Contains(err.Error(), "no candidate model-server pods") {
		t.Fatalf("missing pod error = %v", err)
	}
}

func candidatePod(namespace, revision, name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace,
		Name:      name,
		Labels: map[string]string{
			kubeutil.LabelManagedBy: kubeutil.ManagedByValue,
			kubeutil.LabelRevision:  revision,
			kubeutil.LabelComponent: "model-server",
		},
	}}
}

func podReader(t *testing.T, pods ...*corev1.Pod) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	objects := make([]runtime.Object, 0, len(pods))
	for _, pod := range pods {
		objects = append(objects, pod)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
}
