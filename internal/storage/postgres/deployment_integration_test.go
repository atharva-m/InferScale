package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/deployment"
	"github.com/inferscale/inferscale/internal/tenant"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Each test owns a schema, so this suite can safely share the CI migration
// database and run concurrently without clearing another test's fixtures.
func integrationStore(t *testing.T) *Store {
	t.Helper()
	databaseURL := os.Getenv("INFERSCALE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("set INFERSCALE_TEST_DATABASE_URL to run PostgreSQL transaction tests")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedSchema := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quotedSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+quotedSchema+" CASCADE"); err != nil {
			t.Errorf("drop test schema: %v", err)
		}
	})
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	store := New(pool)
	t.Cleanup(store.Close)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := NewTenantRepository(store).Create(ctx, &tenant.Tenant{
		ID: "tenant-1", Slug: "test", Name: "Test", Namespace: "tenant-test",
		Quota:     tenant.Quota{MaxDeployments: 10, MaxGPUs: 16, MaxConcurrentRequests: 100, MaxQueuedRequests: 100},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

func integrationDeployment(id string) (*deployment.Deployment, *deployment.Revision, *deployment.Operation) {
	now := time.Now().UTC()
	spec := platformv1alpha1.InferenceDeploymentSpec{
		Accelerator: platformv1alpha1.AcceleratorSpec{Count: 1},
		Scaling:     platformv1alpha1.ScalingSpec{MaxReplicas: 1},
		Admission:   platformv1alpha1.AdmissionSpec{MaxConcurrentRequests: 1, MaxQueuedRequests: 1},
	}
	revision := &deployment.Revision{
		ID: id + "-revision-1", DeploymentID: id, Number: 1, Spec: spec,
		State: deployment.RevisionCandidate, CreatedAt: now,
	}
	value := &deployment.Deployment{
		ID: id, TenantID: "tenant-1", Name: id, Namespace: "tenant-test", Generation: 1,
		Spec: spec, State: deployment.StatePending, CandidateRevisionID: revision.ID,
		CreatedAt: now, UpdatedAt: now,
	}
	return value, revision, integrationOperation(value, id+"-create", deployment.OperationCreate)
}

func integrationOperation(value *deployment.Deployment, id string, kind deployment.OperationKind) *deployment.Operation {
	now := time.Now().UTC()
	return &deployment.Operation{
		ID: id, TenantID: value.TenantID, DeploymentID: value.ID, Kind: kind,
		Status: deployment.OperationAccepted, CreatedAt: now, UpdatedAt: now,
	}
}

func TestPostgresUpdatePreservesConcurrentPromotion(t *testing.T) {
	for _, servingChange := range []bool{false, true} {
		name := "policy"
		if servingChange {
			name = "serving"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := integrationStore(t)
			repository := NewDeploymentRepository(store)
			value, revision, operation := integrationDeployment("chat")
			ctx := t.Context()
			if err := repository.Create(ctx, value, revision, operation); err != nil {
				t.Fatal(err)
			}
			stale, err := repository.Get(ctx, value.TenantID, value.ID)
			if err != nil {
				t.Fatal(err)
			}
			// Promotion happens after the service read but before Update takes
			// its row lock, and intentionally does not advance desired generation.
			status := platformv1alpha1.InferenceDeploymentStatus{
				Phase:    platformv1alpha1.DeploymentPhaseReady,
				Revision: platformv1alpha1.RevisionStatus{Stable: "chat-v1", StableID: revision.ID},
				Rollout:  platformv1alpha1.RolloutStatus{Stage: "Stable"},
			}
			if err := repository.ProjectControllerStatus(ctx, value.ID, revision.ID, status, time.Now()); err != nil {
				t.Fatal(err)
			}
			stale.Generation++
			stale.Spec.Admission.MaxConcurrentRequests = 2
			var nextRevision *deployment.Revision
			wantCandidate, wantState := "", deployment.StateReady
			if servingChange {
				copy := *revision
				copy.ID, copy.Number = "chat-revision-2", 2
				nextRevision = &copy
				stale.State = deployment.StateUpdating
				wantCandidate, wantState = copy.ID, deployment.StateUpdating
			}
			if err := repository.Update(ctx, stale, nextRevision, integrationOperation(stale, "update", deployment.OperationUpdate), 1); err != nil {
				t.Fatal(err)
			}
			got, err := repository.Get(ctx, value.TenantID, value.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.StableRevisionID != revision.ID || got.CandidateRevisionID != wantCandidate || got.State != wantState || got.Generation != 2 || got.Spec.Admission.MaxConcurrentRequests != 2 {
				t.Fatalf("update lost promoted state or policy: %#v", got)
			}
			if err := repository.Update(ctx, stale, nil, integrationOperation(stale, "stale-update", deployment.OperationUpdate), 1); !errors.Is(err, deployment.ErrGenerationConflict) {
				t.Fatalf("stale generation error = %v", err)
			}
		})
	}
}

func TestPostgresInitialRevisionRecoversAfterPrerequisiteFailure(t *testing.T) {
	t.Parallel()
	store := integrationStore(t)
	repository := NewDeploymentRepository(store)
	value, revision, operation := integrationDeployment("recovered")
	ctx := t.Context()
	if err := repository.Create(ctx, value, revision, operation); err != nil {
		t.Fatal(err)
	}
	failed := platformv1alpha1.InferenceDeploymentStatus{Phase: platformv1alpha1.DeploymentPhaseFailed}
	if err := repository.ProjectControllerStatus(ctx, value.ID, revision.ID, failed, time.Now()); err != nil {
		t.Fatal(err)
	}
	var state deployment.RevisionState
	if err := store.pool.QueryRow(ctx, `SELECT state FROM deployment_revisions WHERE id=$1`, revision.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != deployment.RevisionFailed {
		t.Fatalf("prerequisite failure state=%q", state)
	}
	ready := platformv1alpha1.InferenceDeploymentStatus{
		Phase:    platformv1alpha1.DeploymentPhaseReady,
		Revision: platformv1alpha1.RevisionStatus{Stable: "recovered-v1", StableID: revision.ID},
		Rollout:  platformv1alpha1.RolloutStatus{Stage: "Stable"},
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := repository.ProjectControllerStatus(ctx, value.ID, revision.ID, ready, time.Now()); err != nil {
			t.Fatal(err)
		}
		got, err := repository.Get(ctx, value.TenantID, value.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != deployment.StateReady || got.StableRevisionID != revision.ID || got.CandidateRevisionID != "" {
			t.Fatalf("recovery/replay lost stable identity: %#v", got)
		}
	}
	if err := store.pool.QueryRow(ctx, `SELECT state FROM deployment_revisions WHERE id=$1`, revision.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != deployment.RevisionStable {
		t.Fatalf("recovered immutable revision state=%q", state)
	}
	var promotions int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM rollout_history WHERE deployment_id=$1 AND details->>'promote'='true'`, value.ID).Scan(&promotions); err != nil {
		t.Fatal(err)
	}
	if promotions != 1 {
		t.Fatalf("replayed recovery recorded %d promotions, want 1", promotions)
	}
}

func TestPostgresQuotaRejectsOverflowingGPURequest(t *testing.T) {
	t.Parallel()
	store := integrationStore(t)
	repository := NewDeploymentRepository(store)
	value, revision, operation := integrationDeployment("too-large")
	value.Spec.Accelerator.Count = 4
	value.Spec.Scaling.MaxReplicas = 1 << 30
	if err := repository.Create(t.Context(), value, revision, operation); !errors.Is(err, deployment.ErrQuotaExceeded) {
		t.Fatalf("overflowing request error = %v, want quota rejection", err)
	}
	var deployments, operations, events int
	if err := store.pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM deployments), (SELECT count(*) FROM operations), (SELECT count(*) FROM sync_outbox)").Scan(&deployments, &operations, &events); err != nil {
		t.Fatal(err)
	}
	if deployments != 0 || operations != 0 || events != 0 {
		t.Fatalf("rejected transaction left rows: deployments=%d operations=%d events=%d", deployments, operations, events)
	}
}

func TestPostgresQuotaHandlesExistingLargeAllocation(t *testing.T) {
	t.Parallel()
	store := integrationStore(t)
	repository := NewDeploymentRepository(store)
	value, revision, operation := integrationDeployment("legacy")
	if err := repository.Create(t.Context(), value, revision, operation); err != nil {
		t.Fatal(err)
	}
	// Reproduce a row admitted by the old int32 multiplication bug.
	value.Spec.Accelerator.Count = 4
	value.Spec.Scaling.MaxReplicas = 1 << 30
	raw, err := json.Marshal(value.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(t.Context(), "UPDATE deployments SET spec=$2 WHERE id=$1", value.ID, raw); err != nil {
		t.Fatal(err)
	}
	value, revision, operation = integrationDeployment("new")
	if err := repository.Create(t.Context(), value, revision, operation); !errors.Is(err, deployment.ErrQuotaExceeded) {
		t.Fatalf("existing large allocation error = %v, want quota rejection rather than SQL overflow", err)
	}
}

func TestPostgresOutboxRetryIsVisibleAndCompletionWins(t *testing.T) {
	t.Parallel()
	store := integrationStore(t)
	repository := NewDeploymentRepository(store)
	outbox := NewOutboxRepository(store)
	value, revision, operation := integrationDeployment("chat")
	ctx := t.Context()
	if err := repository.Create(ctx, value, revision, operation); err != nil {
		t.Fatal(err)
	}
	var eventID int64
	if err := store.pool.QueryRow(ctx, "SELECT id FROM sync_outbox WHERE operation_id=$1", operation.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	internalError := "cannot apply Secret sensitive-token: upstream failure"
	if err := outbox.Retry(ctx, eventID, internalError, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := repository.GetOperation(ctx, value.TenantID, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != deployment.OperationRetrying || got.Error != "Deployment synchronization failed; retry scheduled." || got.CompletedAt != nil {
		t.Fatalf("retry is not safely visible: %#v", got)
	}
	var attempts int
	var lastError string
	if err := store.pool.QueryRow(ctx, "SELECT attempts, last_error FROM sync_outbox WHERE id=$1", eventID).Scan(&attempts, &lastError); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || lastError != internalError {
		t.Fatalf("internal retry evidence = (%d, %q)", attempts, lastError)
	}
	if err := outbox.Complete(ctx, eventID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := outbox.Retry(ctx, eventID, "late failure", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err = repository.GetOperation(ctx, value.TenantID, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != deployment.OperationSucceeded || got.Error != "" || got.CompletedAt == nil {
		t.Fatalf("late retry regressed completion: %#v", got)
	}
	if err := store.pool.QueryRow(ctx, "SELECT attempts, last_error FROM sync_outbox WHERE id=$1", eventID).Scan(&attempts, &lastError); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || lastError != "" {
		t.Fatalf("late retry changed completed outbox: (%d, %q)", attempts, lastError)
	}
}
