package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/deployment"
	platformsync "github.com/inferscale/inferscale/internal/sync"
	"github.com/inferscale/inferscale/internal/tenant"
	"github.com/jackc/pgx/v5"
)

type DeploymentRepository struct{ store *Store }

func NewDeploymentRepository(store *Store) *DeploymentRepository {
	return &DeploymentRepository{store: store}
}

func (r *DeploymentRepository) Create(ctx context.Context, value *deployment.Deployment, revision *deployment.Revision, operation *deployment.Operation) error {
	tx, err := r.store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := enforceQuota(ctx, tx, value.TenantID, "", value.Spec); err != nil {
		return err
	}
	spec, err := json.Marshal(value.Spec)
	if err != nil {
		return err
	}
	status, err := json.Marshal(value.ObservedStatus)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO deployments (
			id, tenant_id, name, namespace, generation, spec, state,
			stable_revision_id, candidate_revision_id, observed_status,
			last_sync_error, created_at, updated_at, deleted_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		value.ID, value.TenantID, value.Name, value.Namespace, value.Generation,
		spec, value.State, nullable(value.StableRevisionID), nullable(value.CandidateRevisionID),
		status, value.LastSyncError, value.CreatedAt, value.UpdatedAt, value.DeletedAt)
	if uniqueViolation(err) {
		return deployment.ErrAlreadyExists
	}
	if err != nil {
		return err
	}
	if err := insertRevision(ctx, tx, revision); err != nil {
		return err
	}
	if err := insertOperation(ctx, tx, operation); err != nil {
		return err
	}
	if err := enqueue(ctx, tx, value.ID, operation.ID, platformsync.EventDeploymentUpsert, json.RawMessage(`{}`)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *DeploymentRepository) Get(ctx context.Context, tenantID, id string) (*deployment.Deployment, error) {
	return scanDeployment(r.store.pool.QueryRow(ctx, deploymentSelect+` WHERE tenant_id=$1 AND id=$2 AND deleted_at IS NULL`, tenantID, id))
}

func (r *DeploymentRepository) GetByID(ctx context.Context, id string) (*deployment.Deployment, error) {
	return scanDeployment(r.store.pool.QueryRow(ctx, deploymentSelect+` WHERE id=$1`, id))
}

// ListForDriftAudit returns a deterministic bounded page of authoritative
// state, including tombstones. UUIDv7 deployment IDs sort by creation time, so
// a cursor completes a full pass without retaining an unbounded snapshot. An
// empty next cursor starts the next repair pass from the beginning.
func (r *DeploymentRepository) ListForDriftAudit(ctx context.Context, afterID string, limit int) ([]deployment.Deployment, string, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.store.pool.Query(ctx, deploymentSelect+`
		WHERE ($1 = '' OR id > $1)
		ORDER BY id ASC LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	values := make([]deployment.Deployment, 0, limit)
	for rows.Next() {
		value, err := scanDeployment(rows)
		if err != nil {
			return nil, "", err
		}
		values = append(values, *value)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(values) == limit {
		next = values[len(values)-1].ID
	}
	return values, next, nil
}

func (r *DeploymentRepository) List(ctx context.Context, tenantID string, options deployment.ListOptions) (*deployment.ListPage, error) {
	query := deploymentSelect + `
		WHERE tenant_id=$1 AND deleted_at IS NULL
		ORDER BY created_at DESC, id DESC LIMIT $2`
	arguments := []any{tenantID, options.Limit + 1}
	if options.Cursor != nil {
		query = deploymentSelect + `
			WHERE tenant_id=$1 AND deleted_at IS NULL
			  AND (created_at, id) < ($2, $3)
			ORDER BY created_at DESC, id DESC LIMIT $4`
		arguments = []any{tenantID, options.Cursor.CreatedAt, options.Cursor.ID, options.Limit + 1}
	}
	rows, err := r.store.pool.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]deployment.Deployment, 0, options.Limit+1)
	for rows.Next() {
		value, err := scanDeployment(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, *value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	page := &deployment.ListPage{Items: values}
	if len(page.Items) > options.Limit {
		page.Items = page.Items[:options.Limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = &deployment.ListCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func (r *DeploymentRepository) Update(ctx context.Context, value *deployment.Deployment, revision *deployment.Revision, operation *deployment.Operation, expectedGeneration int64) error {
	tx, err := r.store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var currentGeneration int64
	var stableRevisionID, candidateRevisionID string
	var state deployment.State
	if err := tx.QueryRow(ctx, `SELECT generation, COALESCE(stable_revision_id,''), COALESCE(candidate_revision_id,''), state
		FROM deployments WHERE id=$1 AND tenant_id=$2 AND deleted_at IS NULL FOR UPDATE`, value.ID, value.TenantID).Scan(&currentGeneration, &stableRevisionID, &candidateRevisionID, &state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return deployment.ErrNotFound
		}
		return err
	}
	if currentGeneration != expectedGeneration {
		return deployment.ErrGenerationConflict
	}
	// Controller promotion does not change the desired-spec generation. Merge
	// policy into the locked row rather than restoring revision pointers from
	// the service's earlier read.
	value.StableRevisionID = stableRevisionID
	if revision == nil {
		value.CandidateRevisionID = candidateRevisionID
		value.State = state
	} else {
		value.CandidateRevisionID = revision.ID
	}
	if err := enforceQuota(ctx, tx, value.TenantID, value.ID, value.Spec); err != nil {
		return err
	}
	if revision != nil {
		if err := insertRevision(ctx, tx, revision); err != nil {
			return err
		}
	}
	spec, err := json.Marshal(value.Spec)
	if err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `
		UPDATE deployments SET spec=$3, generation=$4, state=$5,
			stable_revision_id=$6, candidate_revision_id=$7, updated_at=$8
		WHERE id=$1 AND tenant_id=$2 AND generation=$9 AND deleted_at IS NULL`,
		value.ID, value.TenantID, spec, value.Generation, value.State,
		nullable(value.StableRevisionID), nullable(value.CandidateRevisionID), value.UpdatedAt, expectedGeneration)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return deployment.ErrGenerationConflict
	}
	if err := insertOperation(ctx, tx, operation); err != nil {
		return err
	}
	if err := enqueue(ctx, tx, value.ID, operation.ID, platformsync.EventDeploymentUpsert, json.RawMessage(`{}`)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *DeploymentRepository) SoftDelete(ctx context.Context, tenantID, id string, expectedGeneration int64, at time.Time, operation *deployment.Operation) error {
	tx, err := r.store.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var name, namespace string
	var generation int64
	if err := tx.QueryRow(ctx, `
		SELECT name, namespace, generation FROM deployments
		WHERE tenant_id=$1 AND id=$2 AND deleted_at IS NULL FOR UPDATE`, tenantID, id).Scan(&name, &namespace, &generation); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return deployment.ErrNotFound
		}
		return err
	}
	if expectedGeneration > 0 && expectedGeneration != generation {
		return deployment.ErrGenerationConflict
	}
	_, err = tx.Exec(ctx, `UPDATE deployments SET state=$3, generation=generation+1, updated_at=$4, deleted_at=$4 WHERE tenant_id=$1 AND id=$2`, tenantID, id, deployment.StateDeleting, at)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(platformsync.DeleteTarget{Namespace: namespace, Name: name})
	if err := insertOperation(ctx, tx, operation); err != nil {
		return err
	}
	if err := enqueue(ctx, tx, id, operation.ID, platformsync.EventDeploymentDelete, payload); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *DeploymentRepository) GetOperation(ctx context.Context, tenantID, id string) (*deployment.Operation, error) {
	return r.scanOperation(ctx, `WHERE tenant_id=$1 AND id=$2`, tenantID, id)
}

func (r *DeploymentRepository) FindOperationByIdempotency(ctx context.Context, tenantID, key string) (*deployment.Operation, error) {
	return r.scanOperation(ctx, `WHERE tenant_id=$1 AND idempotency_key=$2`, tenantID, key)
}

func (r *DeploymentRepository) scanOperation(ctx context.Context, predicate string, arguments ...any) (*deployment.Operation, error) {
	var operation deployment.Operation
	err := r.store.pool.QueryRow(ctx, `
		SELECT id, tenant_id, deployment_id, kind, status, request_id,
		       idempotency_key, request_digest, created_at, updated_at, completed_at, error
		FROM operations `+predicate, arguments...).Scan(
		&operation.ID, &operation.TenantID, &operation.DeploymentID, &operation.Kind,
		&operation.Status, &operation.RequestID, &operation.IdempotencyKey, &operation.RequestDigest,
		&operation.CreatedAt, &operation.UpdatedAt, &operation.CompletedAt, &operation.Error)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, deployment.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &operation, nil
}

func (r *DeploymentRepository) UpdateObservedStatus(ctx context.Context, id string, status platformv1alpha1.InferenceDeploymentStatus, at time.Time) error {
	payload, err := json.Marshal(status)
	if err != nil {
		return err
	}
	command, err := r.store.pool.Exec(ctx, `
		UPDATE deployments SET observed_status=$2, state=$3, updated_at=$4, last_sync_error=''
		WHERE id=$1 AND deleted_at IS NULL`, id, payload, status.Phase, at)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return deployment.ErrNotFound
	}
	return nil
}

const deploymentSelect = `SELECT id, tenant_id, name, namespace, generation, spec, state,
	COALESCE(stable_revision_id,''), COALESCE(candidate_revision_id,''), observed_status,
	last_sync_error, created_at, updated_at, deleted_at FROM deployments`

type scanner interface{ Scan(...any) error }

func scanDeployment(row scanner) (*deployment.Deployment, error) {
	var value deployment.Deployment
	var spec, status []byte
	err := row.Scan(&value.ID, &value.TenantID, &value.Name, &value.Namespace,
		&value.Generation, &spec, &value.State, &value.StableRevisionID,
		&value.CandidateRevisionID, &status, &value.LastSyncError,
		&value.CreatedAt, &value.UpdatedAt, &value.DeletedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, deployment.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(spec, &value.Spec); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(status, &value.ObservedStatus); err != nil {
		return nil, err
	}
	return &value, nil
}

func insertRevision(ctx context.Context, tx pgx.Tx, value *deployment.Revision) error {
	if value == nil {
		return nil
	}
	spec, err := json.Marshal(value.Spec)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO deployment_revisions (
			id, deployment_id, number, serving_digest, spec, state,
			requested_backend, resolved_backend, selection_status,
			selected_profile_id, runtime_image_digest, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		value.ID, value.DeploymentID, value.Number, value.ServingDigest, spec, value.State,
		value.RequestedBackend, nullable(string(value.ResolvedBackend)), value.SelectionStatus,
		nullable(value.SelectedProfileID), nullable(value.RuntimeImageDigest), value.CreatedAt)
	return err
}

func enforceQuota(ctx context.Context, tx pgx.Tx, tenantID, excludedDeploymentID string, spec platformv1alpha1.InferenceDeploymentSpec) error {
	var rawQuota []byte
	if err := tx.QueryRow(ctx, `SELECT quota FROM tenants WHERE id=$1 FOR UPDATE`, tenantID).Scan(&rawQuota); err != nil {
		return err
	}
	var quota tenant.Quota
	if err := json.Unmarshal(rawQuota, &quota); err != nil {
		return err
	}
	var count, allocatedGPUs, allocatedConcurrency, allocatedQueue int64
	if err := tx.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(
				((spec->'accelerator'->>'count')::bigint) *
				((spec->'scaling'->>'maxReplicas')::bigint)
		),0), COALESCE(sum((spec->'admission'->>'maxConcurrentRequests')::integer),0),
		COALESCE(sum((spec->'admission'->>'maxQueuedRequests')::integer),0)
		FROM deployments WHERE tenant_id=$1 AND deleted_at IS NULL AND id<>$2`, tenantID, excludedDeploymentID).Scan(&count, &allocatedGPUs, &allocatedConcurrency, &allocatedQueue); err != nil {
		return err
	}
	requestedGPUs := int64(spec.Accelerator.Count) * int64(spec.Scaling.MaxReplicas)
	requestedConcurrency := int64(spec.Admission.MaxConcurrentRequests)
	requestedQueue := int64(spec.Admission.MaxQueuedRequests)
	if !allocationWithinQuota(quota, count, allocatedGPUs, allocatedConcurrency, allocatedQueue, requestedGPUs, requestedConcurrency, requestedQueue) {
		return deployment.ErrQuotaExceeded
	}
	return nil
}

func allocationWithinQuota(quota tenant.Quota, deployments, gpus, concurrency, queue, requestedGPUs, requestedConcurrency, requestedQueue int64) bool {
	return deployments+1 <= int64(quota.MaxDeployments) &&
		gpus+requestedGPUs <= int64(quota.MaxGPUs) &&
		concurrency+requestedConcurrency <= int64(quota.MaxConcurrentRequests) &&
		queue+requestedQueue <= int64(quota.MaxQueuedRequests)
}

func insertOperation(ctx context.Context, tx pgx.Tx, operation *deployment.Operation) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO operations (
			id, tenant_id, deployment_id, kind, status, request_id,
			idempotency_key, request_digest, created_at, updated_at, completed_at, error
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		operation.ID, operation.TenantID, operation.DeploymentID, operation.Kind,
		operation.Status, operation.RequestID, operation.IdempotencyKey, operation.RequestDigest,
		operation.CreatedAt, operation.UpdatedAt, operation.CompletedAt, operation.Error)
	return err
}

func enqueue(ctx context.Context, tx pgx.Tx, aggregateID, operationID string, eventType platformsync.EventType, payload json.RawMessage) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO sync_outbox (aggregate_type, aggregate_id, operation_id, event_type, payload)
		VALUES ('deployment',$1,$2,$3,$4)`, aggregateID, operationID, eventType, payload)
	return err
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
