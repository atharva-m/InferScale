# ADR 0012: Use exclusive co-located GPU allocation

## Context

RTX 5090 has no MIG isolation, time slicing weakens tenant isolation and benchmark validity, and multi-node tensor parallelism adds networking/collective complexity.

## Decision

One worker replica exclusively requests `accelerator.count` GPUs on one node, and v1 requires that count to equal tensor parallelism (`1/2/4`). Replica scaling multiplies GPU demand. No time slicing and no multi-node TP.

## Alternatives considered

- NVIDIA time slicing: rejected for isolation/performance interference.
- Fractional GPU scheduler: rejected as unsupported and out of scope.
- Multi-node TP: deferred beyond v1.

## Consequences

Capacity validation must happen before rollout and pods can remain pending when a contiguous co-located allocation is unavailable. Measurements have clear GPU ownership and cost accounting.
