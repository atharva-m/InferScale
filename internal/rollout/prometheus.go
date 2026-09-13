package rollout

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	platformmetrics "github.com/inferscale/inferscale/internal/metrics"
)

const (
	defaultRegressionWindow = time.Minute
	defaultFreshness        = 2 * time.Minute
	maximumClockSkew        = 30 * time.Second
)

type PrometheusQuerier interface {
	Query(context.Context, string, time.Time) ([]platformmetrics.Sample, error)
}

// PrometheusProvider reads backend-neutral request telemetry from Prometheus
// and reads restart/OOM evidence directly from the Kubernetes API. Pod status
// is authoritative for worker lifecycle safety; rollout correctness does not
// depend on kube-state-metrics being installed or scraped.
type PrometheusProvider struct {
	Client     PrometheusQuerier
	Kubernetes client.Reader
	// Window is the short evaluation window for SLO and regression signals.
	Window time.Duration
	// Freshness is the maximum accepted age for Prometheus source series.
	Freshness time.Duration
}

// Snapshot retains the original single-window contract for callers that do
// not know the current stage transition time. Controllers should use
// SnapshotForStage so the minimum-request gate covers the complete stage.
func (p PrometheusProvider) Snapshot(ctx context.Context, namespace, stableRevision, candidateRevision string, now time.Time) (Metrics, error) {
	window := p.regressionWindow()
	return p.snapshot(ctx, namespace, stableRevision, candidateRevision, now.Add(-window), now)
}

// SnapshotForStage accumulates candidate samples and GPU XID evidence from the
// beginning of the current rollout stage while keeping latency/error
// regressions on the configured short evaluation window.
func (p PrometheusProvider) SnapshotForStage(
	ctx context.Context,
	namespace, stableRevision, candidateRevision string,
	stageStarted, now time.Time,
) (Metrics, error) {
	if stageStarted.IsZero() {
		return Metrics{}, fmt.Errorf("rollout stage start time is required")
	}
	if stageStarted.After(now) {
		return Metrics{}, fmt.Errorf("rollout stage start time is after evaluation time")
	}
	return p.snapshot(ctx, namespace, stableRevision, candidateRevision, stageStarted, now)
}

// SnapshotForRolloutStage selects the telemetry contract that actually sees
// traffic in the requested stage. Gateway API mirrors target the candidate
// runtime Service directly, bypassing llm-d/EPP, while weighted canaries pass
// through each revision's EPP. Keeping the two contracts explicit prevents a
// Shadow rollout from waiting forever on candidate EPP counters that cannot
// increase.
func (p PrometheusProvider) SnapshotForRolloutStage(
	ctx context.Context,
	namespace, stableRevision, candidateRevision string,
	stage Stage,
	stageStarted, now time.Time,
) (Metrics, error) {
	if stageStarted.IsZero() {
		return Metrics{}, fmt.Errorf("rollout stage start time is required")
	}
	if stageStarted.After(now) {
		return Metrics{}, fmt.Errorf("rollout stage start time is after evaluation time")
	}
	if stage == StageShadow {
		return p.shadowSnapshot(ctx, namespace, stableRevision, candidateRevision, stageStarted, now)
	}
	return p.snapshot(ctx, namespace, stableRevision, candidateRevision, stageStarted, now)
}

