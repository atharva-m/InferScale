package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestAPIMetricsUseBoundedRouteAndStatusLabels(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewAPIMetrics(registry)
	handler := metrics.InstrumentHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPatch, "/v1/deployments/0193f4ed-7a4c-7b26-b903-2a96e31fe477?secret=no", nil))

	want := `
# HELP inferscale_api_http_requests_total Management API requests by route, method, and status class.
# TYPE inferscale_api_http_requests_total counter
inferscale_api_http_requests_total{method="PATCH",route="/v1/deployments/{id}",status_class="2xx"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), "inferscale_api_http_requests_total"); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionMetricsNormalizeUnknownReason(t *testing.T) {
	registry := prometheus.NewRegistry()
	metrics := NewAdmissionMetrics(registry)
	metrics.ObserveAdmission("denied", "database said tenant-super-secret", time.Millisecond)

	want := `
# HELP inferscale_admission_requests_total Inference authorization checks by decision and bounded reason.
# TYPE inferscale_admission_requests_total counter
inferscale_admission_requests_total{decision="denied",reason="other"} 1
`
	if err := testutil.GatherAndCompare(registry, strings.NewReader(want), "inferscale_admission_requests_total"); err != nil {
		t.Fatal(err)
	}
}
