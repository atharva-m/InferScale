# Development setup

## Prerequisites

- Windows 11 with WSL2 Ubuntu, or native Ubuntu
- Docker with WSL integration
- Go version from `versions.lock.yaml`
- Python 3.10 or newer
- `kubectl`, `k3d`, and `curl`
- NVIDIA driver plus `nvidia-smi` only for the direct baseline

Copy `.env.example` to `.env` and replace every model/runtime value marked unresolved before running GPU work. A model revision must be a 40-character immutable commit SHA; a branch such as `main` is rejected.

## Local non-GPU stack

```bash
make dev-up
make dev-status
make dev-down
```

`dev-up` creates or restarts `k3d-inferscale-dev`, installs development candidate Gateway/Inference/KEDA dependencies, and starts PostgreSQL, Valkey, Prometheus, Grafana, Tempo, and the OpenTelemetry Collector. It builds and imports the three control-plane images, migration image, benchmark runner, and `ghcr.io/inferscale/fake-runtime:0.1.0-dev` before applying the overlay. It then recreates and waits for the idempotent migration Job and verifies all three control-plane rollouts. Their init gates prevent application startup before the schema is present. `dev-down` stops rather than deletes the k3d cluster; host benchmark results and cluster volumes remain.

New clusters use the `rancher/k3s:v1.36.2-k3s1` image recorded by the lock file
and disable bundled Traefik so Envoy Gateway exclusively owns the local HTTP and
HTTPS entrypoints. Override `INFERSCALE_K3S_IMAGE` only for an intentional
compatibility test. `INFERSCALE_DEV_SKIP_BENCHMARK_IMAGE=true` skips the costly
benchmark-runner build when no benchmark Job will be submitted; the live
control-plane CI gate uses that bounded mode.

The local overlay explicitly sets `INFERSCALE_FAKE_RUNTIME=true` and supplies the fake image through the normal `INFERSCALE_VLLM_IMAGE` setting. The controller therefore replaces only its vLLM adapter implementation: API requests and `InferenceDeployment` objects still say `backend: vllm`, and no development-only backend appears in the public contract. The rendered worker is a CPU-only, deterministic OpenAI-compatible server. Its adapter reports that model caching is not required, so reconciliation creates no prefetch Job or host cache mount and reports cache state `NotRequired`. The worker retains the canonical `<revision>-vllm` Kubernetes name so update, retirement, drift repair, and deletion exercise the production controller paths.

This fake path validates control-plane plumbing, native JSON/SSE shape, cancellation propagation, and route construction only. It is excluded from release images and benchmark profiles and must never be used for performance, cache, GPU, vLLM, or TensorRT-LLM claims. A runtime worker is created only after a tenant deployment is accepted through the management API.

Run the complete live local smoke after the stack is installed:

```bash
make e2e-local-live
```

The target is intentionally separate from `make e2e-local`, which is the fast
in-process deterministic suite. The live target uses real PostgreSQL, Valkey,
Kubernetes SSA, controller reconciliation, and one Gateway/admission/EPP request
to the CPU fake worker. It also checks controller restart idempotency and Valkey
fail-closed recovery. It is not Gateway conformance or GPU evidence; see
`docs/testing.md` for the exact boundary and diagnostic environment variables.

The dependency versions, manifest checksums, and workload image digests are a
resolved candidate matrix. `scripts/install-platform-dependencies.sh` verifies
all seven downloaded artifacts before applying any of them and fails if an
upstream manifest contains a workload image that was not rewritten to an
immutable digest. The Envoy data plane is pinned separately in the checked-in
`EnvoyProxy`, because it is generated after the controller is installed. These
integrity locks are not yet a compatibility claim: M0 must run the conformance
suite recorded in `versions.lock.yaml` before remotely reachable or publishable
use.

Local endpoints are exposed with explicit port forwards:

```bash
kubectl -n inferscale-monitoring port-forward svc/grafana 3000:3000
kubectl -n inferscale-monitoring port-forward svc/prometheus 9090:9090
kubectl -n inferscale-monitoring port-forward svc/tempo 3200:3200
```

Anonymous Grafana is a local-only convenience. Do not expose the local overlay to an untrusted network.

To inspect the configured boundary and any reconciled workers:

```bash
kubectl -n inferscale-system get configmap inferscale-config \
  -o jsonpath='{.data.INFERSCALE_FAKE_RUNTIME}{"\n"}'
kubectl get deployments -A -l app.kubernetes.io/component=model-server \
  -o custom-columns=NAMESPACE:.metadata.namespace,NAME:.metadata.name,IMAGE:.spec.template.spec.containers[0].image
kubectl get jobs -A -l app.kubernetes.io/component=model-prefetch
```

The first command must print `true`, fake workers must use the imported `fake-runtime` image, and a fake-backed revision must not have a model-prefetch Job.

## Direct vLLM baseline

Set `INFERSCALE_LOCAL_MODEL_REVISION` to the immutable revision corresponding to `INFERSCALE_LOCAL_MODEL`, then:

```bash
make baseline-up
make baseline-run
make baseline-down
```

The direct baseline bypasses Kubernetes and InferScale. It validates model/runtime correctness and the streaming benchmark parser on the RTX 4070; its results are permanently non-publishable. The Docker volume containing the Hugging Face cache is retained by `baseline-down`.

## Python tooling

```bash
python3 -m venv .venv
. .venv/bin/activate
pip install -e 'benchmarks/runner[test,analysis]'
pip install -e 'modelcache[test]'
python -m pytest benchmarks/runner/tests modelcache/tests
```

The runner writes atomic JSON artifacts under `benchmarks/results/`. Do not commit real prompts, generated text, API keys, or provider credentials.

## Common failures

- A stopped k3d cluster is restarted automatically; a conflicting Docker port must be freed manually.
- If external manifests fail to download or checksum validation fails, stop and
  verify the release artifact independently. Do not replace a pinned URL with a
  floating `main` manifest or update a checksum from an untrusted download.
- A Gateway/Inference/EPP API incompatibility must fail the recorded conformance gate. Do not bypass that failure by changing the locked CRD version or endpoint-picker image independently.
- A baseline model OOM is a local-development constraint. Reduce context/concurrency or use Qwen3-0.6B; do not redesign production architecture around the laptop.
