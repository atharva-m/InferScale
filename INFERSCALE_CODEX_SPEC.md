# InferScale

A production-grade, Kubernetes-native, multi-tenant LLM inference platform that composes modern inference runtimes and infrastructure instead of reimplementing them.

InferScale is intentionally **not** another FastAPI wrapper around an LLM. Its purpose is to manage deployments, routing, scaling, caching, observability, backend selection, and rollout policy across GPU inference workers.

---

## 1. Project Goal

Build a mini inference cloud that accepts deployment specifications such as:

```yaml
model: Qwen/Qwen3-8B
backend: auto

accelerator:
  type: RTX_5090
  count: 1

precision: bf16
tensor_parallelism: 1

scaling:
  min_replicas: 0
  max_replicas: 8

slo:
  ttft_p95_ms: 750
  tpot_p95_ms: 50
```

and creates/manages the underlying inference infrastructure.

The system should eventually support:

- text generation only
- streaming responses
- vLLM
- TensorRT-LLM
- continuous/in-flight batching
- prefix caching
- KV-aware routing
- multi-tenancy
- tenant quotas
- request prioritization and fairness
- overload backpressure
- model weight caching
- queue/saturation-aware autoscaling
- scale-to-zero
- cold/cache-warm/process-warm measurements
- GPU-aware placement
- single-node multi-GPU tensor parallelism
- shadow traffic
- canary rollout
- automatic rollback
- backend benchmarking and automatic backend selection
- Prometheus/Grafana observability
- OpenTelemetry tracing
- DCGM GPU telemetry

Post-MVP extensions:

- speculative decoding
- disaggregated prefill/decode
- KV offloading / transfer experiments
- smarter placement and autoscaling
- custom Triton kernel
- Nsight profiling

---

# 2. Design Philosophy

Do not reinvent infrastructure that already exists.

InferScale should compose proven components and implement the platform-specific decision-making between them.

Responsibility split:

| Concern | Owner |
|---|---|
| Deployment API | InferScale |
| Tenant model | InferScale |
| Deployment CRD | InferScale |
| Runtime/backend selection | InferScale |
| Model revision lifecycle | InferScale |
| Benchmark registry | InferScale |
| Cost accounting | InferScale |
| Rollout state machine | InferScale |
| Model cache policy | InferScale |
| Tenant quotas | InferScale |
| LLM request routing | llm-d / Gateway API Inference Extension |
| Prefix/KV-aware worker selection | llm-d |
| Flow control / fairness | llm-d |
| Continuous batching | vLLM / TensorRT-LLM |
| KV allocation / paged attention | Runtime |
| Tensor parallel execution | Runtime |
| GPU allocation | Kubernetes + NVIDIA device plugin |
| GPU telemetry | DCGM |
| Metrics | Prometheus |
| Traces | OpenTelemetry |
| Dashboards | Grafana |
| Infrastructure provisioning | Terraform / provider tooling |

InferScale should own the decisions **between** these layers.

---

# 3. Supported Environments

## 3.1 Local development

Host:

- Windows
- WSL2 Ubuntu
- NVIDIA RTX 4070
- 8 GB VRAM

Purpose:

- Go/Python development
- API and controller work
- CRD testing
- lightweight Kubernetes control-plane testing
- local vLLM integration
- small-model correctness tests
- observability integration
- deployment reconciliation tests

Local Kubernetes:

- k3d or another lightweight Kubernetes distribution
- do **not** require a full local Kubernetes GPU stack

GPU inference may run directly under WSL2/Docker while the Kubernetes cluster hosts control-plane objects.

Local GPU results are development results only and should not be used as headline performance benchmarks.

Suggested local model:

- Qwen3-8B-AWQ, with conservative context length and concurrency
- a smaller Qwen model may be used for initial smoke tests

---

## 3.2 Remote benchmark environment

Single physical Linux host with:

- 1, 2, or 4 RTX 5090 GPUs
- 32 GB VRAM per GPU
- Ubuntu 24.04 preferred
- Kubernetes/K3s
- NVIDIA driver
- NVIDIA Container Toolkit
- NVIDIA Kubernetes device plugin
- DCGM Exporter

Possible providers:

- Vast.ai
- similar GPU rental platforms

Important constraint:

**multi-GPU experiments must use multiple GPUs on the same physical machine.**

Do not turn v1 into a multi-node NCCL/networking project.

All publishable performance measurements should come from the remote GPU host.

---

# 4. Model Scope

Primary systems benchmark model:

```text
Qwen3-8B
```

Reasons:

- under 10B parameters
- suitable for vLLM
- suitable for TensorRT-LLM
- clean target for prefix/KV-cache experiments
- suitable for TP=1/2/4 benchmarking
- can be quantized for constrained local testing

Optional compatibility model later:

```text
Qwen3.5-9B
```

Use it to test whether InferScale handles newer hybrid model architectures.

Do not make Qwen3.5 the initial systems benchmark because hybrid architectures can complicate classical KV-cache analysis.

---

# 5. High-Level Architecture

