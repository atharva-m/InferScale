# Local live checks

`live-local-stack.sh` runs the existing CPU fake-runtime gate.
`live-local-gpu.sh` runs a separate, non-publishable functional check against an
already installed InferScale cluster. By default it uses one exclusive local
RTX 4070; environment overrides allow the same bounded check to exercise a
single-node TP2/TP4 worker, prefix caching, or a small autoscaling burst.

The GPU cluster must expose one physical GPU node with the requested
`nvidia.com/gpu` capacity, use an NVIDIA-capable default container runtime, and
label that node with `inferscale.io/gpu-sku` matching the selected GPU type.
Configure `INFERSCALE_FAKE_RUNTIME=false`, the matching GPU node selector, and
real images built from `runtime/vllm/` and `modelcache/`. The node-local model
cache must be writable by UID/GID 65532. Provision a test tenant with at least
one deployment/GPU, two concurrent requests and four queued requests; a rollout
uses the platform's normal extra candidate quota. Port-forward the local Gateway
to a loopback HTTP(S) origin.

Put its credential in a private file, supplied through an editor or provisioning
tool without putting the secret on the command line:

```json
{"base_url":"http://127.0.0.1:8080","api_key":"<tenant API key>"}
```

Then run:

```bash
chmod 600 /tmp/inferscale-gpu-auth.json
INFERSCALE_GPU_KUBE_CONTEXT=inferscale-local-gpu \
  INFERSCALE_GPU_AUTH_FILE=/tmp/inferscale-gpu-auth.json \
  scripts/e2e/live-local-gpu.sh
```

The context is required and passed to every `kubectl` call; the script does not
change the current context. It creates a uniquely named Qwen3-0.6B deployment
at the checked-in immutable model revision, with BF16, context 2048, and the
configured worker GPU count, tensor-parallel degree, cache mode, routing policy,
and replica bounds. It verifies real cache/runtime readiness, GPU visibility,
the running process's explicit prefix-caching and tensor-parallel flags,
authenticated JSON and SSE, controller restart, stable inference while a
candidate awaits the occupied GPU, superseded-candidate cleanup, and operator
abort. Prefix-cache mode can optionally require positive vLLM hit/query and KV
usage metrics after repeated identical requests. Autoscaling mode sends a
bounded concurrent burst and requires at least a second available worker.

The SSE request enables `stream_options.include_usage`. Its response is parsed
incrementally with a 180-second total deadline, a 4 MiB response limit and a
64 KiB event limit. Generated chunks and positive, internally consistent prompt
and completion token usage must precede `[DONE]`; subsequent data fails the
check. The artifact contains event counts and token totals, without generated
text. A usage-only final frame may omit its model identity.

| Optional variable | Default | Effect |
| --- | --- | --- |
| `INFERSCALE_GPU_WAIT_SECONDS` | `1200` | Maximum polling time per convergence check; range 60–3600. |
| `INFERSCALE_GPU_CONTROLLER_RESTART` | `true` | Restart the selected cluster's controller and verify recovery. |
| `INFERSCALE_GPU_CANDIDATES` | `true` | Exercise pending-candidate supersession and abort. |
| `INFERSCALE_GPU_TYPE` | `RTX_4070` | GPU SKU label and expected `nvidia-smi` model substring. |
| `INFERSCALE_GPU_COUNT` | `1` | GPUs allocated to one worker; v1 allows 1, 2, or 4. |
| `INFERSCALE_GPU_TENSOR_PARALLELISM` | worker GPU count | Tensor-parallel degree; must equal `INFERSCALE_GPU_COUNT`. |
| `INFERSCALE_GPU_NODE_GPU_COUNT` | worker GPU count | Total allocatable GPUs on the one physical GPU node. Set to 4 or 8 when testing replicas. |
| `INFERSCALE_GPU_PREFIX_CACHING` | `false` | Request prefix caching on or off and verify the worker environment/process flag. |
| `INFERSCALE_GPU_ROUTING_POLICY` | `load-aware` | Routing policy; `prefix-aware` requires prefix caching. |
| `INFERSCALE_GPU_CACHE_METRICS` | `false` | After repeated requests, require positive vLLM prefix-cache counters and bounded KV usage. |
| `INFERSCALE_GPU_AUTOSCALING` | `false` | Run a bounded burst and require at least two available workers. Requires enough node GPUs and disables candidate checks by default. |
| `INFERSCALE_GPU_SCALE_TO_ZERO` | `false` | Wait for normal KEDA idle scale-down, then require one queued request to activate a new worker. Requires the cluster feature gate; works with one GPU. |
| `INFERSCALE_GPU_MIN_REPLICAS` | `1` (`0` in scale-to-zero mode) | Minimum worker replicas. Zero is accepted only with the scale-to-zero check enabled. |
| `INFERSCALE_GPU_MAX_REPLICAS` | `1` | Maximum worker replicas; must exceed min replicas in autoscaling mode. |
| `INFERSCALE_GPU_KEEP_DEPLOYMENT` | `false` | Retain the created deployment for inspection instead of deleting it. |
| `INFERSCALE_GPU_RESULTS_DIR` | `benchmarks/results` | Directory for the redacted JSON pass/fail artifact. |
| `KUBECTL` | `kubectl` | Path to the Kubernetes client. |

