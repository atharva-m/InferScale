# Experimental Envoy AI Gateway adapter patch

This directory preserves the dependency correction used in the successful
2026-09-13 local Kubernetes GPU smoke test. It is not installed by the production
Kustomize bases or dependency installer, and it does not change versions.lock.yaml.

Envoy AI Gateway v0.7.0 queues duplicate EPP filters when several HTTPRoute rules
reference the same InferencePool. Its duplicate check examines filters already
in the listener, but not those queued during the current translation. InferScale's
four priority rules therefore produced four identical filters. Repeated EPP
admission could consume concurrency capacity before routing the request to vLLM;
with concurrency two, the local request stalled in the queue.

`epp-filter-dedup.patch` tracks queued filter names per filter chain. Its regression
test verifies one EPP filter across four priority rules and repeated translation,
authorization before EPP, unchanged routes, fail-closed full-duplex behavior, and
EPP disabled for unrelated routes. `epp-filter-dedup.provenance.json` records the
upstream archive, source/module hashes, patch hash and tested image identity.

To reproduce the image, obtain the official v0.7.0 source archive, verify its
SHA-256 against the provenance file, extract it into a separate working directory,
and apply the patch there with `patch -p1`. Build from that extracted source using
`Dockerfile.controller-local` and an explicitly CPU/memory-capped BuildKit builder.
The Dockerfile verifies unchanged module files, runs the extension-server package
tests, and builds the controller with network access disabled after dependencies
are downloaded. No runtime/model image rebuild is required. Run builds serially.

The local image passed the extension-server package tests and the GPU smoke:
authenticated JSON/SSE, controller restart, pending candidate preservation,
supersession, abort, and cleanup. Envoy inspection confirmed one EPP filter after
authorization. The affected EPP was restarted before the successful run to clear
state left by duplicate filter invocations. Concurrency was not increased.

This is still an experimental dependency combination: the source depends on
Envoy Gateway 1.8.1 while the platform pins 1.8.0. The adapter rejects multiple
InferencePools in one rule, so healthy dual-revision and weighted-canary routing
remain unsupported. Its 300-second ext-proc timeout does not satisfy the planned
900-second cold activation bound. These tests do not establish production Gateway
conformance, multi-GPU scaling, or performance.

The local test used Qwen3-0.6B on one RTX 4070 Laptop GPU, BF16, TP=1, context 2048,
load-aware routing and prefix caching disabled to verify the off-setting regression.
Prefix caching remains a supported feature and defaults to enabled in the API.
The successful runtime used CUDA graphs; an eager-mode debugging trial never ran.
Runtime memory fraction 0.65, the older model runner, and the corrected entrypoint
mounted into the cached image were explicit local overrides. The final raw run ID
is `local-gpu-20260913T162352Z-a20af79d`; private/local artifacts remain ignored.
