# ADR 0005: Use Kustomize for Kubernetes composition

## Context

InferScale needs a readable local base and small hardware/environment variants without building a chart templating layer.

## Decision

Use Kustomize bases for control plane, storage, gateway, routing prerequisites, and monitoring; overlays select local WSL or remote 1/2/4-GPU profiles. Controller-generated per-deployment resources use Go types/unstructured APIs, not checked-in user templates.

## Alternatives considered

- Helm for all platform manifests: rejected because values/templates add indirection for a small owned stack.
- Raw copied YAML per environment: rejected due drift.
- Operator Lifecycle Manager: outside v1 scope.

## Consequences

Every overlay is rendered in CI. External operators may still be installed from their official pinned manifests/charts; their lifecycle is recorded in the compatibility matrix.
