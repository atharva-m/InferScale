# InferScale

InferScale is a Kubernetes-native, multi-tenant control and policy plane for
text-generation inference. It composes Envoy Gateway, llm-d, vLLM,
TensorRT-LLM, KEDA, Prometheus, OpenTelemetry, and NVIDIA telemetry rather than
reimplementing an inference runtime.

The implementation is organized as three services:

- `inferscale-api` manages tenants, immutable deployment revisions, benchmarks,
  and the PostgreSQL-to-Kubernetes outbox.
- `inferscale-controller` declaratively reconciles runtime, cache, routing,
  scaling, monitoring, and rollout resources.
- `inferscale-admission` authenticates inference requests and injects trusted
  tenant, fairness, priority, model, and SLO metadata at Envoy Gateway.

See `INFERSCALE_CODEX_SPEC.md` for the full project definition and
`docs/development/setup.md` for development setup.

## Development

```bash
cp .env.example .env
make dev-up
make dev-status
make test
make e2e-local-live
```

The live target provisions a test tenant on the local k3d stack and checks the
real control-plane path against the CPU fake runtime. It is a correctness smoke
test, not GPU, performance, or Gateway conformance evidence.

Direct local GPU measurements are development-only and are never used in
published comparisons. This README intentionally contains no performance
numbers until the remote benchmark harness has generated reproducible results.
