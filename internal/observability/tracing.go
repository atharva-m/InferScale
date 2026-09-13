package observability

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type Shutdown func(context.Context) error

func InitTracing(ctx context.Context, serviceName, endpoint string) (Shutdown, error) {
	if strings.TrimSpace(endpoint) == "" {
		return func(context.Context) error { return nil }, nil
	}
	res, err := tracingResource(serviceName)
	if err != nil {
		return nil, err
	}
	exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(endpoint))
	if err != nil {
		return nil, err
	}
	provider := tracesdk.NewTracerProvider(tracesdk.WithBatcher(exporter), tracesdk.WithResource(res))
	otel.SetTracerProvider(provider)
	// Trace identity is the only client-supplied telemetry context admitted by
	// v1. Arbitrary W3C baggage is intentionally not propagated because it can
	// contain user-controlled values that do not belong in traces.
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return func(shutdownCtx context.Context) error {
		return errors.Join(provider.Shutdown(shutdownCtx), exporter.Shutdown(shutdownCtx))
	}, nil
}

func tracingResource(serviceName string) (*resource.Resource, error) {
	// service.name is stable across semantic-convention versions. Keep this
	// override schemaless so the SDK's default resource retains its own schema
	// and attributes when the SDK version changes.
	return resource.Merge(resource.Default(), resource.NewSchemaless(attribute.String("service.name", serviceName)))
}

// TraceHTTP creates one body-free server span for each management request.
// Only protocol metadata is attached: request and response bodies, bearer
// credentials, tenant identity, and error text never become span attributes.
// The global W3C propagator installed by InitTracing keeps a caller-provided
// trace connected to this service and writes the current context back to the
// response for operator correlation.
func TraceHTTP(serviceName string, next http.Handler) http.Handler {
	tracer := otel.Tracer(serviceName + "/http")
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(request.Context(), propagation.HeaderCarrier(request.Header))
		method := boundedMethod(request.Method)
		ctx, span := tracer.Start(ctx, "HTTP "+method, trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(writer.Header()))

		response := &traceResponseWriter{ResponseWriter: writer, status: http.StatusOK}
		next.ServeHTTP(response, request.WithContext(ctx))
		attributes := []attribute.KeyValue{
			attribute.String("http.request.method", method),
			attribute.String("http.route", boundedHTTPRoute(request.URL.Path)),
			attribute.Int("http.response.status_code", response.status),
		}
		if requestID := response.Header().Get("X-Request-ID"); requestID != "" {
			attributes = append(attributes, attribute.String("inferscale.request.id", requestID))
		}
		span.SetAttributes(attributes...)
		if response.status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, "server error")
		}
	})
}

type traceResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *traceResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *traceResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *traceResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func boundedHTTPRoute(path string) string {
	path = strings.TrimSuffix(path, "/")
	if path == "/healthz" || path == "/readyz" || path == "/v1/deployments" {
		return path
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if len(parts) == 3 && parts[0] == "v1" && parts[1] == "deployments" {
		return "/v1/deployments/{id}"
	}
	if len(parts) == 4 && parts[0] == "v1" && parts[1] == "deployments" &&
		(parts[3] == "benchmarks" || parts[3] == "metrics") {
		return "/v1/deployments/{id}/" + parts[3]
	}
	if len(parts) == 3 && parts[0] == "v1" && parts[1] == "operations" {
		return "/v1/operations/{id}"
	}
	if len(parts) == 5 && parts[0] == "internal" && parts[1] == "v1" && parts[2] == "benchmarks" && parts[4] == "result" {
		return "/internal/v1/benchmarks/{id}/result"
	}
	return "unmatched"
}
