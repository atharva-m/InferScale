package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
)

type benchmarkDeploymentStore struct {
	value *Deployment
	gets  int
}

func (s *benchmarkDeploymentStore) Create(context.Context, *Deployment, *Revision, *Operation) error {
	return nil
}

func (s *benchmarkDeploymentStore) Get(_ context.Context, tenantID, deploymentID string) (*Deployment, error) {
	s.gets++
	if s.value == nil || s.value.TenantID != tenantID || s.value.ID != deploymentID {
		return nil, ErrNotFound
	}
	copy := *s.value
	return &copy, nil
}

func (s *benchmarkDeploymentStore) GetByID(ctx context.Context, deploymentID string) (*Deployment, error) {
	if s.value == nil {
		return nil, ErrNotFound
	}
	return s.Get(ctx, s.value.TenantID, deploymentID)
}

func (*benchmarkDeploymentStore) List(context.Context, string, ListOptions) (*ListPage, error) {
	return &ListPage{}, nil
}

func (*benchmarkDeploymentStore) Update(context.Context, *Deployment, *Revision, *Operation, int64) error {
	return nil
}

func (*benchmarkDeploymentStore) SoftDelete(context.Context, string, string, int64, time.Time, *Operation) error {
	return nil
}

func (*benchmarkDeploymentStore) GetOperation(context.Context, string, string) (*Operation, error) {
	return nil, ErrNotFound
}

func (*benchmarkDeploymentStore) FindOperationByIdempotency(context.Context, string, string) (*Operation, error) {
	return nil, ErrNotFound
}

func (*benchmarkDeploymentStore) UpdateObservedStatus(context.Context, string, platformv1alpha1.InferenceDeploymentStatus, time.Time) error {
	return nil
}

type benchmarkRunStore struct {
	runs    map[string]*BenchmarkRun
	creates int
}

func (s *benchmarkRunStore) CreateBenchmark(_ context.Context, run *BenchmarkRun) error {
	if s.runs == nil {
		s.runs = make(map[string]*BenchmarkRun)
	}
	copy := *run
	copy.Configuration = append(json.RawMessage(nil), run.Configuration...)
	s.runs[run.TenantID+":"+run.IdempotencyKey] = &copy
	s.creates++
	return nil
}

func (s *benchmarkRunStore) FindBenchmarkByIdempotency(_ context.Context, tenantID, key string) (*BenchmarkRun, error) {
	run := s.runs[tenantID+":"+key]
	if run == nil {
		return nil, ErrBenchmarkNotFound
	}
	copy := *run
	copy.Configuration = append(json.RawMessage(nil), run.Configuration...)
	return &copy, nil
}

func (*benchmarkRunStore) ListBenchmarks(context.Context, string, string, ListOptions) (*BenchmarkPage, error) {
	return &BenchmarkPage{}, nil
}

