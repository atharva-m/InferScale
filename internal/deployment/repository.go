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
	if input.IdempotencyKey != "" {
		if replay, err := s.replayMutation(ctx, input.TenantID, input.IdempotencyKey); err == nil {
			if !createReplayMatches(replay, input) {
				return nil, ErrIdempotencyConflict
			}
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
	if err := s.repository.Create(ctx, d, revision, operation); err != nil {
		if input.IdempotencyKey != "" {
			if replay, replayErr := s.replayMutation(ctx, input.TenantID, input.IdempotencyKey); replayErr == nil {
				if !createReplayMatches(replay, input) {
					return nil, ErrIdempotencyConflict
				}
				return replay, nil
			}
		}
		return nil, err
	}
	return &Mutation{Deployment: d, Operation: operation}, nil
}

func createReplayMatches(replay *Mutation, input CreateInput) bool {
	return replay != nil && replay.Operation != nil && replay.Deployment != nil &&
		replay.Operation.Kind == OperationCreate && replay.Deployment.Name == input.Name &&
		replay.Deployment.Namespace == input.Namespace && SpecDigest(replay.Deployment.Spec) == SpecDigest(input.Spec)
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
	if input.IdempotencyKey != "" {
		if replay, err := s.replayMutation(ctx, input.TenantID, input.IdempotencyKey); err == nil {
			if replay.Operation.Kind != OperationUpdate || replay.Operation.DeploymentID != input.DeploymentID {
				return nil, ErrIdempotencyConflict
			}
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
	if err := s.repository.Update(ctx, &updated, revision, operation, input.ExpectedGeneration); err != nil {
		if input.IdempotencyKey != "" {
			if replay, replayErr := s.replayMutation(ctx, input.TenantID, input.IdempotencyKey); replayErr == nil {
				return replay, nil
			}
		}
		return nil, err
	}
	return &Mutation{Deployment: &updated, Operation: operation}, nil
}

func (s *Service) Delete(ctx context.Context, input DeleteInput) (*Operation, error) {
	if input.IdempotencyKey != "" {
		if operation, err := s.repository.FindOperationByIdempotency(ctx, input.TenantID, input.IdempotencyKey); err == nil {
			if operation.Kind != OperationDelete || operation.DeploymentID != input.DeploymentID {
				return nil, ErrIdempotencyConflict
			}
			return operation, nil
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	operation := NewOperation(input.TenantID, input.DeploymentID, OperationDelete, input.RequestID, input.IdempotencyKey, s.now())
	if err := s.repository.SoftDelete(ctx, input.TenantID, input.DeploymentID, input.ExpectedGeneration, s.now().UTC(), operation); err != nil {
		if input.IdempotencyKey != "" {
			if replay, replayErr := s.repository.FindOperationByIdempotency(ctx, input.TenantID, input.IdempotencyKey); replayErr == nil {
				return replay, nil
			}
		}
		return nil, err
	}
	return operation, nil
}

func (s *Service) replayMutation(ctx context.Context, tenantID, key string) (*Mutation, error) {
	operation, err := s.repository.FindOperationByIdempotency(ctx, tenantID, key)
	if err != nil {
		return nil, err
	}
	value, err := s.repository.Get(ctx, tenantID, operation.DeploymentID)
	if err != nil {
		return nil, err
	}
	return &Mutation{Deployment: value, Operation: operation}, nil
}
