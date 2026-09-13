# ADR 0011: Use a routing pool and EPP per immutable revision

## Context

Progressive rollout must compare and shift traffic between revisions that may use different runtime backends. Current llm-d `InferenceObjective` objects refer to one same-namespace pool, and a homogeneous EPP cannot safely represent every vLLM-to-TensorRT-LLM transition.

## Decision

Render one runtime Service, EPP, `InferencePool`, and three priority objectives for each immutable revision. The deployment HTTPRoute has class-specific rules and weighted stable/candidate pool backends. A backend-level request-header modifier selects the objective belonging to the backend revision after the Gateway chooses a pool.

Shadow traffic mirrors to the candidate runtime Service and discards its response because a mirror filter cannot attach a candidate-specific objective header. Canary traffic always uses the candidate pool/EPP. Both revisions expose the same tenant-visible served model name, so the OpenAI request body remains stable.

## Alternatives considered

- One pool plus model rewrite: rejected as the universal mechanism because backend-changing rollouts need different runtime/EPP contracts.
- One objective shared by both pools: impossible because the pool reference is required and same-namespace.
- Shadow through a custom proxy: rejected as out of scope.

## Consequences

Rollouts temporarily consume resources for two pools and EPPs. Backend header mutation for extension backends and fractional Service mirroring are mandatory conformance cases; failed candidates remain inspectable before retention cleanup.