```text
                         INFERScale CONTROL PLANE
 ┌─────────────────────────────────────────────────────────────────────┐
 │                                                                     │
 │  User ──► InferScale API (Go) ──► PostgreSQL                        │
 │                  │                                                  │
 │                  ▼                                                  │
 │         InferenceDeployment CRD                                     │
 │                  │                                                  │
 │                  ▼                                                  │
 │        InferScale Controller (Go)                                   │
 │                  │                                                  │
 │       ┌──────────┼───────────┬──────────────┐                       │
 │       │          │           │              │                       │
 │       ▼          ▼           ▼              ▼                       │
 │   Runtime     Scaling     Routing       Rollout                     │
 │   Adapter      Policy      Config        State                      │
 │                                                                     │
 └───────────────────────┬─────────────────────────────────────────────┘
                         │ Kubernetes API
                         ▼
 ─────────────────── KUBERNETES DATA PLANE ─────────────────────────────

                  ┌──────────────────────┐
 Client ─────────►│ Envoy / Gateway API  │
                  │ TLS / auth / quota   │
                  └──────────┬───────────┘
                             │
                             ▼
                   ┌──────────────────┐
                   │  llm-d Router    │
                   │                  │
                   │ • flow control   │
                   │ • fairness       │
                   │ • prefix routing │
                   │ • KV awareness   │
                   │ • load awareness │
                   └────────┬─────────┘
                            │
                       InferencePool
                            │
          ┌─────────────────┼─────────────────┐
          ▼                 ▼                 ▼
    ┌───────────┐     ┌───────────┐     ┌───────────┐
    │  Worker   │     │  Worker   │     │  Worker   │
    │ vLLM/TRT  │     │ vLLM/TRT  │     │ vLLM/TRT  │
    │  GPU 0    │     │  GPU 1    │     │  GPU 2    │
    └───────────┘     └───────────┘     └───────────┘

 ───────────────────── GPU / HOST LAYER ─────────────────────────────────

    NVIDIA driver + Container Toolkit
                  │
    NVIDIA Kubernetes Device Plugin
                  │
               GPUs
                  │
           DCGM Exporter

         node-local NVMe model cache
        /var/lib/inferscale/models/

 ─────────────────── OBSERVABILITY ──────────────────────────────────────

 vLLM/TRT metrics ─┐
 llm-d metrics ────┼──► Prometheus ──► Grafana
 InferScale ───────┤
 DCGM ─────────────┘

 services ──OTLP──► OpenTelemetry Collector
```

---

# 6. Core Architecture Layers

## 6.1 Control plane

Responsible for:

- deployment CRUD
- deployment reconciliation
- runtime selection
- model revision management
- rollout state
- autoscaling configuration
- benchmark profile lookup
- model cache orchestration
- tenant ownership
- platform metadata

Core services:

```text
InferScale API
InferScale Controller
PostgreSQL
Valkey
```

---

## 6.2 Routing/data plane

Use:

- Envoy / Gateway API
- Gateway API Inference Extension
- llm-d router / Endpoint Picker
- InferencePool

Do not implement a full KV-aware router from scratch.

InferScale configures routing policy and supplies tenant/product semantics.

---

## 6.3 GPU execution plane

Backends:

```text
vLLM
TensorRT-LLM
```

Runtime responsibilities include:

- continuous/in-flight batching
- paged KV management
- tensor parallelism
- attention kernels
- prefix cache implementation
- speculative decoding later
- disaggregated serving later

---

# 7. Multi-Tenancy Model

v1 uses:

```text
multi-tenant cluster
+
tenant-dedicated inference deployments
```

Example:

```text
GPU Cluster
├── Tenant A
│   ├── Deployment A1
│   └── Deployment A2
├── Tenant B
│   └── Deployment B1
└── Tenant C
    └── Deployment C1
```

Tenants may share:

- Kubernetes cluster
- gateway
- control plane
- monitoring stack
- GPU inventory

Tenants should not share by default:

- model server process
- KV cache
- untrusted prefix cache entries

Cross-tenant KV reuse is explicitly out of scope for v1.

Use tenant-scoped cache isolation/salting where supported.

---

# 8. InferenceDeployment CRD

Start with exactly one custom CRD.

```yaml
apiVersion: platform.inferscale.io/v1alpha1
kind: InferenceDeployment

metadata:
  name: qwen-chat
  namespace: tenant-acme

spec:

  model:
    uri: hf://Qwen/Qwen3-8B
    revision: "<immutable-model-revision>"

  runtime:
    backend: auto
    precision: bf16
    tensorParallelism: 1
    maxModelLen: 8192

    prefixCaching:
      enabled: true

  accelerator:
    vendor: nvidia
    type: RTX_5090
    count: 1

  scaling:
    minReplicas: 0
    maxReplicas: 8
    policy: saturation

  slo:
    ttft:
      percentile: 95
      targetMs: 750

    tpot:
      percentile: 95
      targetMs: 50

  admission:
    maxConcurrentRequests: 32
    maxQueuedRequests: 128
    priorityClass: standard

  rollout:
    strategy: progressive
    shadowPercent: 10

  observability:
    tracing: true
```

