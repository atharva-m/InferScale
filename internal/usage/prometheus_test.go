package usage

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	platformmetrics "github.com/inferscale/inferscale/internal/metrics"
)

type prometheusStub struct {
	omitGPU           bool
	platformOnly      bool
	secondDeployment  bool
	omitSecondGPUOnly bool
}

func (s prometheusStub) Query(_ context.Context, query string, at time.Time) ([]platformmetrics.Sample, error) {
	metric := map[string]string{"tenant": "tenant-1", "deployment": "chat", "revision": "rev-2"}
	value := 0.0
	switch {
	case strings.Contains(query, "inferscale_requests_total"):
		value = 10
	case strings.Contains(query, "inferscale_request_rejections_total"):
		value = 2
	case strings.Contains(query, `direction="input"`):
		value = 100
	case strings.Contains(query, `direction="output"`):
		value = 50
	case strings.Contains(query, `billing_scope="platform"`):
		if s.omitGPU {
			return nil, nil
		}
		value = 60
	case strings.Contains(query, "inferscale_gpu_seconds_total"):
		if s.omitGPU || s.platformOnly {
			return nil, nil
		}
		value = 3600
	default:
		return nil, fmt.Errorf("unexpected query %q", query)
	}
	samples := []platformmetrics.Sample{{Metric: metric, Timestamp: at, Value: value}}
	if s.secondDeployment {
		second := map[string]string{"tenant": "tenant-2", "deployment": "other", "revision": "rev-9"}
		if !(s.omitSecondGPUOnly && strings.Contains(query, "inferscale_gpu_seconds_total")) {
			samples = append(samples, platformmetrics.Sample{Metric: second, Timestamp: at, Value: value})
		}
	}
	return samples, nil
}

func TestCanaryScopeTransitionDoesNotAbortHourlyUsage(t *testing.T) {
	source := PrometheusSource{Prometheus: prometheusStub{platformOnly: true}, Deployments: resolverStub{}}
	start := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	values, err := source.CompletedHour(context.Background(), start, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].GPUSeconds != 0 || values[0].ShadowGPUSeconds != 60 || values[0].Requests != 10 {
		t.Fatalf("scope-separated usage=%#v", values)
	}
}

type resolverStub struct{}

func (resolverStub) ResolveUsageDeployment(_ context.Context, tenant, name string) (string, error) {
	if tenant == "tenant-2" && name == "other" {
		return "0193f4ed-7a4c-7b26-b903-2a96e31fe478", nil
	}
	if tenant != "tenant-1" || name != "chat" {
		return "", fmt.Errorf("unexpected identity")
	}
	return "0193f4ed-7a4c-7b26-b903-2a96e31fe477", nil
}

func TestPrometheusSourceBuildsCompleteHourlyUsage(t *testing.T) {
	source := PrometheusSource{Prometheus: prometheusStub{}, Deployments: resolverStub{}}
	start := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	values, err := source.CompletedHour(context.Background(), start, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 {
		t.Fatalf("got %d rows, want one", len(values))
	}
	got := values[0]
	if got.Requests != 10 || got.RejectedRequests != 2 || got.InputTokens != 100 || got.OutputTokens != 50 || got.GPUSeconds != 3600 || got.ShadowGPUSeconds != 60 {
		t.Fatalf("unexpected aggregate: %#v", got)
	}
}

func TestHourlyRequestQueryCountsTotalExactlyOnce(t *testing.T) {
	t.Parallel()
	queries := hourlyQueries()
	if got := queries[0].query; !strings.Contains(got, `outcome="total"`) {
		t.Fatalf("request accounting query = %q, want outcome=total", got)
	}
}

func TestPrometheusSourceFailsClosedWhenGPUCounterMissing(t *testing.T) {
	source := PrometheusSource{Prometheus: prometheusStub{omitGPU: true}, Deployments: resolverStub{}}
	start := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	if _, err := source.CompletedHour(context.Background(), start, start.Add(time.Hour)); err == nil || !strings.Contains(err.Error(), "gpu_seconds") {
		t.Fatalf("expected missing GPU counter error, got %v", err)
	}
}

func TestPrometheusSourceRequiresGPUCounterForEveryActiveDeployment(t *testing.T) {
	source := PrometheusSource{
		Prometheus:  prometheusStub{secondDeployment: true, omitSecondGPUOnly: true},
		Deployments: resolverStub{},
	}
	start := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	_, err := source.CompletedHour(context.Background(), start, start.Add(time.Hour))
	if err == nil || !strings.Contains(err.Error(), "tenant-2/other/rev-9") {
		t.Fatalf("expected deployment-scoped missing GPU counter error, got %v", err)
	}
}
