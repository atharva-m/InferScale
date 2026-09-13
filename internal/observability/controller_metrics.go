package observability

import (
	"context"
	"strings"
	"sync"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DeploymentCollector projects bounded CR status into Prometheus without
// coupling telemetry to individual reconciliation branches. A scrape observes
// one coherent status snapshot and never includes condition messages or other
// user-controlled text as labels.
type DeploymentCollector struct {
	client client.Client

	collectionSuccess  *prometheus.Desc
	ready              *prometheus.Desc
	replicas           *prometheus.Desc
	rolloutStage       *prometheus.Desc
	rolloutWeight      *prometheus.Desc
	gpuSeconds         *prometheus.Desc
	runtimeOOMs        *prometheus.Desc
	runtimePreemptions *prometheus.Desc
	now                func() time.Time
	gpuMu              sync.Mutex
	gpuCounters        map[gpuAllocationKey]gpuAllocationCounter
	eventMu            sync.Mutex
	runtimeEvents      map[runtimeEventKey]runtimeEventCounter
	podEvents          map[string]podEventObservation
}

type gpuAllocationKey struct {
	namespace, tenant, deployment, revision, backend, billingScope string
}

type gpuAllocationCounter struct {
	value      float64
	allocation float64
	last       time.Time
	zeroSince  time.Time
}

type servingAllocation struct {
	stable           string
	candidate        string
	candidateServing bool
}

type runtimeEventKey struct {
	namespace, tenant, deployment, revision, backend string
}

type runtimeEventCounter struct {
	ooms, preemptions float64
	lastSeen          time.Time
}

type podEventObservation struct {
	restarts       map[string]int32
	preemptionSeen bool
	lastSeen       time.Time
}

func NewDeploymentCollector(kubeClient client.Client) *DeploymentCollector {
	resourceLabels := []string{"namespace", "tenant", "deployment", "revision", "backend"}
	return &DeploymentCollector{
		client: kubeClient,
		collectionSuccess: prometheus.NewDesc(
			"inferscale_controller_status_collection_success",
			"Whether the controller could list InferenceDeployment status for this scrape.", nil, nil,
		),
		ready: prometheus.NewDesc(
			"inferscale_deployment_ready", "Whether the deployment status is Ready.", resourceLabels, nil,
		),
		replicas: prometheus.NewDesc(
			"inferscale_deployment_replicas", "Deployment replicas by desired or ready state.",
			append(resourceLabels, "state"), nil,
		),
		rolloutStage: prometheus.NewDesc(
			"inferscale_rollout_stage", "Current rollout stage (one active series per deployment).",
			append(resourceLabels, "candidate_revision", "stage"), nil,
		),
		rolloutWeight: prometheus.NewDesc(
			"inferscale_rollout_traffic_weight", "Configured stable or candidate traffic percentage.",
			append(resourceLabels, "candidate_revision", "role"), nil,
		),
		gpuSeconds: prometheus.NewDesc(
			"inferscale_gpu_seconds_total", "Cumulative scheduled exclusive GPU allocation in seconds.",
			[]string{"namespace", "tenant", "deployment", "revision", "backend", "billing_scope"}, nil,
		),
		runtimeOOMs: prometheus.NewDesc(
			"inferscale_runtime_oom_total", "Observed OOM terminations in managed inference worker Pods.", resourceLabels, nil,
		),
		runtimePreemptions: prometheus.NewDesc(
			"inferscale_runtime_preemptions_total", "Observed Kubernetes preemptions of managed inference worker Pods.", resourceLabels, nil,
		),
		now:           time.Now,
		gpuCounters:   make(map[gpuAllocationKey]gpuAllocationCounter),
		runtimeEvents: make(map[runtimeEventKey]runtimeEventCounter),
		podEvents:     make(map[string]podEventObservation),
	}
}

func (c *DeploymentCollector) Describe(output chan<- *prometheus.Desc) {
	output <- c.collectionSuccess
	output <- c.ready
	output <- c.replicas
	output <- c.rolloutStage
	output <- c.rolloutWeight
	output <- c.gpuSeconds
	output <- c.runtimeOOMs
	output <- c.runtimePreemptions
}

func (c *DeploymentCollector) Collect(output chan<- prometheus.Metric) {
	if c.client == nil {
		output <- prometheus.MustNewConstMetric(c.collectionSuccess, prometheus.GaugeValue, 0)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var resources platformv1alpha1.InferenceDeploymentList
	if err := c.client.List(ctx, &resources); err != nil {
		output <- prometheus.MustNewConstMetric(c.collectionSuccess, prometheus.GaugeValue, 0)
		return
	}
	var pods corev1.PodList
	if err := c.client.List(ctx, &pods, client.MatchingLabels{kubeutil.LabelManagedBy: kubeutil.ManagedByValue}); err != nil {
		output <- prometheus.MustNewConstMetric(c.collectionSuccess, prometheus.GaugeValue, 0)
		return
	}
	output <- prometheus.MustNewConstMetric(c.collectionSuccess, prometheus.GaugeValue, 1)
	servingByDeployment := make(map[string]servingAllocation, len(resources.Items))
	for index := range resources.Items {
		resource := &resources.Items[index]
		servingByDeployment[resource.Namespace+"\x00"+resource.Name] = servingAllocation{
			stable:           resource.Status.Revision.Stable,
			candidate:        resource.Status.Revision.Candidate,
			candidateServing: resource.Status.Rollout.CandidateWeight > 0,
		}
		tenant := nonEmpty(resource.Labels[kubeutil.LabelTenant], "unknown")
		revision := nonEmpty(resource.Status.Revision.Candidate, resource.Status.Revision.Stable, "none")
		backend := nonEmpty(string(resource.Status.Runtime.Backend), string(resource.Spec.Runtime.Backend), "unknown")
		labels := []string{resource.Namespace, tenant, resource.Name, revision, backend}
		ready := 0.0
		if resource.Status.Phase == platformv1alpha1.DeploymentPhaseReady {
			ready = 1
		}
		output <- prometheus.MustNewConstMetric(c.ready, prometheus.GaugeValue, ready, labels...)
		output <- prometheus.MustNewConstMetric(c.replicas, prometheus.GaugeValue, float64(resource.Status.Replicas.Desired), withLabels(labels, "desired")...)
		output <- prometheus.MustNewConstMetric(c.replicas, prometheus.GaugeValue, float64(resource.Status.Replicas.Ready), withLabels(labels, "ready")...)

		stage := nonEmpty(resource.Status.Rollout.Stage, "none")
		candidate := nonEmpty(resource.Status.Revision.Candidate, "none")
		rolloutLabels := withLabels(labels, candidate)
		output <- prometheus.MustNewConstMetric(c.rolloutStage, prometheus.GaugeValue, 1, withLabels(rolloutLabels, stage)...)
		candidateWeight := resource.Status.Rollout.CandidateWeight
		if candidateWeight < 0 {
			candidateWeight = 0
		}
		if candidateWeight > 100 {
			candidateWeight = 100
		}
		output <- prometheus.MustNewConstMetric(c.rolloutWeight, prometheus.GaugeValue, float64(candidateWeight), withLabels(rolloutLabels, "candidate")...)
		output <- prometheus.MustNewConstMetric(c.rolloutWeight, prometheus.GaugeValue, float64(100-candidateWeight), withLabels(rolloutLabels, "stable")...)
	}
	c.collectGPUSeconds(output, pods.Items, servingByDeployment)
	c.collectRuntimeEvents(output, pods.Items)
}

// collectRuntimeEvents turns the bounded Kubernetes Pod status already read by
// the controller into monotonic operational counters. It intentionally never
// exposes status messages as metric labels. OOM attribution uses the last
// terminated container reason. Preemption attribution requires Kubernetes to
// report either reason=Preempted or an Evicted status whose message explicitly
// identifies scheduler preemption; ordinary node-pressure evictions are not
// mislabeled as preemptions.
func (c *DeploymentCollector) collectRuntimeEvents(output chan<- prometheus.Metric, pods []corev1.Pod) {
	now := c.now().UTC()
	c.eventMu.Lock()
	defer c.eventMu.Unlock()

	seenPods := make(map[string]struct{}, len(pods))
	for index := range pods {
		pod := &pods[index]
		labels := pod.Labels
		if labels[kubeutil.LabelComponent] != "model-server" {
			continue
		}
		deployment := labels[kubeutil.LabelDeployment]
		revision := labels[kubeutil.LabelRevision]
		if deployment == "" || revision == "" {
			continue
		}
		uid := string(pod.UID)
		if uid == "" {
			uid = pod.Namespace + "/" + pod.Name
		}
		seenPods[uid] = struct{}{}
		key := runtimeEventKey{
			namespace: pod.Namespace, tenant: nonEmpty(labels[kubeutil.LabelTenant]),
			deployment: deployment, revision: revision, backend: nonEmpty(labels[kubeutil.LabelBackend]),
		}
		observation := c.podEvents[uid]
		if observation.restarts == nil {
			observation.restarts = make(map[string]int32)
		}
		counter := c.runtimeEvents[key]
		for _, status := range pod.Status.ContainerStatuses {
			prior := observation.restarts[status.Name]
			if status.RestartCount > prior && status.LastTerminationState.Terminated != nil &&
				status.LastTerminationState.Terminated.Reason == "OOMKilled" {
				// Kubernetes exposes only the most recent termination reason. Count
				// one observed OOM transition even if several restarts happened
				// between scrapes; never fabricate unseen events.
				counter.ooms++
			}
			if status.RestartCount > prior {
				observation.restarts[status.Name] = status.RestartCount
			}
		}
		preempted := pod.Status.Reason == "Preempted" ||
			(pod.Status.Reason == "Evicted" && strings.Contains(strings.ToLower(pod.Status.Message), "preempt"))
		if preempted && !observation.preemptionSeen {
			counter.preemptions++
			observation.preemptionSeen = true
		}
		observation.lastSeen = now
		counter.lastSeen = now
		c.podEvents[uid] = observation
		c.runtimeEvents[key] = counter
	}

	for uid, observation := range c.podEvents {
		if _, ok := seenPods[uid]; !ok && now.Sub(observation.lastSeen) > 2*time.Hour {
			delete(c.podEvents, uid)
		}
	}
	for key, counter := range c.runtimeEvents {
		if now.Sub(counter.lastSeen) > 2*time.Hour {
			delete(c.runtimeEvents, key)
			continue
		}
		labels := []string{key.namespace, key.tenant, key.deployment, key.revision, key.backend}
		output <- prometheus.MustNewConstMetric(c.runtimeOOMs, prometheus.CounterValue, counter.ooms, labels...)
		output <- prometheus.MustNewConstMetric(c.runtimePreemptions, prometheus.CounterValue, counter.preemptions, labels...)
	}
}

func (c *DeploymentCollector) collectGPUSeconds(
	output chan<- prometheus.Metric,
	pods []corev1.Pod,
	servingByDeployment map[string]servingAllocation,
) {
	current := make(map[gpuAllocationKey]float64)
	for index := range pods {
		pod := &pods[index]
		if pod.Spec.NodeName == "" || pod.DeletionTimestamp != nil ||
			pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		labels := pod.Labels
		deployment := labels[kubeutil.LabelDeployment]
		revision := labels[kubeutil.LabelRevision]
		if deployment == "" || revision == "" {
			continue
		}
		allocation := 0.0
		for containerIndex := range pod.Spec.Containers {
			quantity := pod.Spec.Containers[containerIndex].Resources.Limits[corev1.ResourceName("nvidia.com/gpu")]
			allocation += quantity.AsApproximateFloat64()
		}
		if allocation <= 0 {
			continue
		}
		scope := "platform"
		serving := servingByDeployment[pod.Namespace+"\x00"+deployment]
		// Weighted canary requests are customer traffic. Preparation, shadow
		// traffic, superseded revisions and failed candidates remain platform
		// allocation, as do builds. Missing status must not charge warmup to tenants.
		if labels[kubeutil.LabelComponent] == "model-server" &&
			(revision == serving.stable || (revision == serving.candidate && serving.candidateServing)) {
			scope = "tenant"
		}
		key := gpuAllocationKey{
			namespace: pod.Namespace, tenant: nonEmpty(labels[kubeutil.LabelTenant]), deployment: deployment,
			revision: revision, backend: nonEmpty(labels[kubeutil.LabelBackend]), billingScope: scope,
		}
		current[key] += allocation
	}

	now := c.now().UTC()
	c.gpuMu.Lock()
	defer c.gpuMu.Unlock()
	for key, state := range c.gpuCounters {
		if !state.last.IsZero() && now.After(state.last) {
			state.value += state.allocation * now.Sub(state.last).Seconds()
		}
		state.last = now
		state.allocation = current[key]
		if state.allocation == 0 {
			if state.zeroSince.IsZero() {
				state.zeroSince = now
			}
		} else {
			state.zeroSince = time.Time{}
		}
		c.gpuCounters[key] = state
		delete(current, key)
	}
	for key, allocation := range current {
		c.gpuCounters[key] = gpuAllocationCounter{allocation: allocation, last: now}
	}
	for key, state := range c.gpuCounters {
		if state.allocation == 0 && !state.zeroSince.IsZero() && now.Sub(state.zeroSince) > 2*time.Hour {
			delete(c.gpuCounters, key)
			continue
		}
		output <- prometheus.MustNewConstMetric(
			c.gpuSeconds, prometheus.CounterValue, state.value,
			key.namespace, key.tenant, key.deployment, key.revision, key.backend, key.billingScope,
		)
	}
}

func withLabels(labels []string, additional ...string) []string {
	result := make([]string, 0, len(labels)+len(additional))
	result = append(result, labels...)
	return append(result, additional...)
}

func nonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "unknown"
}
