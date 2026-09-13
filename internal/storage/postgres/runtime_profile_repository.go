package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/inferscale/inferscale/internal/deployment"
)

var ErrRuntimeProfileNotFound = errors.New("runtime profile not found")

type RuntimeProfileRepository struct{ store *Store }

func NewRuntimeProfileRepository(store *Store) *RuntimeProfileRepository {
	return &RuntimeProfileRepository{store: store}
}

func (r *RuntimeProfileRepository) AppendRuntimeProfile(ctx context.Context, profile *deployment.RuntimeProfile) error {
	_, err := r.store.pool.Exec(ctx, `
		INSERT INTO runtime_profiles (
			id, model_uri, model_revision, backend, backend_version, runtime_image_digest,
			gpu_sku, gpu_count, precision, quantization, tensor_parallelism,
			max_context_bucket, driver_cuda_fingerprint, scenario_digest, metrics,
			cost_per_successful_request, eligible, approved_by, approved_at,
			measured_at, source_benchmark_run_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`,
		profile.ID, profile.ModelURI, profile.ModelRevision, profile.Backend, profile.BackendVersion,
		profile.RuntimeImageDigest, profile.GPUSKU, profile.GPUCount, profile.Precision,
		profile.Quantization, profile.TensorParallelism, profile.MaxContextBucket,
		profile.DriverCUDAFingerprint, profile.ScenarioDigest, profile.Metrics,
		profile.CostPerSuccessfulRequest, profile.Eligible, nullable(profile.ApprovedBy),
		profile.ApprovedAt, profile.MeasuredAt, nullable(profile.SourceBenchmarkRunID))
	return err
}

func (r *RuntimeProfileRepository) ListEligibleRuntimeProfiles(ctx context.Context, filter deployment.RuntimeProfileFilter) ([]deployment.RuntimeProfile, error) {
	rows, err := r.store.pool.Query(ctx, `
		SELECT id, model_uri, model_revision, backend, backend_version, runtime_image_digest,
		       gpu_sku, gpu_count, precision, quantization, tensor_parallelism,
		       max_context_bucket, driver_cuda_fingerprint, scenario_digest, metrics,
		       cost_per_successful_request, eligible, COALESCE(approved_by,''),
		       approved_at, measured_at, COALESCE(source_benchmark_run_id,'')
		FROM runtime_profiles
		WHERE eligible=true AND approved_at IS NOT NULL
		  AND ($1='' OR model_uri=$1) AND model_revision=$2 AND gpu_sku=$3 AND gpu_count=$4
		  AND precision=$5 AND quantization=$6 AND tensor_parallelism=$7
		  AND max_context_bucket=$8
		ORDER BY cost_per_successful_request, measured_at DESC`,
		filter.ModelURI, filter.ModelRevision, filter.GPUSKU, filter.GPUCount, filter.Precision,
		filter.Quantization, filter.TensorParallelism, filter.MaxContextBucket)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profiles := make([]deployment.RuntimeProfile, 0)
	for rows.Next() {
		var profile deployment.RuntimeProfile
		if err := rows.Scan(&profile.ID, &profile.ModelURI, &profile.ModelRevision, &profile.Backend,
			&profile.BackendVersion, &profile.RuntimeImageDigest, &profile.GPUSKU,
			&profile.GPUCount, &profile.Precision, &profile.Quantization,
			&profile.TensorParallelism, &profile.MaxContextBucket,
			&profile.DriverCUDAFingerprint, &profile.ScenarioDigest, &profile.Metrics,
			&profile.CostPerSuccessfulRequest, &profile.Eligible, &profile.ApprovedBy,
			&profile.ApprovedAt, &profile.MeasuredAt, &profile.SourceBenchmarkRunID); err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
	}
	return profiles, rows.Err()
}

// SetRuntimeProfileEligibility is the only mutable operation permitted on an
// otherwise append-only measured profile. Revocation clears approval metadata
// so selection can only consume explicitly approved evidence.
func (r *RuntimeProfileRepository) SetRuntimeProfileEligibility(ctx context.Context, id string, eligible bool, operator string, at time.Time) error {
	var commandErr error
	var affected int64
	if eligible {
		command, err := r.store.pool.Exec(ctx, `
			UPDATE runtime_profiles SET eligible=true, approved_by=$2, approved_at=$3
			WHERE id=$1`, id, operator, at)
		commandErr = err
		if err == nil {
			affected = command.RowsAffected()
		}
	} else {
		command, err := r.store.pool.Exec(ctx, `
			UPDATE runtime_profiles SET eligible=false, approved_by=NULL, approved_at=NULL
			WHERE id=$1`, id)
		commandErr = err
		if err == nil {
			affected = command.RowsAffected()
		}
	}
	if commandErr != nil {
		return commandErr
	}
	if affected == 0 {
		return ErrRuntimeProfileNotFound
	}
	return nil
}