The SLO numbers above are examples only.

Do not hard-code them as universally achievable defaults.

---

## 8.1 CRD status

Example:

```yaml
status:

  phase: Ready

  observedGeneration: 12

  endpoint:
    url: https://api.inferscale.local/qwen-chat

  runtime:
    backend: vllm
    version: "<runtime-version>"
    modelRevision: "<revision>"
    tensorParallelism: 1

  replicas:
    desired: 2
    ready: 2

  revision:
    stable: qwen-chat-a8f32
    candidate: null

  cache:
    weights: Warm
    workersWarm: 2

  conditions:
    - type: ModelCached
      status: "True"

    - type: RuntimeReady
      status: "True"

    - type: RouteReady
      status: "True"
```

Status should represent platform-level state rather than every raw Kubernetes detail.

---

# 9. Control API

Initial API surface:

```text
POST   /v1/deployments
GET    /v1/deployments
GET    /v1/deployments/{id}
PATCH  /v1/deployments/{id}
DELETE /v1/deployments/{id}

POST   /v1/deployments/{id}/benchmarks
GET    /v1/deployments/{id}/benchmarks
GET    /v1/deployments/{id}/metrics
```

Example create request:

```json
{
  "name": "qwen-chat",

  "model": {
    "uri": "hf://Qwen/Qwen3-8B",
    "revision": "<immutable-revision>"
  },

  "backend": "auto",

  "gpu": {
    "type": "RTX_5090",
    "count": 1
  },

  "precision": "bf16",
  "tensor_parallelism": 1,
  "max_model_len": 8192,

  "min_replicas": 0,
  "max_replicas": 8,

  "slo": {
    "ttft_p95_ms": 750,
    "tpot_p95_ms": 50
  }
}
```

A change to any significant serving characteristic should create a **new deployment revision**.

Examples:

- model revision
- backend
- precision
- quantization
- tensor parallelism
- max context length
- speculative decoding mode later
- disaggregated serving mode later

Do not mutate active runtime configuration in-place when it changes the serving contract.

---

# 10. Inference API

Expose an OpenAI-compatible text-generation surface.

Initial endpoint:

```text
POST /v1/deployments/{deployment}/chat/completions
```

Support:

- streaming SSE
- standard chat message input
- token usage reporting

Do not support:

- embeddings
- image input
- audio
- multimodal
- fine-tuning
- training

InferScale is permanently scoped to text generation.

---

# 11. Kubernetes Resource Graph

One InferenceDeployment should roughly reconcile into:

```text
InferenceDeployment
       │
       ├── Model Prefetch Job
       │
       ├── Runtime workload
       │      └── N worker Pods
       │
       ├── InferencePool
       │
       ├── llm-d Router / EPP config
       │
       ├── HTTPRoute
       │
       ├── Autoscaling resource
       │
       ├── ServiceMonitor
       │
       ├── NetworkPolicy
       │
       └── Secrets / ConfigMaps
```

Recommended worker labels:

```yaml
inferscale.io/deployment: qwen-chat
inferscale.io/revision: a8f32
inferscale.io/backend: vllm
inferscale.io/model: qwen3-8b
inferscale.io/tenant: acme
```

---

# 12. Runtime Adapter Interface

The controller must not hard-code runtime-specific behavior throughout the codebase.

Conceptual interface:

```go
type RuntimeAdapter interface {
    Validate(spec DeploymentSpec) error

    Render(
        spec DeploymentSpec,
        revision Revision,
    ) ([]client.Object, error)

    Capabilities() RuntimeCapabilities
}
```

Directory structure:

```text
internal/runtime/
├── adapter.go
├── vllm/
│   └── adapter.go
└── trtllm/
    └── adapter.go
```

Avoid scattered logic such as:

```go
if backend == "vllm" {
    ...
} else if backend == "trtllm" {
    ...
}
```

---

# 13. Runtime Capabilities

Each backend declares capabilities.

Possible fields:

```text
PrefixCache
KVEventReporting
CacheIsolation
ContinuousBatching
PriorityPropagation
BF16
FP8
TensorParallelism
SpeculativeDecoding
DisaggregatedServing
```

Conceptual Go type:

```go
type RuntimeCapabilities struct {
    PrefixCache          bool
    KVEventReporting     bool
    CacheIsolation       bool
    ContinuousBatching   bool
    PriorityPropagation  bool
    BF16                 bool
    FP8                  bool
    TensorParallelism    bool
    SpeculativeDecoding  bool
    DisaggregatedServing bool
}
```

The control plane must reject invalid combinations instead of silently ignoring unsupported settings.

---

# 14. Request Path

```text
1. Client
      ↓
2. Envoy Gateway
      ↓
3. Authentication + tenant lookup
      ↓
4. Quota / concurrency admission
      ↓
5. Attach:
      tenant identity
      fairness identity
      priority
      trace ID
      ↓
6. llm-d Flow Control
      ↓
7. Endpoint Picker scores candidate workers
      ↓
8. Prefix/KV locality + load decision
      ↓
9. Selected runtime worker
      ↓
10. vLLM / TensorRT-LLM batching
      ↓
11. Stream generated tokens
      ↓
12. Record usage and telemetry
```

