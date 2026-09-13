# ADR 0002: Start with one InferenceDeployment CRD

## Context

Separate public CRDs for models, runtimes, routes, caches, rollouts, and autoscaling would expose internal topology and create cross-resource lifecycle ambiguity before behavior is stable.

## Decision

Expose one namespaced `platform.inferscale.io/v1alpha1 InferenceDeployment`. It contains the tenant deployment's serving and policy intent; status contains platform-level phase, selected runtime, revision, replicas, endpoint, cache, and bounded conditions. Controller-owned Kubernetes resources remain implementation details.

## Alternatives considered

- CRD per subsystem: rejected as premature public API surface.
- PostgreSQL-only imperative orchestration: rejected because it loses Kubernetes-native reconciliation and status.

## Consequences

The CRD schema must be structural, generation-aware, and carefully versioned. Serving-contract changes still create immutable internal revisions even though the public desired object remains one resource.
