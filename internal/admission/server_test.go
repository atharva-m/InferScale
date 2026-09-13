package admission

import (
	"context"
	"errors"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	authv3 "github.com/envoyproxy/go-control-plane/envoy/service/auth/v3"
	"google.golang.org/grpc/codes"
)

type fakeAuthenticator struct {
	err        error
	deployment string
	revision   string
	routing    string
}

func (f fakeAuthenticator) Authenticate(context.Context, string) (Principal, error) {
	return Principal{
		TenantID: "tenant-1", AuthorizedDeployment: f.deployment, AuthorizedRevision: f.revision,
		AuthorizedRoutingPolicy: f.routing,
		Scopes:                  map[string]struct{}{"inference": {}},
	}, f.err
}

type fakeAuthorizer struct {
	active    bool
	stable    string
	candidate string
	routing   string
}

func (f fakeAuthorizer) AuthorizeDeployment(context.Context, string, string) (DeploymentPolicy, error) {
	return DeploymentPolicy{
		ID: "dep", Name: "chat", TenantID: "tenant-1", PriorityClass: "standard",
		RatePerMinute: 60, InferenceActive: f.active,
		StableRevisionID: f.stable, CandidateRevisionID: f.candidate,
		RoutingPolicy: f.routing,
	}, nil
}

type fakeLimiter struct {
	allowed    bool
	retryAfter time.Duration
	err        error
}

type checkObserver struct {
	decision string
	reason   string
}

func (o *checkObserver) ObserveAdmission(decision, reason string, _ time.Duration) {
	o.decision, o.reason = decision, reason
}

const testDeploymentID = "018f6d72-6d22-7b31-a2ac-6a90739016e5"

func (f fakeLimiter) Allow(context.Context, string, int64) (RateLimitDecision, error) {
	retryAfter := f.retryAfter
	if retryAfter == 0 {
		retryAfter = time.Second
	}
	return RateLimitDecision{Allowed: f.allowed, RetryAfter: retryAfter}, f.err
}

func TestCheckRateLimitRetryAfterAndFailureAreFailClosed(t *testing.T) {
	t.Parallel()
	server := NewServer(fakeAuthenticator{}, fakeAuthorizer{active: true}, fakeLimiter{retryAfter: time.Millisecond})
	response, err := server.Check(context.Background(), checkRequest("Bearer secret"))
	if err != nil {
		t.Fatal(err)
	}
	if response.GetDeniedResponse().GetStatus().GetCode() != 429 {
		t.Fatalf("denied HTTP status = %v", response.GetDeniedResponse().GetStatus().GetCode())
	}
	foundRetry := false
	for _, header := range response.GetDeniedResponse().GetHeaders() {
		if header.GetHeader().GetKey() == "retry-after" && header.GetHeader().GetValue() == "1" {
			foundRetry = true
		}
	}
	if !foundRetry {
		t.Fatal("sub-second token refill was not rounded up to Retry-After: 1")
	}

	server = NewServer(fakeAuthenticator{}, fakeAuthorizer{active: true}, fakeLimiter{err: errors.New("Valkey unavailable")})
	response, err = server.Check(context.Background(), checkRequest("Bearer secret"))
	if err != nil {
		t.Fatal(err)
	}
	if response.GetDeniedResponse().GetStatus().GetCode() != 503 {
		t.Fatalf("rate limiter failure did not fail closed: %#v", response)
	}
}

func TestRetryAfterSecondsRoundsUp(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		duration time.Duration
		want     int
	}{
		{duration: 0, want: 0},
		{duration: time.Millisecond, want: 1},
		{duration: time.Second, want: 1},
		{duration: time.Second + time.Millisecond, want: 2},
	} {
		if got := retryAfterSeconds(test.duration); got != test.want {
			t.Fatalf("retryAfterSeconds(%s) = %d, want %d", test.duration, got, test.want)
		}
	}
}

func checkRequest(auth string) *authv3.CheckRequest {
	return checkRequestPath("/v1/deployments/"+testDeploymentID+"/chat/completions", auth)
}

func checkRequestPath(path, auth string) *authv3.CheckRequest {
	return &authv3.CheckRequest{Attributes: &authv3.AttributeContext{Request: &authv3.AttributeContext_Request{Http: &authv3.AttributeContext_HttpRequest{
		Path:    path,
		Headers: map[string]string{"authorization": auth, "x-request-id": "req-1"},
		RawBody: []byte(`{"model":"chat","messages":[{"role":"user","content":"hello"}]}`),
	}}}}
}

func TestCheckAllowsOwnedDeployment(t *testing.T) {
	observer := &checkObserver{}
	server := NewServer(fakeAuthenticator{}, fakeAuthorizer{active: true}, fakeLimiter{allowed: true}, observer)
	response, err := server.Check(context.Background(), checkRequest("Bearer secret"))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if got := codes.Code(response.GetStatus().GetCode()); got != codes.OK {
		t.Fatalf("status = %v", got)
	}
	if response.GetOkResponse() == nil || len(response.GetOkResponse().GetHeaders()) == 0 {
		t.Fatal("expected trusted headers")
	}
	if observer.decision != "allowed" || observer.reason != "allowed" {
		t.Fatalf("observation = %s/%s", observer.decision, observer.reason)
	}
}

func TestCheckConstrainRunTokenToOneDeployment(t *testing.T) {
	server := NewServer(fakeAuthenticator{deployment: "019f6d72-6d22-7b31-a2ac-6a90739016e5"}, fakeAuthorizer{active: true}, fakeLimiter{allowed: true})
	response, err := server.Check(context.Background(), checkRequest("Bearer secret"))
	if err != nil {
		t.Fatal(err)
	}
	if response.GetDeniedResponse().GetStatus().GetCode() != 404 {
		t.Fatalf("cross-deployment run token status = %v", response.GetDeniedResponse().GetStatus().GetCode())
	}
}