func (p PrometheusProvider) snapshot(
	ctx context.Context,
	namespace, stableRevision, candidateRevision string,
	stageStarted, now time.Time,
) (Metrics, error) {
	if p.Client == nil {
		return Metrics{}, fmt.Errorf("Prometheus client is not configured")
	}
	if p.Kubernetes == nil {
		return Metrics{}, fmt.Errorf("Kubernetes client is not configured")
	}
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(stableRevision) == "" || strings.TrimSpace(candidateRevision) == "" {
		return Metrics{}, fmt.Errorf("rollout namespace and revision names are required")
	}

	regressionSelector := durationSelector(p.regressionWindow())
	stageSelector := durationSelector(now.Sub(stageStarted))
	stableEPP := kubeutil.ResourceName(stableRevision, "epp")
	candidateEPP := kubeutil.ResourceName(candidateRevision, "epp")

	pods, podMetrics, err := p.observeCandidatePods(ctx, namespace, candidateRevision)
	if err != nil {
		return Metrics{}, err
	}
	podMetrics.Source = MetricsSourceEndpointPicker
	unavailable := func(err error) (Metrics, error) {
		podMetrics.Available = false
		podMetrics.UnavailableReason = err.Error()
		return podMetrics, nil
	}
	if err := p.ensureServiceFresh(ctx, namespace, stableEPP, now); err != nil {
		return unavailable(fmt.Errorf("stable rollout telemetry is unavailable: %w", err))
	}
	if err := p.ensureServiceFresh(ctx, namespace, candidateEPP, now); err != nil {
		return unavailable(fmt.Errorf("candidate rollout telemetry is unavailable: %w", err))
	}

	value := func(query string) (float64, error) {
		return p.queryValue(ctx, query, now)
	}
	requests, err := value(fmt.Sprintf(
		"sum(increase(inferscale_requests_total{namespace=%s,service=%s,outcome=%s}%s))",
		promString(namespace), promString(candidateEPP), promString("total"), stageSelector,
	))
	if err != nil {
		return unavailable(fmt.Errorf("query candidate sample count: %w", err))
	}
	stableError, err := value(errorRateQuery(namespace, stableEPP, regressionSelector))
	if err != nil {
		return unavailable(fmt.Errorf("query stable error rate: %w", err))
	}
	candidateError, err := value(errorRateQuery(namespace, candidateEPP, regressionSelector))
	if err != nil {
		return unavailable(fmt.Errorf("query candidate error rate: %w", err))
	}
	stableTTFT, err := value(histogramQuery("inferscale_ttft_seconds_bucket", namespace, stableEPP, regressionSelector))
	if err != nil {
		return unavailable(fmt.Errorf("query stable TTFT: %w", err))
	}
	candidateTTFT, err := value(histogramQuery("inferscale_ttft_seconds_bucket", namespace, candidateEPP, regressionSelector))
	if err != nil {
		return unavailable(fmt.Errorf("query candidate TTFT: %w", err))
	}
	stableTPOT, err := value(histogramQuery("inferscale_tpot_seconds_bucket", namespace, stableEPP, regressionSelector))
	if err != nil {
		return unavailable(fmt.Errorf("query stable TPOT: %w", err))
	}
	candidateTPOT, err := value(histogramQuery("inferscale_tpot_seconds_bucket", namespace, candidateEPP, regressionSelector))
	if err != nil {
		return unavailable(fmt.Errorf("query candidate TPOT: %w", err))
	}
	stableQueue, err := value(histogramQuery("inferscale_router_flow_control_wait_seconds_bucket", namespace, stableEPP, regressionSelector))
	if err != nil {
		return unavailable(fmt.Errorf("query stable queue latency: %w", err))
	}
	candidateQueue, err := value(histogramQuery("inferscale_router_flow_control_wait_seconds_bucket", namespace, candidateEPP, regressionSelector))
	if err != nil {
		return unavailable(fmt.Errorf("query candidate queue latency: %w", err))
	}

	podMatcher := podNameMatcher(pods)
	dcgmSelector := fmt.Sprintf("{namespace=%s,pod=~%s}", promString(namespace), promString(podMatcher))
	dcgmAge, err := value("time()-max(timestamp(DCGM_FI_DEV_XID_ERRORS" + dcgmSelector + "))")
	if err != nil {
		return unavailable(fmt.Errorf("candidate DCGM XID telemetry is unavailable: %w", err))
	}
	if err := validateSourceAge(dcgmAge, p.freshness()); err != nil {
		return unavailable(fmt.Errorf("candidate DCGM XID telemetry is unavailable: %w", err))
	}
	xidErrors, err := value(fmt.Sprintf(
		"sum(increase(DCGM_FI_DEV_XID_ERRORS%s%s))",
		dcgmSelector, stageSelector,
	))
	if err != nil {
		return unavailable(fmt.Errorf("query candidate GPU XID errors: %w", err))
	}

	return Metrics{
		Source: MetricsSourceEndpointPicker, Available: true, CandidateRequests: int64(requests),
		StableErrorRate: stableError, CandidateErrorRate: candidateError,
		StableTTFTP95MS: stableTTFT, CandidateTTFTP95MS: candidateTTFT,
		StableTPOTP95MS: stableTPOT, CandidateTPOTP95MS: candidateTPOT,
		StableQueueP95MS: stableQueue, CandidateQueueP95MS: candidateQueue,
		CandidateOOMs: podMetrics.CandidateOOMs, CandidateXIDErrors: int64(math.Ceil(xidErrors)), CandidateRestarts: podMetrics.CandidateRestarts,
	}, nil
}