InferScale owns quota policy.

llm-d owns scheduling of accepted traffic.

---

# 15. Storage

## 15.1 PostgreSQL

Durable platform state.

Suggested tables:

```text
tenants
api_keys
deployments
deployment_revisions
runtime_profiles
benchmark_runs
benchmark_results
rollout_history
usage_hourly
```

Do not store every latency sample in PostgreSQL.

Prometheus owns time-series telemetry.

---

## 15.2 Valkey

Use for ephemeral coordination only:

```text
API rate-limit buckets
concurrency counters
short-lived deployment locks
idempotency keys
activation locks
```

Do not use Valkey as the inference request queue.

---

# 16. Model Weight Cache

Because the target production environment is one physical node, use a node-local cache.

Path:

```text
/var/lib/inferscale/models/
```

Example layout:

```text
/var/lib/inferscale/models/
├── sha256-AAAA/
├── sha256-BBBB/
└── sha256-CCCC/
```

Requirements:

- content/revision-addressed
- immutable model revision
- checksum verification
- atomic completion marker
- worker mounts cache read-only
- avoid duplicate concurrent downloads
- support eviction policy later

Flow:

```text
InferenceDeployment
       ↓
Model Prefetch Job
       ↓
cache exists?
  │          │
 yes         no
  │          ↓
  │     download snapshot
  │          ↓
  │     verify revision
  │          ↓
  │     atomic .complete
  │
  └──────────┬─────
             ▼
        launch worker
```

---

# 17. Startup States

Measure three states separately.

## 17.1 Remote-cold

```text
weights absent
↓
download
↓
load into process/GPU
↓
ready
```

## 17.2 Cache-warm

```text
weights on local NVMe
↓
load into process/GPU
↓
ready
```

## 17.3 Process-warm

```text
model already resident
↓
serve immediately
```

Never combine these into one generic "cold-start latency" number.

---

# 18. GPU Allocation

For v1:

```text
one runtime GPU allocation
=
one exclusive GPU allocation
```

Examples:

```text
1 worker → 1 GPU
1 TP=2 worker → 2 GPUs
1 TP=4 worker → 4 GPUs
```

Do not use GPU time slicing between unrelated tenant workloads.

The RTX 5090 does not provide MIG isolation.

---

# 19. Autoscaling

## 19.1 Initial scaling

Implement:

```text
1 → N replicas
```

Use queue/saturation-related metrics rather than CPU.

Candidate metrics:

```text
queue depth
waiting requests
running requests
KV-cache pressure
estimated queued token work
```

Prefer KEDA/HPA as the scaling mechanism.

InferScale should own the LLM-specific signal/policy, not reimplement Kubernetes replica mechanics.

---

## 19.2 Scale-to-zero

Later in v1:

```text
0 → 1 → N
```

Use the existing llm-d scale-from-zero / activator path if compatible.

Do not invent a second custom request-activation proxy unless absolutely necessary.

Measure:

```text
warm running
cache-warm scale-from-zero
remote-cold scale-from-zero
```

---

# 20. Backend Benchmark Registry

`backend: auto` must be based on measured data.

Benchmark profile key:

```text
model_revision
backend
backend_version
GPU SKU
GPU count
precision
tensor-parallel degree
max-context bucket
```

Store:

```text
P50 TTFT
P95 TTFT
P99 TTFT

P50 TPOT
P95 TPOT
P99 TPOT

input tokens/sec
output tokens/sec

GPU utilization
GPU memory
GPU power

cost / 1M input tokens
cost / 1M output tokens
```

Selection algorithm:

```text
candidate configurations
        ↓
remove SLO violations
        ↓
choose lowest measured cost among survivors
```

If no benchmark profile exists:

```text
backend:auto
↓
default to vLLM
↓
mark selection as unbenchmarked
```

Never fabricate or estimate TensorRT-LLM performance without measurements.

---

# 21. Progressive Rollout

Deployment changes create:

```text
stable revision
+
candidate revision
```

State machine:

```text
Candidate Ready
      ↓
Shadow
      ↓
Canary 5%
      ↓
Canary 25%
      ↓
Canary 50%
      ↓
100%
      ↓
Stable
```

Suggested gates:

```text
5xx/runtime error rate
TTFT regression
TPOT regression
queue latency
OOM count
worker restarts
request failure rate
```

Rollback:

```text
candidate weight → 0
stable weight    → 100
candidate state  → Failed
```

Use Gateway API / Envoy traffic mirroring and traffic weighting.

Do not implement a custom reverse proxy for rollout traffic.

---

# 22. Observability

## 22.1 GPU metrics

DCGM:

```text
GPU utilization
VRAM usage
memory bandwidth
PCIe throughput
power
temperature
health/XID errors
```

---

## 22.2 Runtime metrics

Capture:

