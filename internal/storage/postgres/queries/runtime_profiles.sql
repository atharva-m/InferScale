-- name: ListEligibleRuntimeProfiles :many
SELECT id, model_revision, backend, backend_version, runtime_image_digest,
       gpu_sku, gpu_count, precision, quantization, tensor_parallelism,
       max_context_bucket, driver_cuda_fingerprint, scenario_digest, metrics,
       cost_per_successful_request, eligible, approved_by, approved_at,
       measured_at, source_benchmark_run_id
FROM runtime_profiles
WHERE eligible=true AND approved_at IS NOT NULL
  AND model_revision=$1 AND gpu_sku=$2 AND gpu_count=$3
  AND precision=$4 AND quantization=$5 AND tensor_parallelism=$6
  AND max_context_bucket=$7
ORDER BY cost_per_successful_request, measured_at DESC;
