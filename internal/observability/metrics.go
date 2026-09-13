package observability

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// APIMetrics contains only collectors emitted by the management API process.
// Every label is selected from a closed set; public IDs, tenant IDs, request
// IDs, and error strings are deliberately excluded.
type APIMetrics struct {
	Requests         *prometheus.CounterVec
	RequestSeconds   *prometheus.HistogramVec
	OutboxPending    prometheus.Gauge
	UsageRuns        *prometheus.CounterVec
	UsageRunSeconds  prometheus.Histogram
	UsageLastSuccess prometheus.Gauge
}

func NewAPIMetrics(registerer prometheus.Registerer) *APIMetrics {
	metrics := &APIMetrics{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inferscale_api_http_requests_total",
			Help: "Management API requests by route, method, and status class.",
		}, []string{"route", "method", "status_class"}),
		RequestSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "inferscale_api_http_request_duration_seconds",
			Help:    "Management API request duration by route and method.",
			Buckets: prometheus.DefBuckets,
		}, []string{"route", "method"}),
		OutboxPending: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "inferscale_outbox_pending",
			Help: "Desired-state outbox events that have not been processed.",
		}),
		UsageRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inferscale_usage_aggregation_runs_total",
			Help: "Hourly usage aggregation runs by outcome.",
		}, []string{"outcome"}),
		UsageRunSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "inferscale_usage_aggregation_duration_seconds",
			Help:    "Duration of hourly usage aggregation runs.",
			Buckets: prometheus.DefBuckets,
		}),
		UsageLastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "inferscale_usage_aggregation_last_success_timestamp_seconds",
			Help: "Unix timestamp of the most recent successful usage aggregation.",
		}),
	}
	registerer.MustRegister(
		metrics.Requests,
		metrics.RequestSeconds,
		metrics.OutboxPending,
		metrics.UsageRuns,
		metrics.UsageRunSeconds,
		metrics.UsageLastSuccess,
	)
	return metrics
}

// InstrumentHTTP records management API traffic without using the raw URL as
// a label. This wrapper intentionally recognizes only the public contract and
// collapses everything else into "unmatched".
func (m *APIMetrics) InstrumentHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		started := time.Now()
		response := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(response, request)
		route := apiRoute(request.URL.Path)
		method := boundedMethod(request.Method)
		m.Requests.WithLabelValues(route, method, statusClass(response.status)).Inc()
		m.RequestSeconds.WithLabelValues(route, method).Observe(time.Since(started).Seconds())
	})
}

func (m *APIMetrics) ObserveUsageAggregation(err error, duration time.Duration, at time.Time) {
	outcome := "success"
	if err != nil {
		outcome = "error"
	}
	m.UsageRuns.WithLabelValues(outcome).Inc()
	m.UsageRunSeconds.Observe(duration.Seconds())
	if err == nil {
		m.UsageLastSuccess.Set(float64(at.Unix()))
	}
}

// AdmissionMetrics contains only collectors emitted by the Envoy ext-auth
// service. Reasons are normalized through a closed allowlist before use.
type AdmissionMetrics struct {
	Requests       *prometheus.CounterVec
	RequestSeconds *prometheus.HistogramVec
	Throttles      *prometheus.CounterVec
}

func NewAdmissionMetrics(registerer prometheus.Registerer) *AdmissionMetrics {
	metrics := &AdmissionMetrics{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inferscale_admission_requests_total",
			Help: "Inference authorization checks by decision and bounded reason.",
		}, []string{"decision", "reason"}),
		RequestSeconds: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "inferscale_admission_request_duration_seconds",
			Help:    "Inference authorization check duration by decision.",
			Buckets: prometheus.DefBuckets,
		}, []string{"decision"}),
		Throttles: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inferscale_tenant_throttles_total",
			Help: "Tenant-wide admission throttles by bounded reason.",
		}, []string{"reason"}),
	}
	registerer.MustRegister(metrics.Requests, metrics.RequestSeconds, metrics.Throttles)
	return metrics
}

// ObserveAdmission implements admission.CheckObserver.
func (m *AdmissionMetrics) ObserveAdmission(decision, reason string, duration time.Duration) {
	decision = boundedDecision(decision)
	reason = boundedAdmissionReason(reason)
	m.Requests.WithLabelValues(decision, reason).Inc()
	m.RequestSeconds.WithLabelValues(decision).Observe(duration.Seconds())
	if reason == "rate_limit_exceeded" {
		m.Throttles.WithLabelValues(reason).Inc()
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusRecorder) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecorder) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *statusRecorder) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func apiRoute(path string) string {
	path = strings.TrimSuffix(path, "/")
	switch path {
	case "/healthz":
		return "/healthz"
	case "/readyz":
		return "/readyz"
	case "/v1/deployments":
		return "/v1/deployments"
	}
	segments := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(segments) == 3 && segments[0] == "v1" && segments[1] == "deployments" {
		return "/v1/deployments/{id}"
	}
	if len(segments) == 4 && segments[0] == "v1" && segments[1] == "deployments" {
		switch segments[3] {
		case "benchmarks":
			return "/v1/deployments/{id}/benchmarks"
		case "metrics":
			return "/v1/deployments/{id}/metrics"
		}
	}
	if len(segments) == 3 && segments[0] == "v1" && segments[1] == "operations" {
		return "/v1/operations/{id}"
	}
	if len(segments) == 5 && segments[0] == "internal" && segments[1] == "v1" && segments[2] == "benchmarks" && segments[4] == "result" {
		return "/internal/v1/benchmarks/{id}/result"
	}
	return "unmatched"
}

func boundedMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPatch, http.MethodDelete:
		return method
	default:
		return "OTHER"
	}
}

func statusClass(status int) string {
	if status < 100 || status > 599 {
		return "unknown"
	}
	return strconv.Itoa(status/100) + "xx"
}

func boundedDecision(value string) string {
	switch value {
	case "allowed", "denied", "error":
		return value
	default:
		return "error"
	}
}

func boundedAdmissionReason(value string) string {
	switch value {
	case "allowed", "bypass", "invalid_request", "invalid_api_key",
		"authentication_unavailable", "insufficient_scope", "deployment_not_found",
		"authorization_unavailable", "benchmark_target_changed", "rate_limit_unavailable", "rate_limit_exceeded",
		"grpc_error":
		return value
	default:
		return "other"
	}
}
