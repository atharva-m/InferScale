package postgres

import (
	"errors"
	"testing"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/deployment"
)

func TestPostgresMutationRequestIdentitySurvivesServiceRestart(t *testing.T) {
	t.Parallel()
	store := integrationStore(t)
	repository := NewDeploymentRepository(store)
	service := deployment.NewService(repository)
	input := deployment.CreateInput{
		TenantID: "tenant-1", Namespace: "tenant-test", Name: "qwen-chat", IdempotencyKey: "durable-create",
		Spec: platformv1alpha1.InferenceDeploymentSpec{
			Model:       platformv1alpha1.ModelSpec{URI: "hf://Qwen/Qwen3-8B", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			Runtime:     platformv1alpha1.RuntimeSpec{Backend: platformv1alpha1.RuntimeBackendVLLM, Precision: "bf16", TensorParallelism: 1, MaxModelLen: 8192},
			Accelerator: platformv1alpha1.AcceleratorSpec{Vendor: "nvidia", Type: "RTX_5090", Count: 1},
			Scaling:     platformv1alpha1.ScalingSpec{MinReplicas: 0, MaxReplicas: 1, Policy: "saturation"},
			Admission:   platformv1alpha1.AdmissionSpec{MaxConcurrentRequests: 1, MaxQueuedRequests: 1, PriorityClass: "standard"},
			Routing:     platformv1alpha1.RoutingSpec{Policy: platformv1alpha1.RoutingPolicyLoadAware},
			Rollout:     platformv1alpha1.RolloutSpec{Strategy: "progressive", ShadowPercent: 10},
		},
	}
	created, err := service.Create(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	updateInput := deployment.UpdateInput{TenantID: input.TenantID, DeploymentID: created.Deployment.ID,
		ExpectedGeneration: 1, Spec: input.Spec, IdempotencyKey: "durable-update"}
	updateInput.Spec.Admission.MaxConcurrentRequests = 2
	updated, err := service.Update(t.Context(), updateInput)
	if err != nil {
		t.Fatal(err)
	}
	deleteInput := deployment.DeleteInput{TenantID: input.TenantID, DeploymentID: created.Deployment.ID,
		ExpectedGeneration: 2, IdempotencyKey: "durable-delete"}
	deleted, err := service.Delete(t.Context(), deleteInput)
	if err != nil {
		t.Fatal(err)
	}

	// A fresh service has no response cache. All three operations must replay
	// against PostgreSQL, even though the deployment is now a tombstone.
	restarted := deployment.NewService(NewDeploymentRepository(store))
	createReplay, err := restarted.Create(t.Context(), input)
	if err != nil || createReplay.Operation.ID != created.Operation.ID || createReplay.Deployment.DeletedAt == nil {
		t.Fatalf("durable create replay lost original operation/tombstone: %#v %v", createReplay, err)
	}
	updateReplay, err := restarted.Update(t.Context(), updateInput)
	if err != nil || updateReplay.Operation.ID != updated.Operation.ID {
		t.Fatalf("durable update replay lost original operation: %#v %v", updateReplay, err)
	}
	deleteReplay, err := restarted.Delete(t.Context(), deleteInput)
	if err != nil || deleteReplay.ID != deleted.ID {
		t.Fatalf("durable delete replay lost original operation: %#v %v", deleteReplay, err)
	}
	for _, operation := range []*deployment.Operation{created.Operation, updated.Operation, deleted} {
		stored, err := repository.GetOperation(t.Context(), input.TenantID, operation.ID)
		if err != nil || stored.RequestDigest == "" || stored.RequestDigest != operation.RequestDigest {
			t.Fatalf("operation fingerprint was not persisted: %#v %v", stored, err)
		}
	}
	changed := input
	changed.Spec.Admission.MaxConcurrentRequests = 2
	if _, err := restarted.Create(t.Context(), changed); !errors.Is(err, deployment.ErrIdempotencyConflict) {
		t.Fatalf("create replay accepted a different desired policy after cache loss: %v", err)
	}
	updateInput.ForceReselect = true
	if _, err := restarted.Update(t.Context(), updateInput); !errors.Is(err, deployment.ErrIdempotencyConflict) {
		t.Fatalf("update replay accepted a different reselection instruction: %v", err)
	}
	deleteInput.ExpectedGeneration = 0
	if _, err := restarted.Delete(t.Context(), deleteInput); !errors.Is(err, deployment.ErrIdempotencyConflict) {
		t.Fatalf("delete replay accepted a different generation precondition: %v", err)
	}
	var operations, events int
	if err := store.pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM operations), (SELECT count(*) FROM sync_outbox)").Scan(&operations, &events); err != nil {
		t.Fatal(err)
	}
	if operations != 3 || events != 3 {
		t.Fatalf("replays added duplicate durable work: operations=%d events=%d", operations, events)
	}
	if _, err := store.pool.Exec(t.Context(), "UPDATE operations SET request_digest='malformed' WHERE id=$1", created.Operation.ID); err == nil {
		t.Fatal("malformed request fingerprint bypassed database constraint")
	}
}

func TestPostgresOperationIdentityMigrationPreservesLegacyRows(t *testing.T) {
	t.Parallel()
	store := integrationStore(t)
	repository := NewDeploymentRepository(store)
	value, revision, operation := integrationDeployment("legacy-operation")
	operation.IdempotencyKey = "legacy-key"
	if err := repository.Create(t.Context(), value, revision, operation); err != nil {
		t.Fatal(err)
	}
	// Reproduce schema 000005 inside this test's private schema, preserving an
	// existing operation, then run the real upgrade rather than a SQL mock.
	if _, err := store.pool.Exec(t.Context(), "ALTER TABLE operations DROP COLUMN request_digest"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(t.Context(), "DELETE FROM schema_migrations WHERE version='000006_operation_request_identity.up.sql'"); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(t.Context()); err != nil {
		t.Fatal(err)
	}
	stored, err := repository.GetOperation(t.Context(), value.TenantID, operation.ID)
	if err != nil || stored.RequestDigest != "" || stored.ID != operation.ID {
		t.Fatalf("upgrade did not preserve historical operation with empty fingerprint: %#v %v", stored, err)
	}
	_, err = deployment.NewService(repository).Create(t.Context(), deployment.CreateInput{
		TenantID: value.TenantID, Namespace: value.Namespace, Name: value.Name, Spec: value.Spec, IdempotencyKey: operation.IdempotencyKey,
	})
	if !errors.Is(err, deployment.ErrIdempotencyConflict) {
		t.Fatalf("historical request identity was guessed from mutable deployment: %v", err)
	}
}
