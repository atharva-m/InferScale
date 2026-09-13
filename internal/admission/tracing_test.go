package admission

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc/codes"
)

func TestCheckContinuesW3CTraceWithoutRecordingRequestBody(t *testing.T) {
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

	request := checkRequest("Bearer secret-api-key")
	request.Attributes.Request.Http.Headers["traceparent"] = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	request.Attributes.Request.Http.RawBody = []byte(`{"model":"chat","messages":[{"role":"user","content":"secret prompt"}]}`)
	server := NewServer(fakeAuthenticator{}, fakeAuthorizer{active: true}, fakeLimiter{allowed: true})
	response, err := server.Check(context.Background(), request)
	if err != nil || codes.Code(response.GetStatus().GetCode()) != codes.OK {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	traceparent := ""
	for _, header := range response.GetOkResponse().GetHeaders() {
		if strings.EqualFold(header.GetHeader().GetKey(), "traceparent") {
			traceparent = header.GetHeader().GetValue()
		}
	}
	if traceparent == "" {
		t.Fatal("admission did not propagate the accepted trace context downstream")
	}
	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].Parent().TraceID().String() != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Fatalf("ended spans did not retain incoming trace identity: %#v", spans)
	}
	for _, item := range spans[0].Attributes() {
		if strings.Contains(item.Value.Emit(), "secret") {
			t.Fatalf("credential or prompt leaked into span attribute %s", item.Key)
		}
	}
}

func TestCheckReplacesUnsafeRequestIDBeforeResponseAndTrace(t *testing.T) {
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

	request := checkRequest("Bearer secret-api-key")
	request.Attributes.Request.Http.Headers["x-request-id"] = "secret value/with separators"
	request.Attributes.Request.Http.RawBody = []byte(`{"model":"chat","messages":[{"role":"user","content":"hello"}]}`)
	response, err := NewServer(fakeAuthenticator{}, fakeAuthorizer{active: true}, fakeLimiter{allowed: true}).Check(context.Background(), request)
	if err != nil || codes.Code(response.GetStatus().GetCode()) != codes.OK {
		t.Fatalf("response=%#v err=%v", response, err)
	}
	propagatedRequestID := ""
	for _, header := range response.GetOkResponse().GetHeaders() {
		if strings.EqualFold(header.GetHeader().GetKey(), "x-request-id") {
			propagatedRequestID = header.GetHeader().GetValue()
		}
	}
	if propagatedRequestID == "" || strings.Contains(propagatedRequestID, "secret value") {
		t.Fatalf("unsafe request ID was not replaced downstream: %q", propagatedRequestID)
	}
	for _, item := range recorder.Ended()[0].Attributes() {
		if strings.Contains(item.Value.Emit(), "secret value") {
			t.Fatalf("unsafe request ID leaked into span attribute %s", item.Key)
		}
	}
}

func TestBoundedTraceMethodRejectsClientCardinality(t *testing.T) {
	if got := boundedTraceMethod(http.MethodPost); got != http.MethodPost {
		t.Fatalf("POST trace method=%q", got)
	}
	if got := boundedTraceMethod("SECRET-METHOD"); got != "OTHER" {
		t.Fatalf("unknown trace method=%q, want OTHER", got)
	}
}
