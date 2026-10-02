package deployment

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// This repository exercises service retries deterministically, including the
// lookup/write race in which a competing request wins a unique key first.
type idempotencyRepository struct {
	Repository
	values        map[string]*Deployment
	operations    map[string]*Operation
	writes        int
	hiddenLookups int
	writeError    error
}

func (r *idempotencyRepository) Create(_ context.Context, value *Deployment, _ *Revision, operation *Operation) error {
	return r.store(value, operation)
}

func (r *idempotencyRepository) Update(_ context.Context, value *Deployment, _ *Revision, operation *Operation, _ int64) error {
	return r.store(value, operation)
}

func (r *idempotencyRepository) SoftDelete(_ context.Context, tenantID, id string, _ int64, at time.Time, operation *Operation) error {
	value := *r.values[id]
	value.State, value.DeletedAt = StateDeleting, &at
	value.Generation++
	return r.store(&value, operation)
}

func (r *idempotencyRepository) store(value *Deployment, operation *Operation) error {
	r.writes++
	if r.writeError != nil {
		return r.writeError
	}
	copy := *value
	r.values[value.ID] = &copy
	operationCopy := *operation
	r.operations[operation.TenantID+":"+operation.IdempotencyKey] = &operationCopy
	return nil
}

func (r *idempotencyRepository) Get(ctx context.Context, tenantID, id string) (*Deployment, error) {
	value, err := r.GetByID(ctx, id)
	if err != nil || value.TenantID != tenantID || value.DeletedAt != nil {
		return nil, ErrNotFound
	}
	return value, nil
}

func (r *idempotencyRepository) GetByID(_ context.Context, id string) (*Deployment, error) {
	value := r.values[id]
	if value == nil {
		return nil, ErrNotFound
	}
	copy := *value
	return &copy, nil
}

func (r *idempotencyRepository) FindOperationByIdempotency(_ context.Context, tenantID, key string) (*Operation, error) {
	if r.hiddenLookups > 0 {
		r.hiddenLookups--
		return nil, ErrNotFound
	}
	value := r.operations[tenantID+":"+key]
	if value == nil {
		return nil, ErrNotFound
	}
	copy := *value
	return &copy, nil
}

func idempotencyFixture(t *testing.T) (*Service, *idempotencyRepository, CreateInput, *Mutation) {
	t.Helper()
	repository := &idempotencyRepository{values: map[string]*Deployment{}, operations: map[string]*Operation{}}
	service := NewService(repository)
	input := CreateInput{TenantID: "tenant-1", Namespace: "tenant-acme", Name: "qwen-chat", Spec: testSpec(), IdempotencyKey: "create", RequestID: "first"}
	created, err := service.Create(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	return service, repository, input, created
}

func TestCreateReplayUsesImmutableRequestAfterDeploymentChanges(t *testing.T) {
	service, repository, input, first := idempotencyFixture(t)
	current := repository.values[first.Deployment.ID]
	current.Spec.Scaling.MaxReplicas = 3
	current.Generation++
	at := time.Now()
	current.State, current.DeletedAt = StateDeleting, &at
	input.RequestID = "network-retry"
	replayed, err := service.Create(t.Context(), input)
	if err != nil || replayed.Operation.ID != first.Operation.ID || replayed.Deployment.ID != first.Deployment.ID || repository.writes != 1 {
		t.Fatalf("retry after later update/deletion must return original operation without another create: replay=%#v err=%v writes=%d", replayed, err, repository.writes)
	}
	for _, change := range []struct {
		name string
		edit func(*CreateInput)
	}{
		{"name", func(value *CreateInput) { value.Name = "other-chat" }},
		{"namespace", func(value *CreateInput) { value.Namespace = "tenant-other" }},
		{"policy", func(value *CreateInput) { value.Spec.Scaling.MaxReplicas = 3 }},
		{"model", func(value *CreateInput) { value.Spec.Model.Revision = strings.Repeat("b", 40) }},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := input
			change.edit(&changed)
			if _, err := service.Create(t.Context(), changed); !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("changed request must conflict even if it matches current mutable state: %v", err)
			}
		})
	}
}

func TestUpdateReplayBindsGenerationSpecAndExplicitReselection(t *testing.T) {
	service, repository, _, created := idempotencyFixture(t)
	input := UpdateInput{TenantID: created.Deployment.TenantID, DeploymentID: created.Deployment.ID,
		ExpectedGeneration: 1, Spec: testSpec(), IdempotencyKey: "update", RequestID: "first"}
	input.Spec.Scaling.MaxReplicas = 3
	first, err := service.Update(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	// Later operations must not change what the original key identifies.
	repository.values[input.DeploymentID].Generation++
	repository.values[input.DeploymentID].Spec.Scaling.MaxReplicas = 2
	input.RequestID = "network-retry"
	replayed, err := service.Update(t.Context(), input)
	if err != nil || replayed.Operation.ID != first.Operation.ID || repository.writes != 2 {
		t.Fatalf("same update retry did not reuse original operation: replay=%#v err=%v writes=%d", replayed, err, repository.writes)
	}
	for _, change := range []struct {
		name string
		edit func(*UpdateInput)
	}{
		{"target", func(value *UpdateInput) { value.DeploymentID = "other" }},
		{"generation", func(value *UpdateInput) { value.ExpectedGeneration++ }},
		{"policy", func(value *UpdateInput) { value.Spec.Scaling.MaxReplicas = 2 }},
		{"model", func(value *UpdateInput) { value.Spec.Model.Revision = strings.Repeat("b", 40) }},
		{"reselection", func(value *UpdateInput) { value.ForceReselect = true }},
	} {
		t.Run(change.name, func(t *testing.T) {
			changed := input
			change.edit(&changed)
			if _, err := service.Update(t.Context(), changed); !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("changed update accepted as a retry: %v", err)
			}
		})
	}
}