The harness copies credentials into a mode-0600 temporary JSON file, authenticates
inside Python, disables HTTP redirects, and never writes model output or bearer
headers to the result. It deletes only the deployment it creates, including on
failure; it retains the tenant, model cache, and caller's original credential file.
Use the retention option to inspect failed workloads before cleanup. Convergence
failures produce a partial result identifying the last check reached.

One exclusive GPU cannot prove multi-GPU scaling or healthy simultaneous
stable/candidate canaries. A TP2/TP4 run verifies placement, GPU visibility,
and inference only; it does not measure tensor-parallel speedup or communication
cost. Autoscaling mode verifies a bounded 1-to-N scale-up when capacity exists.
The separate scale-to-zero mode observes actual worker removal through the
normal KEDA cooldown, then sends exactly one inference request with the
900-second cold-start deadline. It requires a new Pod identity and a successful
response, without changing replica counts or injecting scaling metrics. The
result records idle-wait and activation duration as cache-warm functional
evidence. Allow up to 20 minutes for the normal cooldown and model restart.
This harness also does not establish
performance, full Gateway conformance, or telemetry/billing correctness. Those
need separate focused checks after the real inference path is healthy.

Useful bounded profiles on a single physical host are:

```bash
# One GPU: existing cluster must enable INFERSCALE_FEATURE_SCALE_TO_ZERO.
INFERSCALE_GPU_SCALE_TO_ZERO=true \
INFERSCALE_GPU_CONTROLLER_RESTART=false \
INFERSCALE_GPU_WAIT_SECONDS=1200 \
  scripts/e2e/live-local-gpu.sh

# Two GPUs in one worker, prefix cache on, no candidate exercise.
INFERSCALE_GPU_TYPE=RTX_5090 \
INFERSCALE_GPU_COUNT=2 \
INFERSCALE_GPU_TENSOR_PARALLELISM=2 \
INFERSCALE_GPU_NODE_GPU_COUNT=2 \
INFERSCALE_GPU_PREFIX_CACHING=true \
INFERSCALE_GPU_ROUTING_POLICY=prefix-aware \
INFERSCALE_GPU_CACHE_METRICS=true \
INFERSCALE_GPU_CANDIDATES=false \
  scripts/e2e/live-local-gpu.sh

# Two TP1 workers on a four-GPU host, with a short burst-driven scale-up.
INFERSCALE_GPU_TYPE=RTX_5090 \
INFERSCALE_GPU_COUNT=1 \
INFERSCALE_GPU_TENSOR_PARALLELISM=1 \
INFERSCALE_GPU_NODE_GPU_COUNT=4 \
INFERSCALE_GPU_AUTOSCALING=true \
INFERSCALE_GPU_MAX_REPLICAS=2 \
  scripts/e2e/live-local-gpu.sh
```

The profiles are intentionally opt-in. Do not set the 5090 values on a host
that does not expose that SKU and count; the preflight check fails closed.

Run the harness's offline validation with:

```bash
bash tests/shell/live_local_gpu_test.sh
```

