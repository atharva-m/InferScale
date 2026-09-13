# ADR 0006: Use vLLM as the default backend

## Context

`backend:auto` cannot choose TensorRT-LLM without a compatible measured profile. vLLM offers the fastest path to direct Qwen correctness, streaming, continuous batching, prefix caching, and KV events.

## Decision

Use digest-pinned vLLM as the explicit default and the unbenchmarked fallback for `backend:auto`. Record requested backend, resolved backend, runtime digest, and `unbenchmarked` selection status. TensorRT-LLM becomes eligible only after the same model/hardware/precision/workload is measured.

## Alternatives considered

- Estimate TensorRT performance: rejected as scientifically invalid.
- Reject all `auto` requests without profiles: rejected because a safe vLLM baseline is useful.
- TensorRT-LLM first: rejected due engine-build and compatibility complexity.

## Consequences

Auto-selection is deterministic and honest but may not initially be cost-optimal. vLLM regressions affect the default path and require pinned-version testing.