func TestCheckConstrainRunTokenToSoleStableRevision(t *testing.T) {
	t.Parallel()
	tests := map[string]fakeAuthorizer{
		"stable changed":    {active: true, stable: "replacement-revision"},
		"candidate started": {active: true, stable: "stable-revision", candidate: "candidate-revision"},
	}
	for name, authorizer := range tests {
		name, authorizer := name, authorizer
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			authorizer.routing = "load-aware"
			server := NewServer(fakeAuthenticator{deployment: testDeploymentID, revision: "stable-revision", routing: "load-aware"}, authorizer, fakeLimiter{allowed: true})
			response, err := server.Check(context.Background(), checkRequest("Bearer benchmark-token"))
			if err != nil {
				t.Fatal(err)
			}
			if response.GetDeniedResponse().GetStatus().GetCode() != 409 || response.GetStatus().GetMessage() != "benchmark_target_changed" {
				t.Fatalf("changed benchmark target response = %#v", response)
			}
		})
	}

	server := NewServer(
		fakeAuthenticator{deployment: testDeploymentID, revision: "stable-revision", routing: "load-aware"},
		fakeAuthorizer{active: true, stable: "stable-revision", routing: "load-aware"},
		fakeLimiter{allowed: true},
	)
	response, err := server.Check(context.Background(), checkRequest("Bearer benchmark-token"))
	if err != nil || codes.Code(response.GetStatus().GetCode()) != codes.OK {
		t.Fatalf("unchanged stable benchmark target was denied: response=%#v err=%v", response, err)
	}
}

func TestCheckRejectsRunTokenAfterInPlaceRoutingChange(t *testing.T) {
	t.Parallel()
	server := NewServer(
		fakeAuthenticator{deployment: testDeploymentID, revision: "stable-revision", routing: "load-aware"},
		fakeAuthorizer{active: true, stable: "stable-revision", routing: "prefix-aware"},
		fakeLimiter{allowed: true},
	)
	response, err := server.Check(context.Background(), checkRequest("Bearer benchmark-token"))
	if err != nil {
		t.Fatal(err)
	}
	if response.GetDeniedResponse().GetStatus().GetCode() != 409 || response.GetStatus().GetMessage() != "benchmark_target_changed" {
		t.Fatalf("routing-changed benchmark target response = %#v", response)
	}
}

func TestCheckOrdinaryTenantKeyRemainsValidDuringRollout(t *testing.T) {
	t.Parallel()
	server := NewServer(
		fakeAuthenticator{},
		fakeAuthorizer{active: true, stable: "stable-revision", candidate: "candidate-revision"},
		fakeLimiter{allowed: true},
	)
	response, err := server.Check(context.Background(), checkRequest("Bearer ordinary-key"))
	if err != nil || codes.Code(response.GetStatus().GetCode()) != codes.OK {
		t.Fatalf("ordinary tenant key was constrained by benchmark guard: response=%#v err=%v", response, err)
	}
}

func TestCheckFailsClosedWhenAuthenticationUnavailable(t *testing.T) {
	server := NewServer(fakeAuthenticator{err: errors.New("database down")}, fakeAuthorizer{active: true}, fakeLimiter{allowed: true})
	response, err := server.Check(context.Background(), checkRequest("Bearer secret"))
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}
	if response.GetDeniedResponse().GetStatus().GetCode() != 401 {
		t.Fatalf("HTTP status = %v", response.GetDeniedResponse().GetStatus().GetCode())
	}
}

func TestParseDeploymentPath(t *testing.T) {
	if id, err := ParseDeploymentPath("/v1/deployments/" + testDeploymentID + "/chat/completions?x=1"); err != nil || id != testDeploymentID {
		t.Fatalf("ParseDeploymentPath() = %q, %v", id, err)
	}
	if _, err := ParseDeploymentPath("/v1/deployments/not-a-uuid/chat/completions"); err == nil {
		t.Fatal("expected non-UUIDv7 deployment path to be rejected")
	}
	if _, err := ParseDeploymentPath("/v1/chat/completions"); err == nil {
		t.Fatal("expected invalid path")
	}
}

func TestCheckPassesNonInferenceRoutesToManagementAPI(t *testing.T) {
	server := NewServer(nil, nil, nil)
	response, err := server.Check(context.Background(), checkRequestPath("/v1/deployments", ""))
	if err != nil {
		t.Fatal(err)
	}
	if codes.Code(response.GetStatus().GetCode()) != codes.OK {
		t.Fatalf("management route unexpectedly denied: %#v", response)
	}
	if len(response.GetOkResponse().GetHeadersToRemove()) == 0 {
		t.Fatal("untrusted scheduling headers were not stripped")
	}
	for _, header := range response.GetOkResponse().GetHeadersToRemove() {
		if header == "authorization" {
			t.Fatal("management bearer credential was stripped before API authentication")
		}
	}
}

func TestCheckRejectsRuntimeExtensions(t *testing.T) {
	server := NewServer(fakeAuthenticator{}, fakeAuthorizer{active: true}, fakeLimiter{allowed: true})
	request := checkRequest("Bearer secret")
	request.Attributes.Request.Http.RawBody = []byte(`{"model":"chat","messages":[{"role":"user","content":"hello"}],"tools":[]}`)
	response, err := server.Check(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.GetDeniedResponse().GetStatus().GetCode() != 400 {
		t.Fatalf("unsupported extension was not rejected: %#v", response)
	}
}

var _ = corev3.HeaderValue{}
