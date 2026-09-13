package admission

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/genproto/googleapis/rpc/status"
	grpccodes "google.golang.org/grpc/codes"
)

type Server struct {
	authv3.UnimplementedAuthorizationServer
	authenticator Authenticator
	authorizer    DeploymentAuthorizer
	limiter       RateLimiter
	observer      CheckObserver
}

type CheckObserver interface {
	ObserveAdmission(decision, reason string, duration time.Duration)
}

func NewServer(authenticator Authenticator, authorizer DeploymentAuthorizer, limiter RateLimiter, observers ...CheckObserver) *Server {
	server := &Server{authenticator: authenticator, authorizer: authorizer, limiter: limiter}
	if len(observers) > 0 {
		server.observer = observers[0]
	}
	return server
}

func (s *Server) Check(ctx context.Context, req *authv3.CheckRequest) (response *authv3.CheckResponse, checkErr error) {
	started := time.Now()
	requestPath := ""
	var admissionSpan trace.Span
	defer func() {
		decision, reason := "error", "grpc_error"
		if checkErr == nil && response != nil && response.GetStatus() != nil {
			reason = response.GetStatus().GetMessage()
			if response.GetStatus().GetCode() == int32(grpccodes.OK) {
				decision, reason = "allowed", "allowed"
				if !looksLikeInferencePath(requestPath) {
					reason = "bypass"
				}
			} else {
				decision = "denied"
			}
		}
		if s.observer != nil {
			s.observer.ObserveAdmission(decision, reason, time.Since(started))
		}
		if admissionSpan != nil {
			admissionSpan.SetAttributes(
				attribute.String("inferscale.admission.decision", decision),
				attribute.String("inferscale.admission.reason", boundedTraceReason(reason)),
			)
			if checkErr != nil {
				admissionSpan.RecordError(checkErr)
				admissionSpan.SetStatus(codes.Error, "gRPC authorization check failed")
			} else if decision == "denied" {
				admissionSpan.SetStatus(codes.Error, boundedTraceReason(reason))
			} else {
				admissionSpan.SetStatus(codes.Ok, "")
			}
			admissionSpan.End()
		}
	}()
	httpRequest := req.GetAttributes().GetRequest().GetHttp()
	if httpRequest == nil {
		return denied(http.StatusBadRequest, "invalid_request", "HTTP request attributes are required", "", 0), nil
	}
	requestPath = httpRequest.GetPath()
	requestID := safeRequestID(firstNonEmpty(httpRequest.GetHeaders()["x-request-id"], httpRequest.GetId()))
	// The Gateway-level SecurityPolicy also covers the management API and
	// health routes. Those retain their own authentication middleware; this
	// service only authorizes the exact inference path. Scheduling headers are
	// still stripped from every other route before it reaches a backend.
	if !looksLikeInferencePath(httpRequest.GetPath()) {
		return allowed(nil, StripUntrustedScheduling()), nil
	}
	ctx, admissionSpan = startAdmissionSpan(ctx, httpRequest.GetHeaders(), httpRequest.GetMethod(), requestID)
	bearer, err := ExtractBearer(httpRequest.GetHeaders()["authorization"])
	if err != nil {
		return denied(http.StatusUnauthorized, "invalid_api_key", "a valid bearer API key is required", requestID, 0), nil
	}
	principal, err := s.authenticator.Authenticate(ctx, bearer)
	if err != nil {
		statusCode := http.StatusUnauthorized
		code := "invalid_api_key"
		if errors.Is(err, ErrUnavailable) {
			statusCode, code = http.StatusServiceUnavailable, "authentication_unavailable"
		}
		return denied(statusCode, code, http.StatusText(statusCode), requestID, 0), nil
	}
	if !principal.HasScope("inference") {
		return denied(http.StatusForbidden, "insufficient_scope", "the API key cannot invoke inference", requestID, 0), nil
	}
	deploymentID, err := ParseDeploymentPath(httpRequest.GetPath())
	if err != nil {
		return denied(http.StatusNotFound, "deployment_not_found", "deployment not found", requestID, 0), nil
	}
	if principal.AuthorizedDeployment != "" && principal.AuthorizedDeployment != deploymentID {
		return denied(http.StatusNotFound, "deployment_not_found", "deployment not found", requestID, 0), nil
	}
	policy, err := s.authorizer.AuthorizeDeployment(ctx, principal.TenantID, deploymentID)
	if err != nil || policy.TenantID != principal.TenantID || !policy.InferenceActive {
		if errors.Is(err, ErrUnavailable) {
			return denied(http.StatusServiceUnavailable, "authorization_unavailable", "authorization is temporarily unavailable", requestID, 0), nil
		}
		return denied(http.StatusNotFound, "deployment_not_found", "deployment not found", requestID, 0), nil
	}
	admissionSpan.SetAttributes(
		attribute.String("inferscale.tenant.id", principal.TenantID),
		attribute.String("inferscale.deployment.id", deploymentID),
	)
	if principal.AuthorizedRevision != "" &&
		(policy.StableRevisionID != principal.AuthorizedRevision || policy.CandidateRevisionID != "" ||
			policy.RoutingPolicy != principal.AuthorizedRoutingPolicy) {
		return denied(http.StatusConflict, "benchmark_target_changed", "the benchmark target contract has changed", requestID, 0), nil
	}
	body := httpRequest.GetRawBody()
	if len(body) == 0 && httpRequest.GetBody() != "" {
		body = []byte(httpRequest.GetBody())
	}
	if err := ValidateInferenceRequest(body, policy.Name); err != nil {
		return denied(http.StatusBadRequest, "invalid_request", err.Error(), requestID, 0), nil
	}
	decision, err := s.limiter.Allow(ctx, principal.TenantID, policy.RatePerMinute)
	if err != nil {
		return denied(http.StatusServiceUnavailable, "rate_limit_unavailable", "rate limiting is temporarily unavailable", requestID, 0), nil
	}
	if !decision.Allowed {
		return denied(http.StatusTooManyRequests, "rate_limit_exceeded", "tenant request rate exceeded", requestID, retryAfterSeconds(decision.RetryAfter)), nil
	}

	headers := append(InjectTrusted(principal, policy), requestIDHeader(requestID))
	headers = append(headers, traceContextHeaders(ctx)...)
	return allowed(headers, StripUntrusted()), nil
}

