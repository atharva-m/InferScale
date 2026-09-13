package observability

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestTracingResourcePreservesSDKSchemaAndAttributes(t *testing.T) {
	res, err := tracingResource("test-api")
	if err != nil {
		t.Fatalf("initialize tracing resource: %v", err)
	}
	defaults := resource.Default()
	if res.SchemaURL() != defaults.SchemaURL() {
		t.Fatalf("resource schema = %q, want SDK schema %q", res.SchemaURL(), defaults.SchemaURL())
	}
	attributes := res.Set()
	serviceName, ok := attributes.Value("service.name")
	if !ok || serviceName.AsString() != "test-api" {
		t.Fatalf("service.name = %v, want test-api", serviceName)
	}
	for _, expected := range defaults.Attributes() {
		if expected.Key == "service.name" {
			continue
		}
		actual, ok := attributes.Value(expected.Key)
		if !ok || !reflect.DeepEqual(actual.AsInterface(), expected.Value.AsInterface()) {
			t.Fatalf("default attribute %q = %v, want %v", expected.Key, actual, expected.Value)
		}
	}
}

func TestInitTracingWithConfiguredExporter(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})

	// The gRPC client remains idle until a span is exported. Exercise the
	// configured startup path without a collector or any network requests.
	shutdown, err := InitTracing(t.Context(), "test-api", "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("initialize configured tracing: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdown(ctx); err != nil {
			t.Errorf("shutdown tracing: %v", err)
		}
	})
	if _, ok := otel.GetTracerProvider().(*sdktrace.TracerProvider); !ok {
		t.Fatalf("configured tracing did not install an SDK provider")
	}
	if fields := otel.GetTextMapPropagator().Fields(); !reflect.DeepEqual(fields, []string{"traceparent", "tracestate"}) {
		t.Fatalf("propagator fields = %v, want trace identity only", fields)
	}
}

func TestTraceHTTPPropagatesIdentityWithoutSensitiveAttributes(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	previousPropagator := otel.GetTextMapPropagator()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithSpanProcessor(recorder),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		_ = provider.Shutdown(t.Context())
		otel.SetTracerProvider(previousProvider)
		otel.SetTextMapPropagator(previousPropagator)
	})

	handler := TraceHTTP("test-api", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Request-ID", "req-safe")
		writer.WriteHeader(http.StatusAccepted)
	}))
	request := httptest.NewRequest("SECRET-METHOD", "/v1/deployments/019f6d72-6d22-7b31-a2ac-6a90739016e5/benchmarks", strings.NewReader("secret prompt"))
	request.Header.Set("Authorization", "Bearer secret-api-key")
	request.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusAccepted || response.Header().Get("traceparent") == "" {
		t.Fatalf("status=%d traceparent=%q", response.Code, response.Header().Get("traceparent"))
	}
	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].Parent().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("ended spans did not retain incoming trace identity: %#v", spans)
	}
	if spans[0].Name() != "HTTP OTHER" {
		t.Fatalf("unknown client method produced unbounded span name %q", spans[0].Name())
	}
	for _, item := range spans[0].Attributes() {
		value := item.Value.Emit()
		if strings.Contains(value, "secret") || strings.Contains(value, "019f6d72") {
			t.Fatalf("sensitive or high-cardinality request value leaked into span attribute %s=%q", item.Key, value)
		}
	}
	if value := spanAttribute(spans[0].Attributes(), "inferscale.request.id"); value != "req-safe" {
		t.Fatalf("request ID attribute=%q", value)
	}
}

func spanAttribute(values []attribute.KeyValue, key attribute.Key) string {
	for _, value := range values {
		if value.Key == key {
			return value.Value.AsString()
		}
	}
	return ""
}