```text
TTFT
TPOT
E2E request latency
queue time
prompt tokens/sec
generation tokens/sec
running requests
waiting requests
KV-cache utilization
prefix-cache hit ratio
preemptions
OOM/failure counters
```

---

## 22.3 Router metrics

Capture:

```text
queue depth
flow-control wait
routing decision latency
routing reason
worker selected
cache-affinity signal
request rejection count
tenant fairness metadata
```

---

## 22.4 InferScale metrics

Capture:

```text
deployment state
revision
backend selected
scale events
rollout stage
cache state
model download duration
model load duration
cold-start duration
tenant throttles
GPU-hour usage
estimated cost
```

---

## 22.5 Tracing

Trace the full request:

```text
gateway
↓
admission
↓
llm-d queue
↓
routing decision
↓
runtime
↓
first token
↓
last token
```

Use OpenTelemetry end-to-end.

---

# 23. Benchmark Dimensions

Do not run a full Cartesian explosion initially.

Start with selected experiments.

Dimensions:

| Dimension | Values |
|---|---|
| Backend | vLLM, TensorRT-LLM |
| Scheduler | static/naive baseline, continuous/in-flight |
| Prefix cache | off, on |
| Routing | round-robin, least-load, prefix/KV-aware |
| Input length | 128, 2K, 8K |
| Output length | 32, 256, 1K |
| Concurrency | 1, 8, 32, overload |
| Precision | BF16, FP8 where supported |
| GPUs | 1, 2, 4 |
| TP | 1, 2, 4 |
| Cache state | remote-cold, cache-warm, process-warm |
| Scaling | warm, scale-from-zero |

---

# 24. Benchmark Output Metrics

Every benchmark result should capture:

```text
P50/P95/P99 TTFT
P50/P95/P99 TPOT
P50/P95/P99 E2E latency
P50/P95/P99 queue latency

prompt tokens/sec
output tokens/sec
total throughput

GPU utilization
GPU memory
power draw

KV-cache utilization
prefix-cache hit rate

requests rejected
SLO attainment percentage
runtime errors

cold-start time
model-download time
model-load time

GPU-hours
cost / 1M input tokens
cost / 1M output tokens
```

---

# 25. Repository Layout

```text
inferscale/
│
├── cmd/
│   ├── api/
│   │   └── main.go
│   │
│   ├── controller/
│   │   └── main.go
│   │
│   └── admission/
│       └── main.go
│
├── internal/
│   ├── api/
│   ├── auth/
│   ├── quota/
│   ├── tenant/
│   │
│   ├── controller/
│   │   ├── deployment/
│   │   ├── rollout/
│   │   ├── modelcache/
│   │   └── autoscaling/
│   │
│   ├── runtime/
│   │   ├── adapter.go
│   │   ├── vllm/
│   │   └── trtllm/
│   │
│   ├── kubernetes/
│   └── storage/
│
├── api/
│   ├── crds/
│   │   └── inferscaledeployment.yaml
│   │
│   └── openapi/
│       └── openapi.yaml
│
├── deploy/
│   ├── base/
│   │   ├── inferscale/
│   │   ├── llm-d/
│   │   ├── gateway/
│   │   ├── monitoring/
│   │   └── storage/
│   │
│   └── overlays/
│       ├── local-wsl/
│       ├── vast-1x5090/
│       ├── vast-2x5090/
│       └── vast-4x5090/
│
├── runtime/
│   ├── vllm/
│   └── trtllm/
│
├── benchmarks/
│   ├── runner/
│   ├── scenarios/
│   ├── workloads/
│   └── analysis/
│
├── observability/
│   ├── dashboards/
│   ├── alerts/
│   └── prometheus/
│
├── infra/
│   ├── terraform/
│   └── bootstrap/
│
├── scripts/
│
└── docs/
    ├── architecture/
    ├── benchmarks/
    └── decisions/
```

Use:

- Go for API/controller/platform services
- Python for benchmark orchestration/analysis
- Kustomize for Kubernetes application composition
- Terraform for infrastructure provisioning

---

# 26. Implementation Milestones

## M0 — Local infrastructure

Install/configure:

```text
WSL2 Ubuntu
Docker
k3d
kubectl
Go
Python
PostgreSQL
Valkey
Prometheus
Grafana
OpenTelemetry
```

Acceptance criteria:

- all infrastructure starts reproducibly
- one documented command or Make target brings up the local dev stack
- health checks pass
- no LLM deployment required yet

Suggested targets:

```bash
make dev-up
make dev-status
make dev-down
```

---

## M1 — Direct vLLM baseline

Run Qwen directly without InferScale.

Flow:

```text
client
↓
vLLM
↓
RTX 4070
```

Capture baseline:

```text
TTFT
TPOT
tokens/sec
GPU memory
GPU utilization
```

Acceptance criteria:

- model serves successfully
- streaming works
- benchmark script records baseline
- environment and model parameters are documented

No Kubernetes inference worker required yet.

---

## M2 — InferScale control plane

Implement:

- InferenceDeployment CRD
- InferScale API
- InferScale controller
- PostgreSQL persistence

