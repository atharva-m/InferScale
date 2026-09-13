# ADR 0007: Publish only remote Linux GPU benchmarks

## Context

WSL2, an 8 GB RTX 4070, local background load, and constrained/quantized checkpoints do not represent the target 32 GB RTX 5090 system.

## Decision

Use local runs only for correctness and developer feedback. Publish performance/cost graphs only from the remote physical Linux host, with complete provenance and identical versioned scenarios. TP=2/4 requires GPUs on that same host.

## Alternatives considered

- Publish local results with caveats: rejected because caveats do not make environments comparable.
- Multi-node rental experiments: rejected because v1 is not a networking/NCCL project.

## Consequences

Publishable acceptance depends on rental availability/budget. The README contains no placeholder or local headline numbers.