func (p PrometheusProvider) shadowSnapshot(
	ctx context.Context,
	namespace, stableRevision, candidateRevision string,
	stageStarted, now time.Time,
) (Metrics, error) {
	if p.Client == nil {
		return Metrics{}, fmt.Errorf("Prometheus client is not configured")
	}
	if p.Kubernetes == nil {
		return Metrics{}, fmt.Errorf("Kubernetes client is not configured")
	}
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(stableRevision) == "" || strings.TrimSpace(candidateRevision) == "" {
		return Metrics{}, fmt.Errorf("rollout namespace and revision names are required")
	}

	pods, podMetrics, err := p.observeCandidatePods(ctx, namespace, candidateRevision)
	if err != nil {
		return Metrics{}, err
	}
	podMetrics.Source = MetricsSourceShadowRuntime
	unavailable := func(err error) (Metrics, error) {
		podMetrics.Available = false
		podMetrics.UnavailableReason = err.Error()
		return podMetrics, nil
	}
	stableService := kubeutil.ResourceName(stableRevision, "runtime")
	candidateService := kubeutil.ResourceName(candidateRevision, "runtime")
	if err := p.ensureShadowServiceFresh(ctx, namespace, stableService, now); err != nil {
		return unavailable(fmt.Errorf("stable shadow runtime telemetry is unavailable: %w", err))
	}
	if err := p.ensureShadowServiceFresh(ctx, namespace, candidateService, now); err != nil {
		return unavailable(fmt.Errorf("candidate shadow runtime telemetry is unavailable: %w", err))
	}

	value := func(query string) (float64, error) { return p.queryValue(ctx, query, now) }
	regressionSelector := durationSelector(p.regressionWindow())
	stageSelector := durationSelector(now.Sub(stageStarted))
	requests, err := value(fmt.Sprintf(
		"sum(increase(inferscale_shadow_runtime_requests_total{namespace=%s,service=%s,outcome=%s}%s))",
		promString(namespace), promString(candidateService), promString("total"), stageSelector,
	))
	if err != nil {
		return unavailable(fmt.Errorf("query candidate shadow sample count: %w", err))
	}
	stableError, err := value(shadowErrorRateQuery(namespace, stableService, regressionSelector))
	if err != nil {
		return unavailable(fmt.Errorf("query stable shadow error rate: %w", err))
	}
	candidateError, err := value(shadowErrorRateQuery(namespace, candidateService, regressionSelector))
	if err != nil {
		return unavailable(fmt.Errorf("query candidate shadow error rate: %w", err))
	}
	stableTTFT, err := value(shadowHistogramQuery("inferscale_shadow_runtime_ttft_seconds_bucket", namespace, stableService, regressionSelector))
	if err != nil {
		return unavailable(fmt.Errorf("query stable shadow TTFT: %w", err))
	}
	candidateTTFT, err := value(shadowHistogramQuery("inferscale_shadow_runtime_ttft_seconds_bucket", namespace, candidateService, regressionSelector))
	if err != nil {
		return unavailable(fmt.Errorf("query candidate shadow TTFT: %w", err))
	}
	stableTPOT, err := value(shadowHistogramQuery("inferscale_shadow_runtime_tpot_seconds_bucket", namespace, stableService, regressionSelector))
	if err != nil {
		return unavailable(fmt.Errorf("query stable shadow TPOT: %w", err))
	}
	candidateTPOT, err := value(shadowHistogramQuery("inferscale_shadow_runtime_tpot_seconds_bucket", namespace, candidateService, regressionSelector))
	if err != nil {
		return unavailable(fmt.Errorf("query candidate shadow TPOT: %w", err))
	}

	podMatcher := podNameMatcher(pods)
	dcgmSelector := fmt.Sprintf("{namespace=%s,pod=~%s}", promString(namespace), promString(podMatcher))
	dcgmAge, err := value("time()-max(timestamp(DCGM_FI_DEV_XID_ERRORS" + dcgmSelector + "))")
	if err != nil {
		return unavailable(fmt.Errorf("candidate DCGM XID telemetry is unavailable: %w", err))
	}
	if err := validateSourceAge(dcgmAge, p.freshness()); err != nil {
		return unavailable(fmt.Errorf("candidate DCGM XID telemetry is unavailable: %w", err))
	}
	xidErrors, err := value(fmt.Sprintf("sum(increase(DCGM_FI_DEV_XID_ERRORS%s%s))", dcgmSelector, stageSelector))
	if err != nil {
		return unavailable(fmt.Errorf("query candidate GPU XID errors: %w", err))
	}

	return Metrics{
		Source: MetricsSourceShadowRuntime, Available: true, CandidateRequests: int64(requests),
		StableErrorRate: stableError, CandidateErrorRate: candidateError,
		StableTTFTP95MS: stableTTFT, CandidateTTFTP95MS: candidateTTFT,
		StableTPOTP95MS: stableTPOT, CandidateTPOTP95MS: candidateTPOT,
		CandidateOOMs: podMetrics.CandidateOOMs, CandidateXIDErrors: int64(math.Ceil(xidErrors)), CandidateRestarts: podMetrics.CandidateRestarts,
	}, nil
}