## Native trace continuity and privacy

`live-trace-conformance.sh` sends one non-streaming inference request to an
existing stable, tracing-enabled real-vLLM deployment. Prepare separate
loopback Gateway and Tempo forwards before running it. This checker does not
create deployments, change Kubernetes resources, or start port-forwards.
Preflight requires the observed backend to be `vllm` and model-cache weights
to be `Warm`; the local fake adapter's `NotRequired` cache fails this check.
Use a private JSON credential file with `base_url` and `api_key`, as above;
`deployment_id` may be supplied in that file or through the environment.

```bash
INFERSCALE_TRACE_AUTH_FILE=/tmp/inferscale-test-auth.json \
INFERSCALE_TRACE_DEPLOYMENT_ID=<existing-deployment-uuid> \
INFERSCALE_TRACE_TEMPO_URL=http://127.0.0.1:3200 \
  scripts/e2e/live-trace-conformance.sh
```

The checker injects a sampled W3C caller context, then queries Tempo for that
exact trace. Gateway, admission, EPP, and runtime spans must all retain the
same trace ID and an unbroken recorded parent path to the caller span ID.
The synthetic caller itself is not exported. Exact `service.name` defaults
are `inferscale.inferscale-gateway`, `inferscale-admission`, `inferscale-epp`,
and `inferscale-vllm`; deployments with different configured service identities
can set `INFERSCALE_TRACE_GATEWAY_SERVICE`, `INFERSCALE_TRACE_ADMISSION_SERVICE`,
`INFERSCALE_TRACE_EPP_SERVICE`, and `INFERSCALE_TRACE_RUNTIME_SERVICE`.
Four distinct service identities are required. Native EPP images must support
OTLP export and actually attach the configured service name to their resources;
an environment variable alone does not prove this.

The privacy scan covers the complete returned trace, including resource and
scope attributes, span events, links, status messages, nested values, and
base64 OTLP bytes values. It rejects content-bearing attribute names, prompt
and completion markers, the credential, known response fragments, and bearer
values. Numeric token counts and body sizes remain permitted. It reports only
fixed finding categories and structural node numbers, never raw attributes,
prompt text, completion text, HTTP error bodies, or credentials. Auth files
must be regular private files (0600 or stricter); symlinks are rejected.
Both HTTP destinations must be loopback origins (`localhost` is pinned to
`127.0.0.1`). Environment proxies and redirects are disabled, and Gateway
authorization is never sent to Tempo.

| Optional variable | Default | Effect |
| --- | --- | --- |
| `INFERSCALE_TRACE_REQUEST_TIMEOUT_SECONDS` | `300` | Inference timeout; range 30–900 seconds. |
| `INFERSCALE_TRACE_WAIT_SECONDS` | `90` | Trace export polling deadline; range 20–300 seconds. |
| `INFERSCALE_TRACE_SETTLE_SECONDS` | `10` | Minimum time all components must remain correlated and pass privacy checks; range 5–30 seconds. |
| `INFERSCALE_TRACE_ARTIFACT_DIR` | `benchmarks/artifacts/trace-conformance` | Destination for a private, redacted JSON summary. |

Summary artifacts record the lock-file digest and retain `release_signoff:false`.
This is bounded evidence for one successful non-streaming request and the
exports visible during the settle window. It does not establish streaming or
error-path privacy, later exports, other requests or logs, arbitrary content
transformations, or exact pinned-image release conformance. The full release
gate remains pending until its required live evidence is collected.

Run the stdlib tests without network, Kubernetes, containers, or a GPU:

```bash
bash tests/shell/trace_conformance_test.sh
```

## CPU-only native Gateway routing qualification

`live-gateway-conformance.sh` runs isolated native Gateway experiments on an
already configured local cluster. It needs an existing authorized deployment
with a stable revision and no candidate, a mode-0600 credential JSON file in
the same format as the GPU harness, and a locally available fake-runtime image
built from the current `cmd/fakeruntime` code. The source deployment may use a
real GPU; the additional test workers use CPU only.

