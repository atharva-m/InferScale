package metrics

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/inferscale/inferscale/internal/deployment"
	"golang.org/x/sync/errgroup"
)

type deploymentLookup interface {
	Get(context.Context, string, string) (*deployment.Deployment, error)
}

// DeploymentReader implements the management API's bounded metrics view. The
// PromQL templates are owned by InferScale; callers can choose only a time
// window and can never submit arbitrary PromQL.
type DeploymentReader struct {
	Prometheus  *Client
	Deployments deploymentLookup
}

type DeploymentMetrics struct {
	DeploymentID string              `json:"deployment_id"`
	Start        time.Time           `json:"start"`
	End          time.Time           `json:"end"`
	StepSeconds  int64               `json:"step_seconds"`
	Series       map[string][]Sample `json:"series"`
}

func (r DeploymentReader) ReadDeploymentMetrics(
	ctx context.Context,
	tenantID, deploymentID string,
	start, end time.Time,
) (any, error) {
	if r.Prometheus == nil || r.Deployments == nil {
		return nil, fmt.Errorf("deployment metrics reader is not configured")
	}
	value, err := r.Deployments.Get(ctx, tenantID, deploymentID)
	if err != nil {
		return nil, err
	}
	window := end.Sub(start)
	if window < 5*time.Minute || window > 24*time.Hour {
		return nil, fmt.Errorf("%w: metrics window must be between 5 minutes and 24 hours", deployment.ErrInvalid)
	}
	step := 15 * time.Second
	if window > 6*time.Hour {
		step = time.Minute
	}
	// Runtime and router normalizers use the Kubernetes namespace and stable
	// tenant-visible deployment name. The public UUID intentionally does not
	// need to be exposed to third-party runtime metrics.
	selector := "{namespace=" + strconv.Quote(value.Namespace) + ",deployment=" + strconv.Quote(value.Name) + "}"
	totalRequestRate := "sum(rate(inferscale_requests_total" + withMatch(selector, "outcome", "total") + "[5m]))"
	errorRequestRate := "sum(rate(inferscale_requests_total" + withMatch(selector, "outcome", "error") + "[5m]))"
	queries := map[string]string{
		"request_rate": totalRequestRate,
		"error_rate":   errorRequestRate + "/clamp_min(" + totalRequestRate + ",0.001)",
		"ttft_p95_ms":  "1000 * histogram_quantile(0.95, sum by (le) (rate(inferscale_ttft_seconds_bucket" + selector + "[5m])))",
		"tpot_p95_ms":  "1000 * histogram_quantile(0.95, sum by (le) (rate(inferscale_tpot_seconds_bucket" + selector + "[5m])))",
		"queue_p95_ms": "1000 * histogram_quantile(0.95, sum by (le) (rate(inferscale_router_flow_control_wait_seconds_bucket" + selector + "[5m])))",
		"input_token_rate": "sum(rate(inferscale_tokens_total" +
			withMatch(selector, "direction", "input") + "[5m]))",
		"output_token_rate": "sum(rate(inferscale_tokens_total" +
			withMatch(selector, "direction", "output") + "[5m]))",
	}
	series := make(map[string][]Sample, len(queries))
	var seriesMu sync.Mutex
	group, queryCtx := errgroup.WithContext(ctx)
	// Keep the fixed query set responsive without allowing one metrics request
	// to consume an unbounded share of Prometheus capacity.
	group.SetLimit(4)
	for name, query := range queries {
		group.Go(func() error {
			if err := queryCtx.Err(); err != nil {
				return err
			}
			samples, err := r.Prometheus.QueryRange(queryCtx, query, start, end, step)
			if err != nil {
				return fmt.Errorf("query %s: %w", name, err)
			}
			seriesMu.Lock()
			series[name] = samples
			seriesMu.Unlock()
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, err
	}
	return DeploymentMetrics{
		DeploymentID: deploymentID,
		Start:        start.UTC(),
		End:          end.UTC(),
		StepSeconds:  int64(step.Seconds()),
		Series:       series,
	}, nil
}

func withMatch(selector, label, value string) string {
	return selector[:len(selector)-1] + "," + label + "=" + strconv.Quote(value) + "}"
}