Initially the controller may reconcile to a mock/trivial Kubernetes Deployment.

Acceptance criteria:

```text
POST /v1/deployments
```

creates:

- database state
- InferenceDeployment object
- reconciled Kubernetes resources
- CRD status updates

---

## M3 — vLLM RuntimeAdapter

Implement the vLLM adapter.

Flow:

```text
InferenceDeployment
↓
RuntimeAdapter
↓
vLLM worker
```

Acceptance criteria:

- create a Qwen deployment through InferScale
- worker becomes ready
- inference endpoint streams output
- status reflects backend and readiness

---

## M4 — llm-d / InferencePool routing

Add:

```text
Gateway
llm-d Router
InferencePool
multiple workers
```

Experiments:

```text
round-robin
vs
load-aware
vs
prefix/KV-aware
```

Acceptance criteria:

- requests reach multiple workers
- routing mode is configurable
- router metrics are collected
- prefix-reuse workload demonstrates measurable routing differences

---

## M5 — Observability

Add:

```text
Prometheus
Grafana
OpenTelemetry
vLLM metrics
llm-d metrics
InferScale metrics
```

Remote environment additionally:

```text
DCGM
```

Acceptance criteria:

- end-to-end request trace exists
- runtime dashboard exists
- router dashboard exists
- deployment dashboard exists
- GPU dashboard exists remotely

Do not optimize systems before this milestone is complete.

---

## M6 — Multi-tenancy and flow control

Add:

```text
tenants
API keys
tenant ownership
rate limits
concurrency quotas
priority classes
fairness IDs
backpressure
```

Priority classes:

```text
interactive
standard
batch
```

Acceptance criteria:

- one tenant cannot exceed configured quota
- overload returns controlled rejection
- noisy tenant does not starve another tenant
- fairness/priority behavior is benchmarked

---

## M7 — Model cache

Implement node-local model cache.

Measure:

```text
remote-cold
cache-warm
process-warm
```

Acceptance criteria:

- immutable revision cache
- duplicate download protection
- cache hit/miss observability
- reproducible cold-start benchmark
- model cache can be cleared intentionally for tests

---

## M8 — Autoscaling

Stage A:

```text
1 → N
```

Stage B:

```text
0 → 1 → N
```

Use queue/saturation metrics.

Acceptance criteria:

- burst load scales worker count
- scale-up occurs before severe queue collapse
- hysteresis prevents rapid oscillation
- scale-to-zero works
- all three startup states are measured separately

---

## M9 — Remote RTX 5090 benchmarks

First:

```text
1 × RTX 5090
```

Then:

```text
2 × RTX 5090
```

Compare:

```text
TP=1
TP=2
```

Then:

```text
4 × RTX 5090
```

Compare:

```text
TP=1
TP=2
TP=4
```

Acceptance criteria:

- all hardware/environment details recorded
- same workload definitions reused across configurations
- throughput scaling efficiency calculated
- latency tradeoffs reported
- GPU utilization and communication effects documented

---

## M10 — Progressive rollout

Implement:

```text
revision creation
shadow
canary
promotion
rollback
```

Acceptance criteria:

- new revision can receive mirrored traffic
- canary weights progress through policy stages
- metric regression triggers rollback
- failed candidate remains inspectable
- stable revision resumes 100% traffic

Required demo:

deploy a deliberately bad candidate and show automatic rollback.

---

## M11 — TensorRT-LLM

Implement second RuntimeAdapter.

Compare on:

```text
same model
same hardware
same precision
same workload
same SLO
```

Acceptance criteria:

- TensorRT-LLM deployment works through InferScale
- benchmark profiles stored
- backend comparison report generated
- backend:auto chooses only from measured compatible profiles

---

# 27. Post-MVP v1.x

## v1.1 — Speculative decoding

Compare:

```text
Qwen3-8B normal decode
vs
Qwen3-8B + draft model
```

Possible draft model:

```text
Qwen3-0.6B
```

Measure:

```text
TTFT
TPOT
throughput
acceptance rate
accepted speculative tokens
GPU utilization
cost/output token
performance vs concurrency
```

Research question:

**Under what concurrency and workload distributions does speculative decoding help or hurt?**

Do not assume it always improves performance.

---

## v1.2 — Disaggregated prefill/decode

Architecture evolves toward:

```text
request
↓
prefill pool
↓
KV transfer
↓
decode pool
```

Measure:

```text
TTFT
TPOT
throughput
KV-transfer overhead
prefill saturation
decode saturation
cost
```

Research question:

**When does disaggregation improve latency isolation enough to justify KV-transfer overhead and additional scheduling complexity?**

---

## v1.3 — Advanced scheduling

Possible experiments:

- predictive queue delay
- token-work backlog estimates
- SLO-risk routing
- cache-aware autoscaling
- smarter GPU placement
- KV offloading
- remote KV cache experiments

---

## v1.4 — Triton/Nsight

Implement one custom Triton kernel.

Suggested first operation:

```text
RMSNorm
```

Profile with Nsight.

