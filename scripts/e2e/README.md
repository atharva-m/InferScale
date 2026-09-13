# Local live checks

`live-local-stack.sh` runs the existing CPU fake-runtime gate.
`live-local-gpu.sh` runs a separate, non-publishable functional check against an
already installed InferScale cluster using one exclusive local RTX 4070.

The GPU cluster must expose `nvidia.com/gpu: 1`, use an NVIDIA-capable default
container runtime, and label its GPU node `inferscale.io/gpu-sku=RTX_4070`.
Configure `INFERSCALE_FAKE_RUNTIME=false`, the matching GPU node selector, and
real images built from `runtime/vllm/` and `modelcache/`. The node-local model
cache must be writable by UID/GID 65532. Provision a test tenant with at least
one deployment/GPU, two concurrent requests and four queued requests; a rollout
uses the platform's normal extra candidate quota. Port-forward the local
Gateway to a loopback HTTP(S) origin.

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
at the checked-in immutable model revision, with BF16, TP1, context 2048 and one
replica. It verifies real cache/runtime readiness, GPU visibility, the running
process's explicit prefix-caching disable flag, authenticated JSON and SSE,
controller restart, stable inference while a candidate awaits the occupied GPU,
superseded-candidate cleanup, and operator abort.

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
stable/candidate canaries. This harness also does not establish performance,
full Gateway conformance, scale-to-zero, or telemetry/billing correctness.
The latter need separate focused checks after the real inference path is healthy.

Run the harness's offline validation with:

```bash
bash tests/shell/live_local_gpu_test.sh
```