```bash
INFERSCALE_CONFORMANCE_KUBE_CONTEXT=default \
INFERSCALE_CONFORMANCE_AUTH_FILE=/tmp/inferscale-test-auth.json \
INFERSCALE_CONFORMANCE_DEPLOYMENT_ID=<existing-deployment-uuid> \
INFERSCALE_CONFORMANCE_FAKE_IMAGE=ghcr.io/inferscale/fake-runtime:conformance-current \
  scripts/e2e/live-gateway-conformance.sh
```

The harness copies the source revision's native EPP, InferencePool, objective,
RBAC, and network-policy shapes into two uniquely named test revisions. It uses
a bounded native load-aware picker configuration and creates one small fake
worker per revision. A separate HTTPRoute matches the existing public UUID
path **and two test headers**. Those headers select only the test route; normal
tenant authorization still validates the key, deployment, request, and rate
limit. The source's managed HTTPRoute and workload stay under their existing
controller. The harness sets no GPU requests and creates no model-cache jobs.

The two additional EPPs and two fake workers have a combined maximum of 1.5 CPU
cores and 1.75 GiB memory. Unique names and a separate ownership label protect
the source resources from test cleanup and protect the test resources from
deployment retirement. Only objects created by this invocation are removed.
The Kubernetes identity needs read access to source/Gateway resources and
create/patch/delete plus port-forward access to the tenant test resources.

The checks cover:

- Native weighted InferencePool references at 0%, 5%, 25%, 50%, and 100%
  candidate traffic, response identity, per-worker request counts, and the
  selected revision's objective header.
- Native 25% Service mirroring, with candidate request counters increasing and
  every client response remaining from stable.
- Missing/invalid key rejection before runtime; spoofed scheduling metadata
  replaced by the authenticated values; bearer stripping before runtime.
- Incremental native SSE with usage and `[DONE]`, client cancellation observed
  by the worker, bounded EPP queue rejection with HTTP 429, and recovery.

Fake diagnostics require `INFERSCALE_FAKE_TEST_CONTROLS=true` plus a short
`INFERSCALE_FAKE_TEST_IDENTITY`; the harness sets these only on its fake
workers. These endpoints are absent by default and are reached through private
loopback port-forwards. They retain request counters and selected scheduling
headers, never bearer values, messages, generated text, or arbitrary headers.
The image remains excluded from release image builds and public backends.

| Variable | Default | Effect |
| --- | --- | --- |
| `INFERSCALE_CONFORMANCE_REQUESTS` | `100` | Requests per weight and mirror experiment; range 20–1000. Set 1000 for the required qualification sample size. |
| `INFERSCALE_CONFORMANCE_WAIT_SECONDS` | `180` | Per-convergence deadline; range 30–600 seconds. |
| `INFERSCALE_CONFORMANCE_INTERVAL_MS` | `0` | Pause between sequential samples; range 0–1000 milliseconds. |
| `INFERSCALE_CONFORMANCE_RESULTS_DIR` | `benchmarks/results` | Destination for the redacted JSON artifact. |
| `KUBECTL` | `kubectl` | Kubernetes client path. Every call receives the explicit context. |

Use an isolated tenant with enough request-rate budget for six sample groups
plus the protocol checks. A tenant rate-limit rejection fails the experiment;
it cannot serve as proof of EPP queue behavior. The eight-request backpressure
burst has a separate fixed bound. Statistical assertions record the exact
count tolerance: four binomial standard deviations with a minimum of three
requests, exact routing at 0%/100%, and at least one observation of each backend
for intermediate weights. A short development run can therefore fail through
sampling noise, particularly at 5%; it is not the release-size sample.

Artifacts include sample counts/tolerances, current route conditions, deployed
fixture and Gateway image IDs, the lock-file hash, individual passed checks,
the failing stage if applicable, and cleanup status. These are **partial live
evidence**, marked `release_signoff: false`. They do not establish GPU runtime
performance, automatic controller rollout promotion, authorization/Valkey
outages, revocation propagation, distributed trace export/privacy, or exact
pinned-image release sign-off. The script never edits the compatibility lock
or produces a falsely completed release-gate evidence file.

Run the offline fixture/protocol/safety checks with:

```bash
bash tests/shell/gateway_conformance_test.sh
```