func TestDeleteReplayBindsOriginalGenerationAndTarget(t *testing.T) {
	service, repository, _, created := idempotencyFixture(t)
	input := DeleteInput{TenantID: created.Deployment.TenantID, DeploymentID: created.Deployment.ID,
		ExpectedGeneration: 1, IdempotencyKey: "delete", RequestID: "first"}
	first, err := service.Delete(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input.RequestID = "network-retry"
	replayed, err := service.Delete(t.Context(), input)
	if err != nil || replayed.ID != first.ID || repository.writes != 2 {
		t.Fatalf("delete retry did not reuse original operation: replay=%#v err=%v writes=%d", replayed, err, repository.writes)
	}
	changed := input
	changed.ExpectedGeneration = 0
	if _, err := service.Delete(t.Context(), changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("delete retry silently dropped generation precondition: %v", err)
	}
	changed = input
	changed.DeploymentID = "other"
	if _, err := service.Delete(t.Context(), changed); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("delete retry silently changed target: %v", err)
	}
}

func TestMutationRaceFallbackChecksImmutableRequest(t *testing.T) {
	for _, kind := range []OperationKind{OperationCreate, OperationUpdate, OperationDelete} {
		for _, mismatch := range []string{"matching", "request", "kind", "legacy", "tenant"} {
			t.Run(string(kind)+"/"+mismatch, func(t *testing.T) {
				service, repository, createInput, created := idempotencyFixture(t)
				createInput.IdempotencyKey = "race"
				updateInput := UpdateInput{TenantID: createInput.TenantID, DeploymentID: created.Deployment.ID,
					ExpectedGeneration: 1, Spec: testSpec(), IdempotencyKey: "race"}
				deleteInput := DeleteInput{TenantID: createInput.TenantID, DeploymentID: created.Deployment.ID,
					ExpectedGeneration: 1, IdempotencyKey: "race"}
				stored := NewOperation(createInput.TenantID, created.Deployment.ID, kind, "winner", "race", time.Now())
				var call func() (*Operation, error)
				switch kind {
				case OperationCreate:
					stored.RequestDigest = createRequestDigest(createInput)
					call = func() (*Operation, error) {
						value, err := service.Create(t.Context(), createInput)
						if err != nil {
							return nil, err
						}
						return value.Operation, nil
					}
				case OperationUpdate:
					stored.RequestDigest = updateRequestDigest(updateInput)
					call = func() (*Operation, error) {
						value, err := service.Update(t.Context(), updateInput)
						if err != nil {
							return nil, err
						}
						return value.Operation, nil
					}
				case OperationDelete:
					stored.RequestDigest = deleteRequestDigest(deleteInput)
					call = func() (*Operation, error) { return service.Delete(t.Context(), deleteInput) }
				}
				switch mismatch {
				case "request":
					stored.RequestDigest = strings.Repeat("a", 64)
				case "kind":
					stored.Kind = "different-operation"
				case "legacy":
					stored.RequestDigest = ""
				case "tenant":
					stored.TenantID = "other-tenant"
				}
				repository.operations[createInput.TenantID+":race"] = stored
				repository.hiddenLookups = 1
				repository.writeError = ErrAlreadyExists
				replayed, err := call()
				if mismatch == "matching" {
					if err != nil || replayed.ID != stored.ID {
						t.Fatalf("winning request was not replayed: %#v %v", replayed, err)
					}
				} else if !errors.Is(err, ErrIdempotencyConflict) {
					t.Fatalf("%s race must fail closed, got %#v %v", mismatch, replayed, err)
				}
				if repository.writes != 2 {
					t.Fatalf("race fallback not exercised: writes=%d", repository.writes)
				}
			})
		}
	}
}

func TestUpdateReplaysWhenConcurrentCommitPrecedesGenerationRead(t *testing.T) {
	service, repository, _, created := idempotencyFixture(t)
	input := UpdateInput{TenantID: created.Deployment.TenantID, DeploymentID: created.Deployment.ID,
		ExpectedGeneration: 1, Spec: testSpec(), IdempotencyKey: "update"}
	first, err := service.Update(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	repository.hiddenLookups = 1
	replayed, err := service.Update(t.Context(), input)
	if err != nil || replayed.Operation.ID != first.Operation.ID || repository.writes != 2 {
		t.Fatalf("generation read race duplicated/rejected identical update: %#v %v writes=%d", replayed, err, repository.writes)
	}
}

func TestHistoricalOperationWithoutRequestDigestFailsClosed(t *testing.T) {
	service, repository, input, created := idempotencyFixture(t)
	repository.operations[input.TenantID+":"+input.IdempotencyKey].RequestDigest = ""
	if _, err := service.Create(t.Context(), input); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("historical operation cannot prove original request identity: %v", err)
	}
	encoded, err := json.Marshal(created.Operation)
	if err != nil {
		t.Fatal(err)
	}
	if created.Operation.RequestDigest == "" || strings.Contains(string(encoded), created.Operation.RequestDigest) || strings.Contains(string(encoded), "requestDigest") {
		t.Fatalf("internal request fingerprint must be present in storage and absent in API JSON: %s", encoded)
	}
}
