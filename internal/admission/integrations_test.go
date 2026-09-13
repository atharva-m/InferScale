package admission

import (
	"context"
	"errors"
	"testing"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/benchmark"
	"github.com/inferscale/inferscale/internal/deployment"
	"github.com/inferscale/inferscale/internal/tenant"
)

type runLookup struct {
	run benchmark.Run
	err error
}

func (r runLookup) GetRun(context.Context, string) (benchmark.Run, error) {
	return r.run, r.err
}

func TestRunScopedAuthenticatorAcceptsOnlyActiveRun(t *testing.T) {
	tokens, err := benchmark.NewCallbackTokens([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := tokens.Issue("run-1")
	if err != nil {
		t.Fatal(err)
	}
	authenticator := RunScopedAuthenticator{
		Tokens: tokens,
		Runs: runLookup{run: benchmark.Run{
			ID: "run-1", TenantID: "tenant-1", DeploymentID: "deployment-1", RevisionID: "revision-1", State: benchmark.RunRunning,
			Execution: benchmark.ExecutionContract{RoutingPolicy: "load-aware"},
		}},
	}
	principal, err := authenticator.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	if principal.TenantID != "tenant-1" || principal.AuthorizedDeployment != "deployment-1" || principal.AuthorizedRevision != "revision-1" || principal.AuthorizedRoutingPolicy != "load-aware" || !principal.HasScope("inference") {
		t.Fatalf("unexpected principal: %#v", principal)
	}
}

func TestRunScopedAuthenticatorRejectsTerminalOrUnknownRun(t *testing.T) {
	tokens, _ := benchmark.NewCallbackTokens([]byte("0123456789abcdef0123456789abcdef"))
	token, _ := tokens.Issue("run-1")
	for name, lookup := range map[string]runLookup{
		"terminal":         {run: benchmark.Run{ID: "run-1", TenantID: "tenant-1", DeploymentID: "deployment-1", RevisionID: "revision-1", State: benchmark.RunSucceeded, Execution: benchmark.ExecutionContract{RoutingPolicy: "load-aware"}}},
		"missing revision": {run: benchmark.Run{ID: "run-1", TenantID: "tenant-1", DeploymentID: "deployment-1", State: benchmark.RunRunning}},
		"missing routing":  {run: benchmark.Run{ID: "run-1", TenantID: "tenant-1", DeploymentID: "deployment-1", RevisionID: "revision-1", State: benchmark.RunRunning}},
		"missing":          {err: benchmark.ErrRunNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			authenticator := RunScopedAuthenticator{Tokens: tokens, Runs: lookup}
			if _, err := authenticator.Authenticate(context.Background(), token); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestPolicyRepositoryExposesDatabaseRevisionIdentity(t *testing.T) {
	t.Parallel()
	repository := PolicyRepository{
		Deployments: deploymentLookupStub{value: &deployment.Deployment{
			ID: "deployment-1", TenantID: "tenant-1", StableRevisionID: "stable-id", CandidateRevisionID: "candidate-id",
			Spec: platformv1alpha1.InferenceDeploymentSpec{
				Admission: platformv1alpha1.AdmissionSpec{PriorityClass: "standard"},
				Routing:   platformv1alpha1.RoutingSpec{Policy: platformv1alpha1.RoutingPolicyLoadAware},
			},
			ObservedStatus: platformv1alpha1.InferenceDeploymentStatus{
				Phase:    platformv1alpha1.DeploymentPhaseUpdating,
				Revision: platformv1alpha1.RevisionStatus{Stable: "stable-name", Candidate: "candidate-name"},
			},
		}},
		Tenants: tenantLookupStub{value: &tenant.Tenant{ID: "tenant-1", Quota: tenant.Quota{RequestsPerMinute: 60}}},
	}
	policy, err := repository.AuthorizeDeployment(context.Background(), "tenant-1", "deployment-1")
	if err != nil {
		t.Fatal(err)
	}
	if policy.StableRevisionID != "stable-id" || policy.CandidateRevisionID != "candidate-id" ||
		policy.RoutingPolicy != "load-aware" || !policy.InferenceActive {
		t.Fatalf("revision policy = %#v", policy)
	}
}

type deploymentLookupStub struct{ value *deployment.Deployment }

func (s deploymentLookupStub) Get(context.Context, string, string) (*deployment.Deployment, error) {
	return s.value, nil
}

type tenantLookupStub struct{ value *tenant.Tenant }

func (s tenantLookupStub) Get(context.Context, string) (*tenant.Tenant, error) {
	return s.value, nil
}

func TestPolicyRepositoryRejectsSuspendedTenantForEveryCredentialType(t *testing.T) {
	suspendedAt := time.Now().UTC()
	repository := PolicyRepository{
		Deployments: deploymentLookupStub{value: &deployment.Deployment{
			ID: "deployment-1", TenantID: "tenant-1",
			ObservedStatus: platformv1alpha1.InferenceDeploymentStatus{
				Phase:    platformv1alpha1.DeploymentPhaseReady,
				Revision: platformv1alpha1.RevisionStatus{Stable: "revision-1"},
			},
		}},
		Tenants: tenantLookupStub{value: &tenant.Tenant{
			ID: "tenant-1", SuspendedAt: &suspendedAt,
		}},
	}
	if _, err := repository.AuthorizeDeployment(context.Background(), "tenant-1", "deployment-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want fail-closed not found", err)
	}
}
