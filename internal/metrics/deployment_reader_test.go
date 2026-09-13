package metrics

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inferscale/inferscale/internal/deployment"
)

type deploymentLookupStub struct {
	value *deployment.Deployment
}

func (s deploymentLookupStub) Get(_ context.Context, tenantID, deploymentID string) (*deployment.Deployment, error) {
	if tenantID != s.value.TenantID || deploymentID != s.value.ID {
		return nil, deployment.ErrNotFound
	}
	return s.value, nil
}

func TestDeploymentReaderUsesCanonicalMetricContract(t *testing.T) {
	queries := make([]string, 0, 7)
	var mu sync.Mutex
	httpClient := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		mu.Lock()
		queries = append(queries, request.URL.Query().Get("query"))
		mu.Unlock()
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"status":"success","data":{"resultType":"matrix","result":[]}}`)),
		}, nil
	})}
	client, err := NewPrometheusClient("http://prometheus.test", httpClient)
	if err != nil {
		t.Fatal(err)
	}
	reader := DeploymentReader{
		Prometheus: client,
		Deployments: deploymentLookupStub{value: &deployment.Deployment{
			ID: "018f-id", TenantID: "tenant-id", Name: "chat", Namespace: "tenant-acme",
		}},
	}
	end := time.Now().UTC()
	if _, err := reader.ReadDeploymentMetrics(context.Background(), "tenant-id", "018f-id", end.Add(-time.Hour), end); err != nil {
		t.Fatal(err)
	}
	if len(queries) != 7 {
		t.Fatalf("got %d queries, want 7", len(queries))
	}
	joined := strings.Join(queries, "\n")
	for _, required := range []string{
		"inferscale_requests_total",
		`outcome="total"`,
		"inferscale_ttft_seconds_bucket",
		"inferscale_tpot_seconds_bucket",
		"inferscale_router_flow_control_wait_seconds_bucket",
		"inferscale_tokens_total",
		`namespace="tenant-acme"`,
		`deployment="chat"`,
	} {
		if !strings.Contains(joined, required) {
			t.Errorf("queries do not contain %q:\n%s", required, joined)
		}
	}
	for _, forbidden := range []string{"018f-id", "_milliseconds_bucket", "inferscale_inference_requests_total"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("queries unexpectedly contain %q:\n%s", forbidden, joined)
		}
	}
}

func TestDeploymentReaderBoundsParallelQueries(t *testing.T) {
	entered, release := make(chan struct{}, 7), make(chan struct{})
	var active, maximum atomic.Int32
	httpClient := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for previous := maximum.Load(); current > previous; previous = maximum.Load() {
			if maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		entered <- struct{}{}
		select {
		case <-release:
		case <-request.Context().Done():
			return nil, request.Context().Err()
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"status":"success","data":{"result":[]}}`))}, nil
	})}
	prometheus, err := NewPrometheusClient("http://prometheus.test", httpClient)
	if err != nil {
		t.Fatal(err)
	}
	reader := DeploymentReader{Prometheus: prometheus, Deployments: deploymentLookupStub{value: &deployment.Deployment{ID: "deployment", TenantID: "tenant"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		now := time.Now()
		_, err := reader.ReadDeploymentMetrics(ctx, "tenant", "deployment", now.Add(-time.Hour), now)
		result <- err
	}()
	for range 4 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("queries were not parallel")
		}
	}
	close(release)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 4 {
		t.Fatalf("concurrent queries=%d, want 4", maximum.Load())
	}
}
