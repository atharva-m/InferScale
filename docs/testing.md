# Test strategy

`make test` is hardware-independent. CI renders every Kustomize overlay,
validates OpenAPI/CRD/source-view contracts, regenerates checked-in CRD/deepcopy/RBAC
artifacts, runs SQL migrations on empty and v000001 upgrade fixtures, executes
shell image/entrypoint contracts, and verifies observability JSON/YAML/rules.
The empty-database migration job starts two migrators concurrently, exercising
the PostgreSQL session advisory lock before the idempotent replay check.
The migration job also runs repository transaction regressions against isolated
schemas: promotion between a service read and its update, large GPU allocations,
and public outbox retry/completion state. To run these locally, set
`INFERSCALE_TEST_DATABASE_URL` to a PostgreSQL database whose role can create
schemas, then run `go test -race ./internal/storage/postgres`. The ordinary Go
suite skips these integration cases when that variable is absent.
The local fake-runtime contract additionally proves that only the `local-wsl`
overlay enables the adapter override, its image is built/imported by `dev-up`,
and neither the public API/CRD nor remote release images expose it.

| Tier | Required coverage |
|---|---|
| Unit | Spec defaults/validation/hash, serving-change classification, capabilities, backend selection, rollout state machine/gates, quota leases, safe cache keys/checksums, runtime argv, cost/statistics/SSE parsing |
| PostgreSQL/API | Migrations/constraints, tenant isolation, duplicate idempotency, concurrent revision/usage updates, auth/404 semantics, pagination, ETag conflicts, asynchronous delete |
| Controller/envtest | First-reconcile finalization, create/update/delete, child drift repair, status conflicts, restart idempotency, failed prefetch/readiness, candidate preservation, tenant selectors |
| Local k3d | Repeated up/status/down, storage/telemetry outages, API/controller restart with pending operations, mock runtime routing |
| Remote GPU | Streaming vLLM, multiple workers, prefix workload, two-tenant fairness/backpressure, three cache states, `1->N`, `0->1->N`, bad-candidate rollback, TensorRT parity/auto selection |
| Security/fault | Cross-tenant denial, spoofed headers, unsafe URI/path/image, read-only cache, NetworkPolicy/RBAC negatives, worker OOM/crash, disk full, router/Valkey/Prometheus outage |

Useful direct checks:

```bash
make verify-generated
make test
make e2e-local
make e2e-local-live
make conformance-static
```

`make e2e-local` is a deterministic in-process vertical slice. It sends an
authenticated deployment create through the public HTTP handler, verifies the
transaction-shaped repository queued an outbox event, projects that event into
a CRD using a fake Kubernetes client, and invokes the controller's first
reconciliation to persist its finalizer. A second case proves a failed
Kubernetes apply remains retryable instead of completing the outbox event. It
does not claim PostgreSQL transaction, apiserver SSA, Gateway, or GPU coverage.

`make e2e-local-live` is the separate deployable-stack gate. By default it runs
`scripts/dev/up.sh`, provisions a tenant namespace and one API key with
`inferscalectl` through a host-side PostgreSQL port-forward, and creates a
deployment through the public management route. It then waits for the real
PostgreSQL outbox, Kubernetes apiserver server-side apply, controller, CPU fake
worker, EPP, InferencePool, and HTTPRoute to converge. The gate requires the CR
and PostgreSQL status projection to be `Ready`, sends one authenticated chat
request through Gateway/admission/EPP, restarts the controller without changing
the stable revision, and proves that a Valkey outage fails new inference closed
with `503` before recovery. Public liveness is checked through `/healthz`;
dependency-aware `/readyz` is checked only through an internal API Service
port-forward and remains absent from the public Gateway routes. CI runs this
target in an ephemeral, version-pinned k3d cluster; local runs retain their
development cluster and historical test tenant records for inspection.

This is a single-path smoke test. It does not establish Gateway/llm-d
conformance, fractional mirroring, traffic-weight accuracy, full-duplex
processing, GPU runtime behavior, cache performance, autoscaling, or publishable
benchmark evidence. Those remain in the explicit conformance and remote GPU
tiers. Set `INFERSCALE_E2E_BOOTSTRAP=false` to reuse an already healthy local
cluster. The controller-restart and Valkey-outage checks can be disabled
individually for diagnosis, but CI leaves both enabled.

The controller unit suite separately reconciles a valid vLLM CR through the
local fake adapter far enough to render the worker and routing prerequisites.
It asserts that no prefetch/engine Job or GPU request is created, the canonical
vLLM workload identity is retained, and status says cache `NotRequired`. This
is an in-memory test, not evidence that a live k3d Gateway/llm-d data path has
passed conformance.

`make conformance-static` inspects the rendered Gateway/llm-d contracts. It
cannot prove the pinned implementations support Extended features. The
mandatory live procedure and evidence format are in
`tests/conformance/gateway/README.md`. `make release-check` intentionally fails
while `versions.lock.yaml` says `pending`, or when evidence is missing, stale,
incomplete, or belongs to another lock-file digest.

The live evidence also has to show that a caller trace remains correlated
through Gateway, admission, EPP, and the selected runtime while prompts,
completions, bearer values, and request bodies remain absent from exported
spans. Unit tests cover the body-free API/admission instrumentation, and static
tests cover the Envoy and vLLM configuration, but neither substitutes for this
data-path check.

`scripts/e2e/live-trace-conformance.sh` performs this bounded check for one
successful non-streaming real-vLLM request against existing loopback Gateway
and Tempo forwards. It requires all four services to have an unbroken parent
path to the caller, scans the complete returned trace for sensitive exports,
and writes a redacted summary with `release_signoff:false`. Setup, timeout
bounds, and explicit coverage limits are in `scripts/e2e/README.md`.
`bash tests/shell/trace_conformance_test.sh` runs its offline protocol, privacy,
credential-boundary, and export-settling regressions; it does not exercise a
live collector or substitute for pinned-image evidence.

The generated-file verifier backs up and restores checked-in outputs, so a
failure reports the precise stale files without silently rewriting a caller's
worktree.

Remote latency assertions compare matched measured baselines with declared gates. CI must not assert universal RTX latency numbers or publish local results.
