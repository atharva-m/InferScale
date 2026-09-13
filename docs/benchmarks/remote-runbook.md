# Remote RTX 5090 runbook

## Prepare one host

```bash
cd infra/terraform/gpu-host
cp terraform.tfvars.example terraform.tfvars
terraform init && terraform plan && terraform apply
```

Copy `/etc/rancher/k3s/k3s.yaml` locally, replace its loopback server address with the host address, and protect it with mode `0600`. Set explicit verified NVIDIA chart versions, install the device plugin/DCGM exporter, and run `scripts/verify-remote-gpu.sh <1|2|4>`.

Create a TLS Secret and an externally managed `inferscale-system/inferscale-storage` Secret containing `postgres_user`, `postgres_password`, `postgres_database`, `database_url`, `valkey_addr`, a random `benchmark_callback_signing_key` of at least 32 bytes, and immutable registry references pinned with `@sha256:` for `benchmark_runner_image`, `runtime_vllm_image`, `runtime_trtllm_image`, and `modelcache_image`. Remote overlays intentionally contain no credential or mutable image fallback. Install the pinned dependency matrix, then apply the matching overlay:

Configure the API scheduler with operator-owned values
`INFERSCALE_BENCHMARK_PROVIDER`,
`INFERSCALE_BENCHMARK_INFERENCE_BASE_URL`,
`INFERSCALE_BENCHMARK_PROMETHEUS_URL`,
`INFERSCALE_BENCHMARK_DRIVER_VERSION`, and
`INFERSCALE_BENCHMARK_CUDA_VERSION`. The scheduler combines those values with
the frozen deployment revision and digest-pinned runner image, stores the
execution snapshot, and only then creates a Job. Scenario JSON cannot override
the target, model, backend, runtime version/image, GPU shape, driver, or CUDA
identity.

These five values are provenance and service locations, not credentials, so the
release renderer stores them in `inferscale-system/inferscale-config`. The
callback signing key and immutable runner/runtime image references remain in the
externally managed `inferscale-storage` Secret. The renderer rejects missing,
placeholder, malformed, credential-bearing, or plaintext inference values, and
the API repeats that validation at startup in a remote environment.

Before enabling `INFERSCALE_FEATURE_BACKEND_AUTO`, set
`INFERSCALE_DRIVER_CUDA_FINGERPRINT` to the SHA-256 fingerprint emitted by the
remote benchmark provenance, set `INFERSCALE_SELECTION_SCENARIO_DIGEST` to the
approved standardized scenario digest, and configure
`INFERSCALE_VLLM_VERSION`/`INFERSCALE_TRTLLM_VERSION` alongside their exact
`@sha256:` runtime images. The controller refuses backend auto-selection when
any applicable identity is missing or mutable; these values are never treated
as wildcards.

```bash
scripts/create-tls-secret.sh /secure/path/tls.crt /secure/path/tls.key
scripts/install-platform-dependencies.sh
export API_IMAGE=registry.example/inferscale/api@sha256:<digest>
export CONTROLLER_IMAGE=registry.example/inferscale/controller@sha256:<digest>
export ADMISSION_IMAGE=registry.example/inferscale/admission@sha256:<digest>
export MIGRATE_IMAGE=registry.example/inferscale/migrate@sha256:<digest>
export PUBLIC_BASE_URL=https://inference.example.com
export INFERSCALE_BENCHMARK_PROVIDER=vast
export INFERSCALE_BENCHMARK_INFERENCE_BASE_URL="${PUBLIC_BASE_URL}"
export INFERSCALE_BENCHMARK_PROMETHEUS_URL=http://prometheus.inferscale-monitoring.svc.cluster.local:9090
export INFERSCALE_BENCHMARK_DRIVER_VERSION=<verified-nvidia-driver-version>
export INFERSCALE_BENCHMARK_CUDA_VERSION=<verified-cuda-compatibility-version>
scripts/render-remote-release.sh vast-1x5090 | \
  kubectl apply --server-side --field-manager=inferscale-bootstrap -f -
```

The checked-in remote Kustomize overlay is a fail-closed render template: its
four control-plane images point at the non-routable `registry.invalid` domain.
A remote release must go through `scripts/render-remote-release.sh`, which
refuses mutable references and replaces every API, controller, admission, and
migration image with an explicit digest. Do not apply the remote or Vast
overlay directly for a release.

Do not continue while `versions.lock.yaml`, `models.lock.yaml`, an image digest, rental price, or conformance evidence is unresolved.

Build and publish `benchmarks/runner/Dockerfile` as the digest-pinned image selected by `INFERSCALE_BENCHMARK_IMAGE`. Set `INFERSCALE_BENCHMARK_RESULTS_PVC` to a pre-created tenant-accessible results claim when raw JSON artifacts must survive the retained Job; otherwise only normalized measurements and provenance persist in PostgreSQL. Scheduled Jobs receive a run-scoped signed token; the admission service accepts it only for the running benchmark's tenant and deployment, and the runner uses the same token for its terminal callback. Never place a tenant API key in a scenario file.

## Execute

```bash
scripts/verify-remote-gpu.sh 1
PYTHONPATH=benchmarks/runner python -m inferscale_bench validate benchmarks/scenarios/qwen3-8b-vllm-1gpu.yaml
INFERSCALE_API_KEY=... INFERSCALE_DEPLOYMENT_ID=... PYTHONPATH=benchmarks/runner python -m inferscale_bench run \
  --scenario benchmarks/scenarios/qwen3-8b-vllm-1gpu.yaml \
  --prometheus-url http://127.0.0.1:9090 \
  --provider vast \
  --gpu-hourly-price <actual-price-at-run-time> \
  --driver-version <nvidia-driver-version> \
  --cuda-version <cuda-compatibility-version> \
  --container-image-digest sha256:<verified-runtime-digest> \
  --runner-image-digest sha256:<verified-runner-digest> \
  --deployment-namespace <tenant-namespace> \
  --runtime-pod-prefix <revision-runtime-pod-prefix> \
  --epp-service <revision-epp-service>
```

For API-scheduled runs, the submitted configuration selects the standardized
workload, SLO, and experiment grouping. Reusable selection runs require a
stable-only Ready deployment, `cache_state: process-warm`, and the deployment's
actual routing policy. The server verifies those claims and aborts authorization
if the stable revision changes or a candidate appears during measurement.
Provider, GPU-hour price, target/deployment identity, driver/CUDA, runtime
version/image, and runner image are server-authored. Missing or mutable evidence
fails the run instead of producing a reusable compatibility profile with
fabricated provenance.

Use `scripts/clear-model-cache.sh --cache-root /var/lib/inferscale/models --yes` only for an intentional remote-cold run after all runtime pods using the entry are stopped. Cache-warm keeps verified weights but scales the process down. Process-warm keeps the worker ready and receives unmeasured warm-up requests.

Run 1 GPU first, then 2, then 4. For each host compare TP=1 replica scaling against TP=2/4 using identical workloads. Never present cross-node results as tensor parallelism.

## Close out

Copy result JSON off the rental host, verify provenance and no sensitive content, render measured reports, then terminate rented capacity through the provider. Retain raw JSON and scenario files with the report.