Goal is not necessarily to beat optimized PyTorch/runtime kernels.

Goal is to demonstrate understanding of:

```text
GPU kernel
↓
memory bandwidth
↓
CUDA/Triton runtime
↓
collectives
↓
model runtime
↓
serving scheduler
↓
Kubernetes
↓
user latency
```

---

# 28. Router Abstraction for Future Features

Do not permanently model routing as:

```go
SelectWorker(request) Worker
```

Prefer an abstraction that can later express more complex execution.

Conceptual:

```go
Plan(request) ExecutionPlan
```

MVP example:

```text
ExecutionPlan
  backend: vllm
  worker: worker-3
  priority: interactive
```

Future speculative decode example:

```text
ExecutionPlan
  backend: vllm
  worker: worker-3

  decoding:
    mode: speculative
    draft_model: qwen3-0.6b
```

Future disaggregated example:

```text
ExecutionPlan
  backend: vllm

  prefill:
    worker: worker-1

  decode:
    worker: worker-3

  kv_transport:
    connector: <runtime-specific>
```

Do not implement unused post-MVP fields prematurely.

Only preserve the architectural ability to grow toward them.

---

# 29. Benchmark Harness

Do not create a custom high-performance load generator unless required later.

Python benchmark orchestration should:

```text
load scenario YAML
↓
apply runtime configuration
↓
clear/warm caches as required
↓
invoke benchmark tool
↓
scrape Prometheus
↓
capture DCGM
↓
normalize results
↓
store benchmark result
↓
generate plots
```

Possible underlying benchmark tools:

- vLLM benchmark utilities
- GuideLLM
- inference-perf
- another established compatible tool

The Python layer owns experiment reproducibility and result normalization.

---

# 30. Benchmark Scenario Format

Suggested structure:

```yaml
name: qwen3-8b-vllm-1gpu-bf16-prefix-on

deployment:
  model: Qwen/Qwen3-8B
  backend: vllm
  precision: bf16
  gpu_count: 1
  tensor_parallelism: 1
  prefix_cache: true
  max_model_len: 8192

workload:
  input_tokens: 2048
  output_tokens: 256
  concurrency: 8
  requests: 500

cache_state: process-warm

routing:
  policy: prefix-aware
```

Every run should store:

- Git commit
- container image digest
- model revision
- runtime version
- GPU model
- NVIDIA driver version
- CUDA version
- kernel version
- benchmark configuration
- timestamp

Reproducibility matters.

---

# 31. Cost Model

Do not report only:

```text
cost / 1M tokens
```

Prefer:

```text
cost / 1M input tokens
cost / 1M output tokens
GPU-hours
```

At minimum:

```text
run_cost =
GPU_hourly_price
×
GPU_count
×
run_duration_hours
```

Store rental price alongside each benchmark run.

Do not compare cost numbers across hardware/providers without recording the underlying rental price.

---

# 32. Failure Scenarios to Test

After core functionality works, intentionally test:

```text
worker crash
worker OOM
runtime readiness failure
model download failure
corrupt cache entry
router overload
tenant quota violation
GPU exhaustion
candidate rollout regression
scale-from-zero burst
Prometheus temporary failure
controller restart during reconciliation
duplicate deployment request
```

The controller must be idempotent.

Kubernetes reconciliation should recover from controller restarts.

---

# 33. Security Boundaries

v1 requirements:

- API-key authentication
- tenant ownership on deployments
- per-tenant namespace or equivalent isolation policy
- network policies
- read-only model-cache mount
- immutable model revision
- secret storage via Kubernetes Secrets
- no cross-tenant KV reuse
- tenant-specific rate/concurrency quotas
- cache salting where supported
- no arbitrary user-provided container images in v1

Do not build a full IAM product.

Keep security proportional to the project scope.

---

# 34. Non-Goals

Explicitly out of scope:

- training
- fine-tuning
- RLHF
- embeddings service
- image generation
- audio
- multimodal serving
- distributed multi-node inference
- building custom GPU drivers
- reimplementing continuous batching
- reimplementing PagedAttention
- reimplementing Kubernetes
- implementing Prometheus/Grafana
- implementing a full KV index from scratch
- writing a custom reverse proxy
- cross-tenant model-server process sharing in v1
- multi-cloud production support
- enterprise IAM/billing product

---

# 35. Coding Principles

Codex should follow these constraints.

## 35.1 Prefer integration over reimplementation

Before writing a large subsystem, check whether:

- Kubernetes
- llm-d
- Gateway API
- vLLM
- TensorRT-LLM
- KEDA/HPA
- Prometheus
- Grafana
- OpenTelemetry
- DCGM
- NVIDIA device plugin

already provide the needed mechanism.

InferScale should implement platform-specific policy and orchestration.

---

## 35.2 Keep services small

Do not create microservices unnecessarily.

Initial services:

```text
inferscale-api
inferscale-controller
inferscale-admission
```

Some may be combined during early development if doing so reduces complexity.

---

## 35.3 Prefer declarative reconciliation

The Kubernetes controller should be idempotent.

