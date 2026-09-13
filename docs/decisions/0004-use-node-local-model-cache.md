# ADR 0004: Use an immutable node-local model cache

## Context

v1 targets one physical GPU node. Repeated remote downloads dominate cold starts, while a distributed cache would add unnecessary network/storage orchestration.

## Decision

Store immutable Hugging Face snapshots under `/var/lib/inferscale/models/sha256-<key>`, where the key hashes URI, NUL, and lowercase commit SHA. A prefetch Job uses a POSIX lock, unique partial directory, regular-file SHA-256 manifest, fsync, `.complete`, and atomic rename. Workers mount completed entries read-only.

## Alternatives considered

- Download in each worker: rejected for duplication and poor startup measurement.
- Shared network filesystem/object-cache service: rejected for single-node v1 complexity.
- Mutable model tags: rejected because cache correctness and reproducibility require immutable commits.

## Consequences

Disk capacity/eviction is manually managed in v1. Remote-cold, cache-warm, and process-warm can be measured separately and corruption cannot become worker-ready silently.
