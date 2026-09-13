package usage

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	platformmetrics "github.com/inferscale/inferscale/internal/metrics"
)

// PrometheusQuerier is the small, read-only subset required by the usage
// materializer. Callers cannot inject PromQL; all templates live in this file.
type PrometheusQuerier interface {
	Query(context.Context, string, time.Time) ([]platformmetrics.Sample, error)
}

// DeploymentResolver converts the resource identity carried by Prometheus
// (tenant ID plus immutable tenant-visible name) into the public deployment ID
// stored by PostgreSQL.
type DeploymentResolver interface {
	ResolveUsageDeployment(context.Context, string, string) (string, error)
}

type PrometheusSource struct {
	Prometheus  PrometheusQuerier
	Deployments DeploymentResolver
}

type usageKey struct {
	tenant, deployment, revision string
}

type metricKind int

const (
	requestMetric metricKind = iota
	rejectionMetric
	inputTokenMetric
	outputTokenMetric
	gpuMetric
	shadowGPUMetric
)

type usageQuery struct {
	kind  metricKind
	query string
}

func (s PrometheusSource) CompletedHour(ctx context.Context, start, end time.Time) ([]Hourly, error) {
	if s.Prometheus == nil || s.Deployments == nil {
		return nil, fmt.Errorf("Prometheus usage source is not configured")
	}
	start = start.UTC().Truncate(time.Hour)
	end = end.UTC().Truncate(time.Hour)
	if !end.After(start) {
		return nil, fmt.Errorf("usage interval must include at least one completed hour")
	}

	result := make([]Hourly, 0)
	resolved := make(map[string]string)
	for hour := start; hour.Before(end); hour = hour.Add(time.Hour) {
		values := make(map[usageKey]*Hourly)
		// Activity and allocation evidence must match at the full immutable
		// usage identity. A GPU series for one tenant/deployment/revision must
		// never make another active deployment look accounted for.
		customerActivity := make(map[usageKey]bool)
		gpuObserved := make(map[usageKey]bool)
		for _, query := range hourlyQueries() {
			samples, err := s.Prometheus.Query(ctx, query.query, hour.Add(time.Hour))
			if err != nil {
				return nil, fmt.Errorf("query %s for %s: %w", metricName(query.kind), hour.Format(time.RFC3339), err)
			}
			for _, sample := range samples {
				key, err := sampleKey(sample)
				if err != nil {
					return nil, fmt.Errorf("decode %s labels: %w", metricName(query.kind), err)
				}
				if math.IsNaN(sample.Value) || math.IsInf(sample.Value, 0) || sample.Value < 0 {
					return nil, fmt.Errorf("%s returned invalid value %v", metricName(query.kind), sample.Value)
				}
				aggregate := values[key]
				if aggregate == nil {
					aggregate = &Hourly{TenantID: key.tenant, Revision: key.revision, Hour: hour}
					values[key] = aggregate
				}
				switch query.kind {
				case requestMetric:
					aggregate.Requests += int64(math.Round(sample.Value))
					customerActivity[key] = customerActivity[key] || sample.Value > 0
				case rejectionMetric:
					aggregate.RejectedRequests += int64(math.Round(sample.Value))
				case inputTokenMetric:
					aggregate.InputTokens += int64(math.Round(sample.Value))
					customerActivity[key] = customerActivity[key] || sample.Value > 0
				case outputTokenMetric:
					aggregate.OutputTokens += int64(math.Round(sample.Value))
					customerActivity[key] = customerActivity[key] || sample.Value > 0
				case gpuMetric:
					aggregate.GPUSeconds += sample.Value
					gpuObserved[key] = gpuObserved[key] || sample.Value > 0
				case shadowGPUMetric:
					aggregate.ShadowGPUSeconds += sample.Value
					gpuObserved[key] = gpuObserved[key] || sample.Value > 0
				}
			}
		}
		// GPU accounting is fail closed. If serving activity exists but the
		// canonical allocation counter is absent from both scopes, retaining the
		// previous hourly row is safer than silently replacing it with zero GPU
		// cost. A revision can change from platform-funded shadow preparation to
		// tenant canary allocation within this hour; either scope is evidence of
		// allocation, while their costs remain separately attributed.
		for key, active := range customerActivity {
			if active && !gpuObserved[key] {
				return nil, fmt.Errorf(
					"canonical inferscale_gpu_seconds_total series is missing for active %s/%s/%s in hour %s",
					key.tenant, key.deployment, key.revision, hour.Format(time.RFC3339),
				)
			}
		}
		for key, aggregate := range values {
			lookupKey := key.tenant + "\x00" + key.deployment
			deploymentID, ok := resolved[lookupKey]
			if !ok {
				var err error
				deploymentID, err = s.Deployments.ResolveUsageDeployment(ctx, key.tenant, key.deployment)
				if err != nil {
					return nil, fmt.Errorf("resolve deployment %s/%s: %w", key.tenant, key.deployment, err)
				}
				resolved[lookupKey] = deploymentID
			}
			aggregate.DeploymentID = deploymentID
			result = append(result, *aggregate)
		}
	}
	return result, nil
}

func hourlyQueries() []usageQuery {
	const labels = `{tenant!="",deployment!=""}`
	const window = `[3600s]`
	grouped := func(metric, selector string) string {
		return "sum by (tenant,deployment,revision) (increase(" + metric + selector + window + "))"
	}
	return []usageQuery{
		{kind: requestMetric, query: grouped("inferscale_requests_total", `{tenant!="",deployment!="",outcome="total"}`)},
		{kind: rejectionMetric, query: grouped("inferscale_request_rejections_total", labels)},
		{kind: inputTokenMetric, query: grouped("inferscale_tokens_total", `{tenant!="",deployment!="",direction="input"}`)},
		{kind: outputTokenMetric, query: grouped("inferscale_tokens_total", `{tenant!="",deployment!="",direction="output"}`)},
		{kind: gpuMetric, query: grouped("inferscale_gpu_seconds_total", `{tenant!="",deployment!="",billing_scope!="platform"}`)},
		{kind: shadowGPUMetric, query: grouped("inferscale_gpu_seconds_total", `{tenant!="",deployment!="",billing_scope="platform"}`)},
	}
}

func sampleKey(sample platformmetrics.Sample) (usageKey, error) {
	key := usageKey{
		tenant:     strings.TrimSpace(sample.Metric["tenant"]),
		deployment: strings.TrimSpace(sample.Metric["deployment"]),
		revision:   strings.TrimSpace(sample.Metric["revision"]),
	}
	if key.tenant == "" || key.deployment == "" {
		return usageKey{}, fmt.Errorf("tenant and deployment labels are required")
	}
	if key.revision == "" {
		key.revision = "unattributed"
	}
	return key, nil
}

func metricName(kind metricKind) string {
	switch kind {
	case requestMetric:
		return "requests"
	case rejectionMetric:
		return "rejections"
	case inputTokenMetric:
		return "input tokens"
	case outputTokenMetric:
		return "output tokens"
	case gpuMetric:
		return "tenant GPU seconds"
	case shadowGPUMetric:
		return "platform GPU seconds"
	default:
		return "unknown usage metric"
	}
}