func (p PrometheusProvider) observeCandidatePods(ctx context.Context, namespace, candidateRevision string) ([]corev1.Pod, Metrics, error) {
	podList := &corev1.PodList{}
	if err := p.Kubernetes.List(
		ctx,
		podList,
		client.InNamespace(namespace),
		client.MatchingLabels{
			kubeutil.LabelManagedBy: kubeutil.ManagedByValue,
			kubeutil.LabelRevision:  candidateRevision,
			kubeutil.LabelComponent: "model-server",
		},
	); err != nil {
		return nil, Metrics{}, fmt.Errorf("list candidate model-server pods: %w", err)
	}
	if len(podList.Items) == 0 {
		return nil, Metrics{}, fmt.Errorf("no candidate model-server pods are observable")
	}

	metrics := Metrics{}
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Status.Reason == "OOMKilled" {
			metrics.CandidateOOMs++
		}
		for _, statuses := range [][]corev1.ContainerStatus{
			pod.Status.InitContainerStatuses,
			pod.Status.ContainerStatuses,
			pod.Status.EphemeralContainerStatuses,
		} {
			for j := range statuses {
				status := &statuses[j]
				metrics.CandidateRestarts += int64(status.RestartCount)
				if containerOOMKilled(status) {
					metrics.CandidateOOMs++
				}
			}
		}
	}
	return podList.Items, metrics, nil
}

func containerOOMKilled(status *corev1.ContainerStatus) bool {
	return status.State.Terminated != nil && status.State.Terminated.Reason == "OOMKilled" ||
		status.LastTerminationState.Terminated != nil && status.LastTerminationState.Terminated.Reason == "OOMKilled"
}

func (p PrometheusProvider) ensureServiceFresh(ctx context.Context, namespace, service string, now time.Time) error {
	selector := fmt.Sprintf("namespace=%s,service=%s", promString(namespace), promString(service))
	query := "time()-min(" +
		"timestamp(inferscale_requests_total{" + selector + ",outcome=~\"total|error\"}) or " +
		"timestamp(inferscale_ttft_seconds_bucket{" + selector + "}) or " +
		"timestamp(inferscale_tpot_seconds_bucket{" + selector + "}) or " +
		"timestamp(inferscale_router_flow_control_wait_seconds_bucket{" + selector + "})" +
		")"
	age, err := p.queryValue(ctx, query, now)
	if err != nil {
		return err
	}
	return validateSourceAge(age, p.freshness())
}