func requestIDHeader(requestID string) *corev3.HeaderValueOption {
	return &corev3.HeaderValueOption{
		Header:       &corev3.HeaderValue{Key: "x-request-id", Value: requestID},
		AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
	}
}

func startAdmissionSpan(ctx context.Context, headers map[string]string, method, requestID string) (context.Context, trace.Span) {
	carrier := propagation.MapCarrier{}
	for key, value := range headers {
		carrier[strings.ToLower(key)] = value
	}
	ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	ctx, span := otel.Tracer("inferscale-admission/ext-auth").Start(
		ctx, "inference admission", trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(
			attribute.String("http.request.method", boundedTraceMethod(method)),
			attribute.String("http.route", "/v1/deployments/{deployment_id}/chat/completions"),
			attribute.String("inferscale.request.id", requestID),
		),
	)
	return ctx, span
}

func traceContextHeaders(ctx context.Context) []*corev3.HeaderValueOption {
	carrier := propagation.MapCarrier{}
	// Propagate only W3C trace identity. InitTracing intentionally excludes
	// arbitrary baggage so client-controlled values cannot cross the admission
	// trust boundary as opaque telemetry metadata.
	propagation.TraceContext{}.Inject(ctx, carrier)
	result := make([]*corev3.HeaderValueOption, 0, 2)
	for _, key := range []string{"traceparent", "tracestate"} {
		if value := carrier[key]; value != "" {
			result = append(result, &corev3.HeaderValueOption{
				Header:       &corev3.HeaderValue{Key: key, Value: value},
				AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			})
		}
	}
	return result
}

func boundedTraceReason(value string) string {
	switch value {
	case "allowed", "invalid_request", "invalid_api_key", "authentication_unavailable",
		"insufficient_scope", "deployment_not_found", "authorization_unavailable",
		"benchmark_target_changed", "rate_limit_unavailable", "rate_limit_exceeded", "grpc_error":
		return value
	default:
		return "other"
	}
}

func boundedTraceMethod(value string) string {
	if value == http.MethodPost {
		return value
	}
	return "OTHER"
}

func retryAfterSeconds(duration time.Duration) int {
	if duration <= 0 {
		return 0
	}
	return int((duration + time.Second - 1) / time.Second)
}

func allowed(headers []*corev3.HeaderValueOption, headersToRemove []string) *authv3.CheckResponse {
	return &authv3.CheckResponse{
		Status: &status.Status{Code: int32(grpccodes.OK)},
		HttpResponse: &authv3.CheckResponse_OkResponse{OkResponse: &authv3.OkHttpResponse{
			Headers:         headers,
			HeadersToRemove: headersToRemove,
		}},
	}
}

func looksLikeInferencePath(path string) bool {
	path = strings.SplitN(path, "?", 2)[0]
	return strings.HasPrefix(path, "/v1/deployments/") && strings.HasSuffix(path, "/chat/completions")
}

func denied(httpStatus int, code, message, requestID string, retryAfter int) *authv3.CheckResponse {
	body := fmt.Sprintf(`{"type":"about:blank","title":%q,"status":%d,"code":%q,"request_id":%q}`, message, httpStatus, code, requestID)
	headers := []*corev3.HeaderValueOption{{
		Header:       &corev3.HeaderValue{Key: "content-type", Value: "application/problem+json"},
		AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
	}}
	if requestID != "" {
		headers = append(headers, &corev3.HeaderValueOption{Header: &corev3.HeaderValue{Key: "x-request-id", Value: requestID}, AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD})
	}
	if retryAfter > 0 {
		headers = append(headers, &corev3.HeaderValueOption{Header: &corev3.HeaderValue{Key: "retry-after", Value: strconv.Itoa(retryAfter)}, AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD})
	}
	grpcCode := grpccodes.PermissionDenied
	if httpStatus >= 500 {
		grpcCode = grpccodes.Unavailable
	} else if httpStatus == http.StatusUnauthorized {
		grpcCode = grpccodes.Unauthenticated
	} else if httpStatus == http.StatusTooManyRequests {
		grpcCode = grpccodes.ResourceExhausted
	}
	return &authv3.CheckResponse{
		Status: &status.Status{Code: int32(grpcCode), Message: code},
		HttpResponse: &authv3.CheckResponse_DeniedResponse{DeniedResponse: &authv3.DeniedHttpResponse{
			Status:  &typev3.HttpStatus{Code: typev3.StatusCode(httpStatus)},
			Headers: headers,
			Body:    body,
		}},
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func safeRequestID(value string) string {
	value = strings.TrimSpace(value)
	if value != "" && len(value) <= 128 {
		valid := true
		for _, character := range value {
			if (character >= 'a' && character <= 'z') ||
				(character >= 'A' && character <= 'Z') ||
				(character >= '0' && character <= '9') ||
				character == '-' || character == '_' || character == '.' || character == ':' {
				continue
			}
			valid = false
			break
		}
		if valid {
			return value
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}
