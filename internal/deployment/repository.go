package deployment

import (
	"context"
	"errors"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
)

// Repository atomically persists a deployment mutation, its immutable revision
// when one is supplied, and the matching sync-outbox event.
type Repository interface {
	Create(context.Context, *Deployment, *Revision, *Operation) error
	Get(context.Context, string, string) (*Deployment, error)
	GetByID(context.Context, string) (*Deployment, error)
	List(context.Context, string, ListOptions) (*ListPage, error)
	Update(context.Context, *Deployment, *Revision, *Operation, int64) error
	SoftDelete(context.Context, string, string, int64, time.Time, *Operation) error
	GetOperation(context.Context, string, string) (*Operation, error)
	FindOperationByIdempotency(context.Context, string, string) (*Operation, error)
	UpdateObservedStatus(context.Context, string, platformv1alpha1.InferenceDeploymentStatus, time.Time) error
}

type Service struct {
	repository Repository
	now        func() time.Time
}

func NewService(repository Repository) *Service {
	return &Service{repository: repository, now: time.Now}
}

func (s *Service) Create(ctx context.Context, input CreateInput) (*Mutation, error) {
	requestDigest := createRequestDigest(input)
	if input.IdempotencyKey != "" {
		if replay, err := s.replayMutation(ctx, input.TenantID, input.IdempotencyKey, OperationCreate, "", requestDigest); err == nil {
			return replay, nil
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	d, revision, err := New(input, s.now())
	if err != nil {
		return nil, err
	}
	operation := NewOperation(input.TenantID, d.ID, OperationCreate, input.RequestID, input.IdempotencyKey, s.now())
	operation.RequestDigest = requestDigest
	if err := s.repository.Create(ctx, d, revision, operation); err != nil {
		if input.IdempotencyKey != "" {
			if replay, replayErr := s.replayMutation(ctx, input.TenantID, input.IdempotencyKey, OperationCreate, "", requestDigest); replayErr == nil {
				return replay, nil
			} else if !errors.Is(replayErr, ErrNotFound) {
				return nil, replayErr
			}
		}
		return nil, err
	}
	return &Mutation{Deployment: d, Operation: operation}, nil
}

func (s *Service) Get(ctx context.Context, tenantID, id string) (*Deployment, error) {
	return s.repository.Get(ctx, tenantID, id)
}

func (s *Service) List(ctx context.Context, tenantID string, options ListOptions) (*ListPage, error) {
	if options.Limit <= 0 {
		options.Limit = 50
	}
	if options.Limit > 200 {
		options.Limit = 200
	}
	if options.Offset < 0 {
		options.Offset = 0
	}
	return s.repository.List(ctx, tenantID, options)
}

func (s *Service) GetOperation(ctx context.Context, tenantID, id string) (*Operation, error) {
	return s.repository.GetOperation(ctx, tenantID, id)
}

func (s *Service) Update(ctx context.Context, input UpdateInput) (*Mutation, error) {
	requestDigest := updateRequestDigest(input)
	if input.IdempotencyKey != "" {
		if replay, err := s.replayMutation(ctx, input.TenantID, input.IdempotencyKey, OperationUpdate, input.DeploymentID, requestDigest); err == nil {
			return replay, nil
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	current, err := s.repository.Get(ctx, input.TenantID, input.DeploymentID)
	if err != nil {
		return nil, err
	}
	if current.Generation != input.ExpectedGeneration {
		// Another identical request may have committed after our first
		// idempotency lookup but before this read of the desired generation.
		if input.IdempotencyKey != "" {
			if replay, replayErr := s.replayMutation(ctx, input.TenantID, input.IdempotencyKey, OperationUpdate, input.DeploymentID, requestDigest); replayErr == nil {
				return replay, nil
			} else if !errors.Is(replayErr, ErrNotFound) {
				return nil, replayErr
			}
		}
		return nil, ErrGenerationConflict
	}
	updated := *current
	updated.Spec = input.Spec
	updated.Generation++
	updated.UpdatedAt = s.now().UTC()
	if err := Validate(&updated); err != nil {
		return nil, err
	}

	var revision *Revision
	if ServingDigest(current.Spec) != ServingDigest(updated.Spec) || input.ForceReselect {
		revision = &Revision{
			ID:               newID("rev"),
			DeploymentID:     updated.ID,
			Number:           updated.Generation,
			ServingDigest:    ServingDigest(updated.Spec),
			Spec:             updated.Spec,
			State:            RevisionCandidate,
			RequestedBackend: updated.Spec.Runtime.Backend,
			SelectionStatus:  "pending",
			CreatedAt:        updated.UpdatedAt,
		}
		updated.CandidateRevisionID = revision.ID
		updated.State = StateUpdating
	}
	operation := NewOperation(input.TenantID, updated.ID, OperationUpdate, input.RequestID, input.IdempotencyKey, s.now())
	operation.RequestDigest = requestDigest
	if err := s.repository.Update(ctx, &updated, revision, operation, input.ExpectedGeneration); err != nil {
		if input.IdempotencyKey != "" {
			if replay, replayErr := s.replayMutation(ctx, input.TenantID, input.IdempotencyKey, OperationUpdate, input.DeploymentID, requestDigest); replayErr == nil {
				return replay, nil
			} else if !errors.Is(replayErr, ErrNotFound) {
				return nil, replayErr
			}
		}
		return nil, err
	}
	return &Mutation{Deployment: &updated, Operation: operation}, nil
}

func (s *Service) Delete(ctx context.Context, input DeleteInput) (*Operation, error) {
	requestDigest := deleteRequestDigest(input)
	if input.IdempotencyKey != "" {
		if operation, err := s.replayOperation(ctx, input.TenantID, input.IdempotencyKey, OperationDelete, input.DeploymentID, requestDigest); err == nil {
			return operation, nil
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	operation := NewOperation(input.TenantID, input.DeploymentID, OperationDelete, input.RequestID, input.IdempotencyKey, s.now())
	operation.RequestDigest = requestDigest
	if err := s.repository.SoftDelete(ctx, input.TenantID, input.DeploymentID, input.ExpectedGeneration, s.now().UTC(), operation); err != nil {
		if input.IdempotencyKey != "" {
			if replay, replayErr := s.replayOperation(ctx, input.TenantID, input.IdempotencyKey, OperationDelete, input.DeploymentID, requestDigest); replayErr == nil {
				return replay, nil
			} else if !errors.Is(replayErr, ErrNotFound) {
				return nil, replayErr
			}
		}
		return nil, err
	}
	return operation, nil
}

func (s *Service) replayMutation(ctx context.Context, tenantID, key string, kind OperationKind, deploymentID, requestDigest string) (*Mutation, error) {
	operation, err := s.replayOperation(ctx, tenantID, key, kind, deploymentID, requestDigest)
	if err != nil {
		return nil, err
	}
	// A replay is bound to the original operation even after later spec
	// mutations or deletion. Read tombstones too, so retrying create cannot
	// accidentally turn into a new create after its deployment was deleted.
	value, err := s.repository.GetByID(ctx, operation.DeploymentID)
	if err != nil {
		return nil, err
	}
	if value == nil || value.TenantID != tenantID || value.ID != operation.DeploymentID {
		return nil, ErrIdempotencyConflict
	}
	return &Mutation{Deployment: value, Operation: operation}, nil
}

func (s *Service) replayOperation(ctx context.Context, tenantID, key string, kind OperationKind, deploymentID, requestDigest string) (*Operation, error) {
	operation, err := s.repository.FindOperationByIdempotency(ctx, tenantID, key)
	if err != nil {
		return nil, err
	}
	// Old rows have no reconstructable original request. Fail closed rather
	// than treating a mutable deployment row as proof of request identity.
	if operation == nil || operation.TenantID != tenantID || operation.Kind != kind ||
		operation.RequestDigest == "" || operation.RequestDigest != requestDigest ||
		(deploymentID != "" && operation.DeploymentID != deploymentID) {
		return nil, ErrIdempotencyConflict
	}
	return operation, nil
}
