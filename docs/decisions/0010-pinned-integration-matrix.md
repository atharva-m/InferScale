# ADR 0010: Pin and conformance-test the integration matrix

## Context

Gateway API and `InferencePool` are stable, but llm-d objectives, endpoint-picker configuration, metrics, and some Envoy Gateway filters evolve independently. A manifest that merely parses can still fail at attachment or at request time.

## Decision

Record every Kubernetes, Gateway API, Inference Extension, Envoy Gateway,
llm-d, KEDA, runtime, observability, and storage version in
`versions.lock.yaml`. Checksum every downloaded manifest and verify the complete
set before applying any object. Rewrite workload images in verified upstream
manifests to locked digests before apply and reject unrecognized mutable images.
Pin controller-generated images too: Envoy's data-plane image is fixed in the
`EnvoyProxy`, while its shutdown-manager and optional rate-limit image are fixed
in the verified controller configuration. Isolate alpha llm-d group/version,
header, plugin, and metric names in the routing contract. A release remains
blocked while the lock file reports conformance `pending`.

The mandatory suite verifies external authorization, full-duplex inference processing, `InferencePool` backend references, backend request-header mutation, weighted stable/candidate traffic, fractional request mirroring, URL rewriting, native SSE preservation, and KEDA activation metrics against the exact pinned set.

## Alternatives considered

- Track latest tags: rejected because independent upgrades are not reproducible.
- Treat Kubernetes object acceptance as conformance: rejected because it misses data-path behavior.
- Fall back to a custom proxy: rejected by the v1 architecture boundary.

## Consequences

Dependency upgrades require independently verifying new artifact checksums,
inventorying every installed or generated workload image, updating the lock and
rewrite contract, and collecting fresh conformance evidence. A checksum,
unexpected image, or Extended Gateway feature failure blocks release instead of
silently weakening integrity, rollout, or admission behavior.