Desired pattern:

```text
desired state
vs
observed state
↓
reconcile
```

Avoid imperative orchestration chains where possible.

---

## 35.4 Do not optimize before measurement

Order:

```text
working baseline
↓
observability
↓
benchmark
↓
change
↓
benchmark again
```

Every optimization claim must be backed by a reproducible benchmark.

---

## 35.5 Avoid premature abstraction

The architecture should allow later speculative/disaggregated serving, but do not implement abstractions that are not yet needed.

Prefer simple interfaces that can evolve.

---

# 36. Documentation Requirements

Maintain Architecture Decision Records in:

```text
docs/decisions/
```

Recommended initial ADRs:

```text
0001-use-llmd-for-inference-aware-routing.md
0002-use-single-inference-deployment-crd.md
0003-tenant-dedicated-runtime-pools.md
0004-use-node-local-model-cache.md
0005-use-kustomize-for-k8s-composition.md
0006-use-vllm-as-default-backend.md
0007-remote-only-headline-benchmarks.md
```

Each ADR should contain:

```text
Context
Decision
Alternatives considered
Consequences
```

---

# 37. README Success Criteria

The final README should contain actual measured graphs.

Examples:

```text
TTFT vs concurrency
TPOT vs concurrency
throughput vs concurrency
GPU utilization vs concurrency
prefix-aware vs load-aware routing
prefix cache on/off
cold vs cache-warm vs process-warm
1 vs 2 vs 4 GPUs
TP=1 vs TP=2 vs TP=4
vLLM vs TensorRT-LLM
autoscaling burst response
overload behavior
canary rollback timeline
cost per 1M tokens
```

Do not publish placeholder numbers.

Every number must come from the benchmark harness.

---

# 38. Definition of MVP Complete

InferScale MVP is complete when all of the following are true:

- [ ] User can create an InferenceDeployment through the API.
- [ ] Controller reconciles it into Kubernetes resources.
- [ ] vLLM worker serves Qwen text generation.
- [ ] Streaming inference works.
- [ ] Gateway/llm-d routes requests to multiple workers.
- [ ] Prefix/KV-aware routing is available.
- [ ] Tenant authentication and ownership exist.
- [ ] Tenant rate/concurrency quotas exist.
- [ ] Overload produces controlled backpressure.
- [ ] Prometheus/Grafana dashboards exist.
- [ ] OpenTelemetry traces exist.
- [ ] DCGM metrics are collected on remote hardware.
- [ ] Model weight cache works.
- [ ] Remote-cold/cache-warm/process-warm states are measured separately.
- [ ] Queue/saturation-aware autoscaling works.
- [ ] Scale-to-zero works.
- [ ] 1/2/4 GPU experiments have been run where rental hardware permits.
- [ ] Tensor parallel experiments are measured.
- [ ] Progressive rollout works.
- [ ] Deliberately bad candidate automatically rolls back.
- [ ] TensorRT-LLM backend works.
- [ ] vLLM and TensorRT-LLM are compared on the same workload.
- [ ] `backend:auto` uses measured benchmark profiles.
- [ ] Final README contains reproducible graphs rather than placeholder metrics.

---

# 39. Immediate Codex Task

Do not begin with the Kubernetes controller.

Start with M0 and M1.

## Step 1

Create the repository skeleton described above.

## Step 2

Create local development tooling:

```text
Makefile
docker-compose/dev dependencies if useful
k3d bootstrap script
Kustomize local overlay
environment example file
developer setup documentation
```

## Step 3

Create commands:

```bash
make dev-up
make dev-status
make dev-down
```

These should start/check/stop the local non-GPU infrastructure.

## Step 4

Create the direct vLLM baseline setup.

Run a small Qwen checkpoint first if Qwen3-8B-AWQ does not fit reliably on the RTX 4070.

Do not redesign the architecture merely to accommodate the laptop.

The laptop exists for development, not production benchmarking.

## Step 5

Create a minimal benchmark script that records:

```text
TTFT
TPOT
request latency
input token count
output token count
tokens/sec
GPU memory usage if available
GPU utilization if available
```

Write results to a structured JSON file under:

```text
benchmarks/results/
```

## Step 6

Only after the direct baseline works, begin M2:

```text
InferenceDeployment CRD
InferScale API
InferScale Controller
```

---

# 40. Final Project Principle

InferScale is not an LLM runtime.

InferScale is the platform that decides:

```text
who may send work
↓
which deployment serves it
↓
which runtime configuration should exist
↓
where the model should run
↓
how traffic should be routed
↓
how overload is handled
↓
when capacity changes
↓
which backend is best
↓
whether a new revision is safe
↓
how the entire system is measured
```

Existing inference engines should remain responsible for:

```text
attention
KV allocation
continuous batching
CUDA kernels
tensor parallel execution
speculative execution
```

The project is successful if it demonstrates strong cross-layer reasoning across:

```text
GPU
↓
runtime
↓
routing
↓
autoscaling
↓
Kubernetes
↓
observability
↓
user-visible latency and cost
```
