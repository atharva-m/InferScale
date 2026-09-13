# v1 architecture

InferScale is a control and policy plane, not an inference runtime.

| Service | Responsibility | Durable dependencies | Kubernetes authority |
|---|---|---|---|
| `inferscale-api` | Tenant-scoped deployment CRUD, immutable revisions, benchmark operations, idempotency/outbox | PostgreSQL; Valkey for short-lived idempotency | Apply/read `InferenceDeployment` through durable operations |
| `inferscale-controller` | Declarative cache/runtime/routing/scaling/rollout reconciliation and status | PostgreSQL for profiles/history; Prometheus for gates | CRD plus owned Jobs, Deployments, Services, InferencePools, EPP RBAC, routes, KEDA, policies, monitors |
| `inferscale-admission` | Authenticate inference traffic, enforce tenant rate limits, attach trusted llm-d concurrency/queue metadata | PostgreSQL keys/policies; Valkey rate buckets | No Kubernetes token |

PostgreSQL owns tenant/key identity, public deployment identity, immutable revision history, benchmark/profile provenance, usage aggregates, rollout events, and the reconciliation outbox. The `InferenceDeployment` CRD owns live Kubernetes desired state and platform status. An API transaction first stores a revision plus operation; an idempotent dispatcher applies the CRD and records completion. Because PostgreSQL and Kubernetes cannot share a transaction, clients see `202 Accepted` and operation/reconciliation state rather than false synchronous success.

## Resource flow

```text
API transaction -> deployment revision + outbox operation
                -> idempotent CRD apply
                -> controller observes generation
                -> immutable model prefetch Job
                -> runtime workload and Service
                -> deployment-scoped EPP + InferencePool
                -> route, KEDA ScaledObject, NetworkPolicy, monitors
                -> status conditions and observedGeneration
```

Significant serving changes create a candidate revision: immutable model commit, backend, precision/quantization, GPUs/TP, maximum context, or prefix-cache behavior. Replica bounds, admission limits, SLO thresholds, tracing, and rollout policy are mutable policy. Existing streams remain on their selected worker/revision; traffic weights affect only new requests.

Each worker replica requests an exclusive, co-located GPU allocation equal to its tensor-parallel degree. `accelerator.count` is GPUs per worker, not a deployment-wide total. Replica scaling multiplies that count. v1 accepts TP `1/2/4` only and never spans physical nodes.

llm-d owns accepted-request scheduling and cache/load-aware endpoint selection. vLLM/TensorRT-LLM own batching, paged KV memory, kernels, and tensor-parallel execution. KEDA owns HPA mechanics; InferScale renders the LLM-specific Prometheus signal and safety policy.

## Request path and trace boundary

Inference traffic does not pass through `inferscale-api`. Envoy Gateway calls
`inferscale-admission`, removes client scheduling metadata, and forwards an
accepted request through the revision EPP and InferencePool to the native
runtime. The public deployment path is rewritten to the runtime's
`/v1/chat/completions` endpoint, so streaming bytes and cancellation remain on
the native data path.

Gateway, admission, and vLLM use W3C `traceparent`/`tracestate` and export OTLP
to the in-cluster collector. Spans contain bounded protocol and platform
identity only; prompt/completion bodies, authorization values, model URIs, and
arbitrary client baggage are excluded. Continuity through the pinned Envoy,
llm-d EPP, and runtime builds is a live conformance requirement. Static
rendering alone is not release evidence, and TensorRT-LLM tracing is not
claimed until its pinned engine-backed path passes that gate.
