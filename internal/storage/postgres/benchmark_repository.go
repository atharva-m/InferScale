package postgres

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/benchmark"
	"github.com/inferscale/inferscale/internal/deployment"
	kubeutil "github.com/inferscale/inferscale/internal/kubernetes"
	"github.com/jackc/pgx/v5"
)

type BenchmarkRepository struct{ store *Store }

func NewBenchmarkRepository(store *Store) *BenchmarkRepository {
	return &BenchmarkRepository{store: store}
}

var (
	_ deployment.BenchmarkRepository = (*BenchmarkRepository)(nil)
	_ benchmark.Repository           = (*BenchmarkRepository)(nil)
	_ benchmark.PendingRunRepository = (*BenchmarkRepository)(nil)
)

const deploymentBenchmarkColumns = `id, tenant_id, deployment_id, revision_id, scenario, scenario_digest,
	configuration, artifact_uri, provenance, idempotency_key, state,
	rental_price_usd_per_gpu_hour, created_at, started_at, completed_at, error`

func (r *BenchmarkRepository) CreateBenchmark(ctx context.Context, run *deployment.BenchmarkRun) error {
	provenance := run.Provenance
	if len(provenance) == 0 {
		provenance = json.RawMessage(`{}`)
	}
	_, err := r.store.pool.Exec(ctx, `
		INSERT INTO benchmark_runs (`+deploymentBenchmarkColumns+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		run.ID, run.TenantID, run.DeploymentID, run.RevisionID, run.Scenario,
		run.ScenarioDigest, run.Configuration, run.ArtifactURI, provenance, run.IdempotencyKey, run.State,
		run.RentalPriceUSD, run.CreatedAt, run.StartedAt, run.CompletedAt, run.Error)
	return err
}

