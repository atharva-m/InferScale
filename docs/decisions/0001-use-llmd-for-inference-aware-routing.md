# ADR 0001: Use llm-d for inference-aware routing

## Context

InferScale needs load-, fairness-, prefix-, and KV-aware endpoint selection. Reimplementing request scheduling or a KV index would duplicate a fast-moving ecosystem component and put proxy logic in the control plane.

## Decision

Use Gateway API `InferencePool v1` with a deployment-scoped llm-d endpoint picker. InferScale renders pool membership, bounded policy/objective configuration, admission metadata, and pinned compatibility inputs; llm-d selects among accepted ready workers. The candidate image is digest-pinned and release use remains blocked until the `versions.lock.yaml` conformance suite passes.

## Alternatives considered

- Custom reverse proxy/router: rejected as duplicate data-plane infrastructure.
- Round-robin Kubernetes Service only: retained as an experiment baseline, insufficient for v1 goals.
- Embed endpoint selection in the API/controller: rejected because it couples reconciliation to the request hot path.

## Consequences

InferScale depends on Gateway/llm-d API compatibility and needs conformance tests. The controller must create tenant-scoped EPP RBAC and cannot silently fall back when a requested capability is unavailable.