func (p PrometheusProvider) ensureShadowServiceFresh(ctx context.Context, namespace, service string, now time.Time) error {
	selector := fmt.Sprintf("namespace=%s,service=%s", promString(namespace), promString(service))
	query := "time()-min(" +
		"timestamp(inferscale_shadow_runtime_requests_total{" + selector + ",outcome=~\"total|error\"}) or " +
		"timestamp(inferscale_shadow_runtime_ttft_seconds_bucket{" + selector + "}) or " +
		"timestamp(inferscale_shadow_runtime_tpot_seconds_bucket{" + selector + "})" +
		")"
	age, err := p.queryValue(ctx, query, now)
	if err != nil {
		return err
	}
	return validateSourceAge(age, p.freshness())
}

func (p PrometheusProvider) queryValue(ctx context.Context, query string, now time.Time) (float64, error) {
	samples, err := p.Client.Query(ctx, query, now)
	if err != nil {
		return 0, err
	}
	if len(samples) != 1 {
		return 0, fmt.Errorf("rollout query returned %d samples, want exactly one", len(samples))
	}
	observedAt := samples[0].Timestamp
	if observedAt.IsZero() {
		return 0, fmt.Errorf("rollout query returned a sample without a timestamp")
	}
	if observedAt.After(now.Add(maximumClockSkew)) {
		return 0, fmt.Errorf("rollout query sample timestamp is in the future")
	}
	if now.Sub(observedAt) > p.freshness() {
		return 0, fmt.Errorf("rollout query sample is stale by %s", now.Sub(observedAt).Round(time.Second))
	}
	return samples[0].Value, nil
}

func validateSourceAge(age float64, maximum time.Duration) error {
	if math.IsNaN(age) || math.IsInf(age, 0) || age < -maximumClockSkew.Seconds() {
		return fmt.Errorf("invalid telemetry source age %.3fs", age)
	}
	if age > maximum.Seconds() {
		return fmt.Errorf("telemetry source is stale by %s", (time.Duration(age * float64(time.Second))).Round(time.Second))
	}
	return nil
}

func (p PrometheusProvider) regressionWindow() time.Duration {
	if p.Window <= 0 {
		return defaultRegressionWindow
	}
	return p.Window
}

func (p PrometheusProvider) freshness() time.Duration {
	if p.Freshness <= 0 {
		return defaultFreshness
	}
	return p.Freshness
}

func durationSelector(window time.Duration) string {
	seconds := int64(math.Ceil(window.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return fmt.Sprintf("[%ds]", seconds)
}

func podNameMatcher(pods []corev1.Pod) string {
	names := make([]string, 0, len(pods))
	for i := range pods {
		names = append(names, regexp.QuoteMeta(pods[i].Name))
	}
	sort.Strings(names)
	return "^(" + strings.Join(names, "|") + ")$"
}

func errorRateQuery(namespace, service, window string) string {
	failures := fmt.Sprintf("sum(increase(inferscale_requests_total{namespace=%s,service=%s,outcome=%s}%s))", promString(namespace), promString(service), promString("error"), window)
	total := fmt.Sprintf("sum(increase(inferscale_requests_total{namespace=%s,service=%s,outcome=%s}%s))", promString(namespace), promString(service), promString("total"), window)
	return failures + "/clamp_min(" + total + ",1)"
}

func histogramQuery(metric, namespace, service, window string) string {
	return fmt.Sprintf(
		"histogram_quantile(0.95,sum(rate(%s{namespace=%s,service=%s}%s)) by (le))*1000",
		metric, promString(namespace), promString(service), window,
	)
}

func shadowErrorRateQuery(namespace, service, window string) string {
	failures := fmt.Sprintf("sum(increase(inferscale_shadow_runtime_requests_total{namespace=%s,service=%s,outcome=%s}%s))", promString(namespace), promString(service), promString("error"), window)
	total := fmt.Sprintf("sum(increase(inferscale_shadow_runtime_requests_total{namespace=%s,service=%s,outcome=%s}%s))", promString(namespace), promString(service), promString("total"), window)
	return failures + "/clamp_min(" + total + ",1)"
}

func shadowHistogramQuery(metric, namespace, service, window string) string {
	return fmt.Sprintf(
		"histogram_quantile(0.95,sum(rate(%s{namespace=%s,service=%s}%s)) by (le))*1000",
		metric, promString(namespace), promString(service), window,
	)
}

func promString(value string) string { return strconv.Quote(value) }