func readyBenchmarkDeployment(t *testing.T) *Deployment {
	t.Helper()
	value, revision, err := New(CreateInput{
		TenantID: "tenant-1", Namespace: "tenant-acme", Name: "qwen-chat", Spec: testSpec(),
	}, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	value.State = StateReady
	value.StableRevisionID = revision.ID
	value.CandidateRevisionID = ""
	value.ObservedStatus = platformv1alpha1.InferenceDeploymentStatus{
		Phase:              platformv1alpha1.DeploymentPhaseReady,
		ObservedGeneration: value.Generation,
		Revision: platformv1alpha1.RevisionStatus{
			Stable: "qwen-chat-a1b2c3d4", StableID: revision.ID,
		},
	}
	return value
}

func TestBenchmarkCreateTargetsOnlyFullyReadyStableRevision(t *testing.T) {
	t.Parallel()
	value := readyBenchmarkDeployment(t)
	deployments := &benchmarkDeploymentStore{value: value}
	runs := &benchmarkRunStore{}
	service := NewBenchmarkService(deployments, runs)

	run, err := service.Create(context.Background(), value.TenantID, value.ID, "selection", json.RawMessage(`{"seed":7}`), 1.5, "run-on-stable")
	if err != nil {
		t.Fatal(err)
	}
	if run.RevisionID != value.StableRevisionID {
		t.Fatalf("benchmark revision = %q, want stable %q", run.RevisionID, value.StableRevisionID)
	}
	if runs.creates != 1 {
		t.Fatalf("created runs = %d, want 1", runs.creates)
	}
}

func TestBenchmarkCreateRejectsNonStableTargetStates(t *testing.T) {
	t.Parallel()
	base := readyBenchmarkDeployment(t)
	now := time.Unix(200, 0)
	tests := map[string]func(*Deployment){
		"pending state":             func(value *Deployment) { value.State = StatePending },
		"missing stable":            func(value *Deployment) { value.StableRevisionID = "" },
		"database candidate":        func(value *Deployment) { value.CandidateRevisionID = "candidate-id" },
		"controller updating":       func(value *Deployment) { value.ObservedStatus.Phase = platformv1alpha1.DeploymentPhaseUpdating },
		"generation not reconciled": func(value *Deployment) { value.ObservedStatus.ObservedGeneration-- },
		"status candidate":          func(value *Deployment) { value.ObservedStatus.Revision.Candidate = "candidate" },
		"stable identity mismatch":  func(value *Deployment) { value.ObservedStatus.Revision.StableID = "other-id" },
		"deleting":                  func(value *Deployment) { value.DeletedAt = &now },
	}
	for name, mutate := range tests {
		name, mutate := name, mutate
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			value := *base
			mutate(&value)
			runs := &benchmarkRunStore{}
			service := NewBenchmarkService(&benchmarkDeploymentStore{value: &value}, runs)
			_, err := service.Create(context.Background(), value.TenantID, value.ID, "selection", json.RawMessage(`{"seed":7}`), 1.5, "guarded")
			if !errors.Is(err, ErrBenchmarkTargetNotReady) {
				t.Fatalf("error = %v, want ErrBenchmarkTargetNotReady", err)
			}
			if runs.creates != 0 {
				t.Fatalf("created runs = %d, want 0", runs.creates)
			}
		})
	}
}

func TestBenchmarkCreateReplaysBeforeRevalidatingMutableDeployment(t *testing.T) {
	t.Parallel()
	value := readyBenchmarkDeployment(t)
	deployments := &benchmarkDeploymentStore{value: value}
	runs := &benchmarkRunStore{}
	service := NewBenchmarkService(deployments, runs)
	configuration := json.RawMessage(`{"batch":2,"seed":7}`)

	first, err := service.Create(context.Background(), value.TenantID, value.ID, "selection", configuration, 1.5, "same-request")
	if err != nil {
		t.Fatal(err)
	}
	value.State = StateUpdating
	value.CandidateRevisionID = "candidate-id"
	value.ObservedStatus.Phase = platformv1alpha1.DeploymentPhaseUpdating
	getsBeforeReplay := deployments.gets
	replay, err := service.Create(context.Background(), value.TenantID, value.ID, "selection", json.RawMessage(`{"seed":7,"batch":2}`), 1.5, "same-request")
	if err != nil {
		t.Fatalf("idempotent replay after update began: %v", err)
	}
	if replay.ID != first.ID || deployments.gets != getsBeforeReplay || runs.creates != 1 {
		t.Fatalf("replay consulted mutable target or created another run: replay=%q first=%q gets=%d/%d creates=%d", replay.ID, first.ID, deployments.gets, getsBeforeReplay, runs.creates)
	}
	if _, err := service.Create(context.Background(), value.TenantID, value.ID, "selection", json.RawMessage(`{"batch":3,"seed":7}`), 1.5, "same-request"); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different replay error = %v, want ErrIdempotencyConflict", err)
	}
}

func TestBenchmarkCreatePreservesCrossTenantNotFound(t *testing.T) {
	t.Parallel()
	value := readyBenchmarkDeployment(t)
	service := NewBenchmarkService(&benchmarkDeploymentStore{value: value}, &benchmarkRunStore{})
	_, err := service.Create(context.Background(), "another-tenant", value.ID, "selection", json.RawMessage(`{"seed":7}`), 1.5, "cross-tenant")
	if !errors.Is(err, ErrNotFound) || errors.Is(err, ErrBenchmarkTargetNotReady) {
		t.Fatalf("cross-tenant error = %v, want ErrNotFound", err)
	}
}