func (r *BenchmarkRepository) ListBenchmarks(ctx context.Context, tenantID, deploymentID string, options deployment.ListOptions) (*deployment.BenchmarkPage, error) {
	query := `SELECT ` + deploymentBenchmarkColumns + `
		FROM benchmark_runs WHERE tenant_id=$1 AND deployment_id=$2
		ORDER BY created_at DESC, id DESC LIMIT $3`
	arguments := []any{tenantID, deploymentID, options.Limit + 1}
	if options.Cursor != nil {
		query = `SELECT ` + deploymentBenchmarkColumns + `
			FROM benchmark_runs WHERE tenant_id=$1 AND deployment_id=$2
			  AND (created_at, id) < ($3, $4)
			ORDER BY created_at DESC, id DESC LIMIT $5`
		arguments = []any{tenantID, deploymentID, options.Cursor.CreatedAt, options.Cursor.ID, options.Limit + 1}
	}
	rows, err := r.store.pool.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]deployment.BenchmarkRun, 0, options.Limit+1)
	for rows.Next() {
		run, err := scanDeploymentBenchmark(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, run)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	page := &deployment.BenchmarkPage{Items: values}
	if len(page.Items) > options.Limit {
		page.Items = page.Items[:options.Limit]
		last := page.Items[len(page.Items)-1]
		page.NextCursor = &deployment.ListCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func (r *BenchmarkRepository) FindBenchmarkByIdempotency(ctx context.Context, tenantID, key string) (*deployment.BenchmarkRun, error) {
	run, err := scanDeploymentBenchmark(r.store.pool.QueryRow(ctx, `
		SELECT `+deploymentBenchmarkColumns+`
		FROM benchmark_runs WHERE tenant_id=$1 AND idempotency_key=$2`, tenantID, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, deployment.ErrBenchmarkNotFound
	}
	if err != nil {
		return nil, err
	}
	return &run, nil
}

func scanDeploymentBenchmark(row scanner) (deployment.BenchmarkRun, error) {
	var run deployment.BenchmarkRun
	err := row.Scan(
		&run.ID, &run.TenantID, &run.DeploymentID, &run.RevisionID, &run.Scenario,
		&run.ScenarioDigest, &run.Configuration, &run.ArtifactURI, &run.Provenance,
		&run.IdempotencyKey, &run.State, &run.RentalPriceUSD, &run.CreatedAt,
		&run.StartedAt, &run.CompletedAt, &run.Error,
	)
	return run, err
}

// CreateRun bridges the internal benchmark package to the same table used by
// the public deployment benchmark API.
func (r *BenchmarkRepository) CreateRun(ctx context.Context, run benchmark.Run) error {
	if len(run.Configuration) == 0 {
		run.Configuration = json.RawMessage(`{}`)
	}
	provenance, err := json.Marshal(run.Provenance)
	if err != nil {
		return err
	}
	if run.IdempotencyKey == "" {
		run.IdempotencyKey = run.ID
	}
	publicRun := &deployment.BenchmarkRun{
		ID: run.ID, TenantID: run.TenantID, DeploymentID: run.DeploymentID,
		RevisionID: run.RevisionID, Scenario: run.ScenarioName,
		ScenarioDigest: run.ScenarioDigest, Configuration: run.Configuration,
		ArtifactURI: run.ArtifactURI, Provenance: provenance,
		State: deployment.BenchmarkState(run.State), RentalPriceUSD: run.RentalPriceUSD,
		CreatedAt: run.CreatedAt, StartedAt: run.StartedAt, CompletedAt: run.CompletedAt,
		Error: run.Error, IdempotencyKey: run.IdempotencyKey,
	}
	return r.CreateBenchmark(ctx, publicRun)
}

const internalRunSelect = `SELECT br.id, br.tenant_id, br.deployment_id, br.revision_id,
	br.scenario, br.scenario_digest, br.configuration, br.state, br.provenance,
	br.artifact_uri, br.error, br.rental_price_usd_per_gpu_hour,
	br.idempotency_key, br.created_at, br.started_at, br.completed_at, d.namespace,
	d.name, d.observed_status, d.spec, results.metrics, dr.spec, dr.requested_backend,
	COALESCE(dr.resolved_backend,''), COALESCE(dr.runtime_image_digest,''),
	COALESCE(br.execution_configuration, 'null'::jsonb),
	COALESCE(br.execution_digest,''), COALESCE(br.execution_contract, 'null'::jsonb)
	FROM benchmark_runs br
	JOIN deployments d ON d.id=br.deployment_id
	JOIN deployment_revisions dr ON dr.id=br.revision_id AND dr.deployment_id=br.deployment_id
	LEFT JOIN benchmark_results results ON results.benchmark_run_id=br.id`

func (r *BenchmarkRepository) GetRun(ctx context.Context, id string) (benchmark.Run, error) {
	run, err := scanInternalRun(r.store.pool.QueryRow(ctx, internalRunSelect+` WHERE br.id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return benchmark.Run{}, benchmark.ErrRunNotFound
	}
	return run, err
}

func (r *BenchmarkRepository) ListRuns(ctx context.Context, tenantID, deploymentID, cursor string, limit int) ([]benchmark.Run, string, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := internalRunSelect + ` WHERE br.tenant_id=$1 AND br.deployment_id=$2`
	arguments := []any{tenantID, deploymentID}
	if cursor != "" {
		createdAt, id, err := decodeRunCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		query += ` AND (br.created_at, br.id) < ($3,$4)`
		arguments = append(arguments, createdAt, id)
	}
	arguments = append(arguments, limit+1)
	query += fmt.Sprintf(` ORDER BY br.created_at DESC, br.id DESC LIMIT $%d`, len(arguments))
	rows, err := r.store.pool.Query(ctx, query, arguments...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	runs := make([]benchmark.Run, 0, limit+1)
	for rows.Next() {
		run, err := scanInternalRun(rows)
		if err != nil {
			return nil, "", err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(runs) > limit {
		runs = runs[:limit]
		last := runs[len(runs)-1]
		next = encodeRunCursor(last.CreatedAt, last.ID)
	}
	return runs, next, nil
}

func (r *BenchmarkRepository) ClaimPendingRun(ctx context.Context, workerID string, lease time.Duration) (benchmark.Run, error) {
	if strings.TrimSpace(workerID) == "" || lease <= 0 {
		return benchmark.Run{}, errors.New("benchmark worker ID and positive lease are required")
	}
	var runID string
	err := r.store.pool.QueryRow(ctx, `
		WITH candidate AS (
			SELECT br.id
			FROM benchmark_runs br
			WHERE br.state=$1 OR (br.state=$2 AND br.lease_expires_at <= now())
			ORDER BY br.created_at, br.id
			FOR UPDATE OF br SKIP LOCKED
			LIMIT 1
		), claimed AS (
			UPDATE benchmark_runs br SET
				state=$2,
				started_at=COALESCE(br.started_at, now()),
				lease_owner=$3,
				lease_expires_at=now() + ($4 * interval '1 second'),
				attempt_count=br.attempt_count+1
			FROM candidate c WHERE br.id=c.id
			RETURNING br.id
		)
		SELECT id FROM claimed`, benchmark.RunPending, benchmark.RunRunning, workerID, lease.Seconds()).Scan(&runID)
	if errors.Is(err, pgx.ErrNoRows) {
		return benchmark.Run{}, nil
	}
	if err != nil {
		return benchmark.Run{}, err
	}
	return r.GetRun(ctx, runID)
}

func (r *BenchmarkRepository) PrepareClaimedRun(ctx context.Context, run benchmark.Run, workerID string) (benchmark.Run, error) {
	if strings.TrimSpace(run.ID) == "" || strings.TrimSpace(workerID) == "" ||
		len(run.ExecutionConfiguration) == 0 || benchmark.ConfigurationDigest(run.ExecutionConfiguration) != run.ExecutionDigest {
		return benchmark.Run{}, fmt.Errorf("%w: complete claimed-run execution snapshot is required", benchmark.ErrInvalidReport)
	}
	contract, err := json.Marshal(run.Execution)
	if err != nil {
		return benchmark.Run{}, err
	}
	command, err := r.store.pool.Exec(ctx, `
		UPDATE benchmark_runs SET execution_configuration=$3, execution_digest=$4,
			execution_contract=$5
		WHERE id=$1 AND state=$2 AND lease_owner=$6
		  AND (execution_digest IS NULL OR
		       (execution_digest=$4 AND execution_configuration=$3 AND execution_contract=$5))`,
		run.ID, benchmark.RunRunning, run.ExecutionConfiguration, run.ExecutionDigest, contract, workerID)
	if err != nil {
		return benchmark.Run{}, err
	}
	if command.RowsAffected() != 1 {
		return benchmark.Run{}, benchmark.ErrInvalidTransition
	}
	return r.GetRun(ctx, run.ID)
}

func (r *BenchmarkRepository) RenewRunLease(ctx context.Context, runID, workerID string, lease time.Duration) error {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(workerID) == "" || lease <= 0 {
		return errors.New("benchmark run ID, worker ID, and positive lease are required")
	}
	command, err := r.store.pool.Exec(ctx, `
		UPDATE benchmark_runs SET lease_expires_at=now() + ($3 * interval '1 second')
		WHERE id=$1 AND state=$4 AND lease_owner=$2`, runID, workerID, lease.Seconds(), benchmark.RunRunning)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	var state benchmark.RunState
	if err := r.store.pool.QueryRow(ctx, `SELECT state FROM benchmark_runs WHERE id=$1`, runID).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return benchmark.ErrRunNotFound
		}
		return err
	}
	// A very fast callback can win the race with renewal. Terminal state means
	// there is no lease left to renew and is therefore already safe.
	if state == benchmark.RunSucceeded || state == benchmark.RunFailed {
		return nil
	}
	return benchmark.ErrInvalidTransition
}

func (r *BenchmarkRepository) CompleteRun(ctx context.Context, runID string, result benchmark.Measurements, provenance map[string]any, artifactURI string, profile *benchmark.RuntimeProfile, at time.Time) (benchmark.Run, error) {
	tx, err := r.store.pool.Begin(ctx)
	if err != nil {
		return benchmark.Run{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var state benchmark.RunState
	var storedScenarioDigest, revisionID string
	var revisionSpec []byte
	var requestedBackend, resolvedBackend, runtimeImageDigest string
	if err := tx.QueryRow(ctx, `
		SELECT br.state, br.scenario_digest, br.revision_id, dr.spec,
		       dr.requested_backend, COALESCE(dr.resolved_backend,''),
		       COALESCE(dr.runtime_image_digest,'')
		FROM benchmark_runs br
		JOIN deployment_revisions dr ON dr.id=br.revision_id AND dr.deployment_id=br.deployment_id
		WHERE br.id=$1 FOR UPDATE OF br`, runID).Scan(
		&state, &storedScenarioDigest, &revisionID, &revisionSpec,
		&requestedBackend, &resolvedBackend, &runtimeImageDigest,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return benchmark.Run{}, benchmark.ErrRunNotFound
		}
		return benchmark.Run{}, err
	}
	if state == benchmark.RunSucceeded {
		_ = tx.Rollback(ctx)
		return r.GetRun(ctx, runID)
	}
	if state != benchmark.RunRunning {
		return benchmark.Run{}, benchmark.ErrInvalidTransition
	}
	metrics, err := json.Marshal(result)
	if err != nil {
		return benchmark.Run{}, err
	}
	provenanceJSON, err := json.Marshal(provenance)
	if err != nil {
		return benchmark.Run{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO benchmark_results (benchmark_run_id, provenance, metrics, created_at)
		VALUES ($1,$2,$3,$4)`, runID, provenanceJSON, metrics, at); err != nil {
		return benchmark.Run{}, err
	}
	if profile != nil {
		if profile.BenchmarkRunID != runID {
			return benchmark.Run{}, fmt.Errorf("%w: profile source run does not match callback", benchmark.ErrInvalidReport)
		}
		var spec platformv1alpha1.InferenceDeploymentSpec
		if err := json.Unmarshal(revisionSpec, &spec); err != nil {
			return benchmark.Run{}, fmt.Errorf("decode benchmark revision %s: %w", revisionID, err)
		}
		if err := validateMeasuredProfile(profile.Key, spec, requestedBackend, resolvedBackend, runtimeImageDigest); err != nil {
			return benchmark.Run{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO runtime_profiles (
				id, model_uri, model_revision, backend, backend_version,
				runtime_image_digest, gpu_sku, gpu_count, precision, quantization,
				tensor_parallelism, max_context_bucket, driver_cuda_fingerprint,
				scenario_digest, metrics, cost_per_successful_request, eligible,
				approved_by, approved_at, measured_at, source_benchmark_run_id
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,false,NULL,NULL,$17,$18)`,
			profile.ID, profile.Key.ModelURI, profile.Key.ModelRevision,
			profile.Key.Backend, profile.Key.BackendVersion, profile.Key.RuntimeImageDigest,
			profile.Key.GPUSKU, profile.Key.GPUCount, profile.Key.Precision,
			profile.Key.Quantization, profile.Key.TensorParallelism,
			profile.Key.MaxContextBucket, profile.Key.DriverCUDAFingerprint,
			profile.Key.ScenarioDigest, metrics,
			profile.Measurements.CostPerSuccessfulRequest, profile.MeasuredAt, runID,
		); err != nil {
			return benchmark.Run{}, err
		}
	}
	completedScenarioDigest := storedScenarioDigest
	if profile != nil {
		completedScenarioDigest = profile.Key.ScenarioDigest
	}
	command, err := tx.Exec(ctx, `
		UPDATE benchmark_runs SET state=$2, provenance=$3, artifact_uri=$4,
			completed_at=$5, scenario_digest=$6, error='', lease_owner=NULL,
			lease_expires_at=NULL
		WHERE id=$1 AND state=$7`, runID, benchmark.RunSucceeded,
		provenanceJSON, artifactURI, at, completedScenarioDigest, benchmark.RunRunning)
	if err != nil {
		return benchmark.Run{}, err
	}
	if command.RowsAffected() != 1 {
		return benchmark.Run{}, benchmark.ErrInvalidTransition
	}
	if err := tx.Commit(ctx); err != nil {
		return benchmark.Run{}, err
	}
	return r.GetRun(ctx, runID)
}

func (r *BenchmarkRepository) MarkRunFailed(ctx context.Context, runID, reason string) error {
	command, err := r.store.pool.Exec(ctx, `
		UPDATE benchmark_runs SET state=$2, error=$3, completed_at=now(),
			lease_owner=NULL, lease_expires_at=NULL
		WHERE id=$1 AND state IN ($4,$5)`, runID, benchmark.RunFailed,
		reason, benchmark.RunPending, benchmark.RunRunning)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	var exists bool
	if err := r.store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM benchmark_runs WHERE id=$1)`, runID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return benchmark.ErrRunNotFound
	}
	var state benchmark.RunState
	if err := r.store.pool.QueryRow(ctx, `SELECT state FROM benchmark_runs WHERE id=$1`, runID).Scan(&state); err != nil {
		return err
	}
	if state == benchmark.RunFailed {
		return nil
	}
	return benchmark.ErrInvalidTransition
}

func (r *BenchmarkRepository) ApproveProfile(ctx context.Context, profileID, operator string, at time.Time) error {
	command, err := r.store.pool.Exec(ctx, `
		UPDATE runtime_profiles SET eligible=true, approved_by=$2, approved_at=$3
		WHERE id=$1 AND source_benchmark_run_id IS NOT NULL`, profileID, operator, at)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrRuntimeProfileNotFound
	}
	return nil
}

func scanInternalRun(row scanner) (benchmark.Run, error) {
	var run benchmark.Run
	var provenanceJSON, metricsJSON, revisionSpecJSON, deploymentSpecJSON, observedStatusJSON []byte
	var executionConfiguration, executionContractJSON []byte
	var requestedBackend, resolvedBackend, runtimeImageDigest string
	err := row.Scan(
		&run.ID, &run.TenantID, &run.DeploymentID, &run.RevisionID,
		&run.ScenarioName, &run.ScenarioDigest, &run.Configuration, &run.State,
		&provenanceJSON, &run.ArtifactURI, &run.Error, &run.RentalPriceUSD,
		&run.IdempotencyKey, &run.CreatedAt, &run.StartedAt, &run.CompletedAt,
		&run.Namespace, &run.DeploymentName, &observedStatusJSON, &deploymentSpecJSON, &metricsJSON, &revisionSpecJSON,
		&requestedBackend, &resolvedBackend, &runtimeImageDigest,
		&executionConfiguration, &run.ExecutionDigest, &executionContractJSON,
	)
	if err != nil {
		return benchmark.Run{}, err
	}
	if len(provenanceJSON) > 0 {
		if err := json.Unmarshal(provenanceJSON, &run.Provenance); err != nil {
			return benchmark.Run{}, err
		}
	}
	if len(metricsJSON) > 0 {
		var result benchmark.Measurements
		if err := json.Unmarshal(metricsJSON, &result); err != nil {
			return benchmark.Run{}, err
		}
		run.Result = &result
	}
	var revisionSpec platformv1alpha1.InferenceDeploymentSpec
	if err := json.Unmarshal(revisionSpecJSON, &revisionSpec); err != nil {
		return benchmark.Run{}, err
	}
	var deploymentSpec platformv1alpha1.InferenceDeploymentSpec
	if err := json.Unmarshal(deploymentSpecJSON, &deploymentSpec); err != nil {
		return benchmark.Run{}, fmt.Errorf("decode current deployment policy: %w", err)
	}
	backend := resolvedBackend
	if backend == "" && requestedBackend != string(platformv1alpha1.RuntimeBackendAuto) {
		backend = requestedBackend
	}
	run.Serving = benchmark.ServingContract{
		ModelURI: revisionSpec.Model.URI, ModelRevision: revisionSpec.Model.Revision,
		Backend: benchmark.Backend(backend), RuntimeImageDigest: runtimeImageDigest,
		GPUSKU: revisionSpec.Accelerator.Type, GPUCount: revisionSpec.Accelerator.Count,
		Precision: revisionSpec.Runtime.Precision, Quantization: revisionSpec.Runtime.Quantization,
		TensorParallelism: revisionSpec.Runtime.TensorParallelism,
		MaxContextBucket:  revisionSpec.Runtime.MaxModelLen,
		PrefixCaching:     revisionSpec.Runtime.PrefixCaching.Enabled,
		RoutingPolicy:     string(deploymentSpec.Routing.Policy),
	}
	var observed platformv1alpha1.InferenceDeploymentStatus
	if len(observedStatusJSON) > 0 {
		if err := json.Unmarshal(observedStatusJSON, &observed); err != nil {
			return benchmark.Run{}, fmt.Errorf("decode deployment controller status: %w", err)
		}
	}
	runtimeRevision := ""
	switch run.RevisionID {
	case observed.Revision.StableID:
		runtimeRevision = observed.Revision.Stable
	case observed.Revision.CandidateID:
		runtimeRevision = observed.Revision.Candidate
	}
	if runtimeRevision != "" {
		suffix := "vllm"
		if run.Serving.Backend == benchmark.BackendTensorRTLLM {
			suffix = "trtllm"
		}
		run.RuntimePodPrefix = kubeutil.ResourceName(runtimeRevision, suffix)
		run.EPPService = kubeutil.ResourceName(runtimeRevision, "epp")
	}
	if string(executionConfiguration) != "null" {
		run.ExecutionConfiguration = append(json.RawMessage(nil), executionConfiguration...)
	}
	if string(executionContractJSON) != "null" {
		if err := json.Unmarshal(executionContractJSON, &run.Execution); err != nil {
			return benchmark.Run{}, err
		}
	}
	return run, nil
}

func validateMeasuredProfile(key benchmark.ProfileKey, spec platformv1alpha1.InferenceDeploymentSpec, requestedBackend, resolvedBackend, runtimeImageDigest string) error {
	actualBackend := resolvedBackend
	if actualBackend == "" && requestedBackend != string(platformv1alpha1.RuntimeBackendAuto) {
		actualBackend = requestedBackend
	}
	if actualBackend == "" {
		return fmt.Errorf("%w: benchmark revision has no frozen backend", benchmark.ErrInvalidReport)
	}
	if key.ModelURI != spec.Model.URI || key.ModelRevision != spec.Model.Revision ||
		string(key.Backend) != actualBackend || key.GPUSKU != spec.Accelerator.Type ||
		key.GPUCount != spec.Accelerator.Count || key.Precision != spec.Runtime.Precision ||
		key.Quantization != spec.Runtime.Quantization || key.TensorParallelism != spec.Runtime.TensorParallelism ||
		key.MaxContextBucket != spec.Runtime.MaxModelLen {
		return fmt.Errorf("%w: profile key does not match the immutable benchmark revision", benchmark.ErrInvalidReport)
	}
	if runtimeImageDigest != "" && key.RuntimeImageDigest != runtimeImageDigest {
		return fmt.Errorf("%w: profile runtime image does not match the frozen revision", benchmark.ErrInvalidReport)
	}
	return nil
}

func encodeRunCursor(createdAt time.Time, id string) string {
	payload, _ := json.Marshal(struct {
		CreatedAt time.Time `json:"t"`
		ID        string    `json:"i"`
	}{createdAt, id})
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeRunCursor(raw string) (time.Time, string, error) {
	payload, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return time.Time{}, "", err
	}
	var value struct {
		CreatedAt time.Time `json:"t"`
		ID        string    `json:"i"`
	}
	if err := json.Unmarshal(payload, &value); err != nil || value.CreatedAt.IsZero() || value.ID == "" {
		return time.Time{}, "", errors.New("invalid benchmark cursor")
	}
	return value.CreatedAt, value.ID, nil
}
