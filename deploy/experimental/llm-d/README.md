# Native round-robin EPP candidate

This patch adds `round-robin-picker` to the exact llm-d-router v0.9.0 source
and registers its factory in the EPP binary. It packages a candidate dependency
image. The controller renderer now selects this plugin for the `round-robin`
policy (and its internal empty-policy fallback), replacing the previous random
picker alias. `load-aware` and `prefix-aware` retain their scoring plugins and
`max-score-picker`. Production dependency pins remain pending exact-image live
qualification; use the [candidate dependency workflow](../envoy-ai-gateway/CANDIDATE-RELEASE.md)
to supply the compatible picker image.

The locally built candidate registry-manifest reference is
`ghcr.io/inferscale/llm-d-epp@sha256:ed57697703506b6db0232ee170dcee4bc72a8e5b724946ca39045d2a449f88b2`.
Its four affected package suites passed with Go's race detector. This records
the build identity; registry availability and live conformance are separate
requirements before a remote deployment.

The patch also gives exported trace resources the configured `OTEL_SERVICE_NAME`
alongside the existing `service.version`. An unset or empty name uses the
caller's default (`llm-d-epp` in the EPP). This lets collector and Tempo queries
identify the EPP segment of a request trace.

EPP propagation carries only W3C `traceparent`/`tracestate`. Backend header
mutations overwrite each canonical trace header once, remove stale trace
headers when their context is absent, and remove client baggage. Original
request headers cannot re-add old trace parents or baggage after injection.

Each configured picker instance advances through a stable lexical order of
namespace, pod name, address, serving port, and rank. Every eligible endpoint
gets one selection per cycle while the candidate set is stable. Concurrent
calls share a mutex; selection order does not guarantee request completion order.
When candidates change, the cursor resumes at the first eligible identity after
the last selection, wrapping as needed. It never sorts the caller's slice,
retains removed endpoints, or selects duplicate endpoint identities in a batch.

The cursor is local to one plugin instance in one EPP process. Different EPP
replicas and process restarts do not share an ordering guarantee. Separate
profiles should configure separate picker instances for independent cycles.
The default batch is one endpoint; `maxNumOfEndpoints` selects consecutive
distinct endpoints up to the available count.

## Reproduce the candidate

Use the exact archive recorded in `round-robin-picker.provenance.json`.
The source archive is deliberately outside the repository and is supplied as a
named build context. A complete source checksum, patch checksum, and checksums
of every patched file and unchanged module manifest are checked during the build.

With that archive available locally, prepare a directory containing only it:

```bash
mkdir -p /tmp/inferscale-llmd-build-source
cp /tmp/inferscale-llmd-v0.9.0.tar.gz /tmp/inferscale-llmd-build-source/llm-d-router-v0.9.0.tar.gz
```

Run from the InferScale repository. Use an existing resource-capped builder or
create a dedicated one once with the limits shown here. Run one image build at
a time on the local laptop. The Dockerfile also caps Go build parallelism and
Go-managed memory; Docker builder limits bound the whole process tree.

```bash
docker buildx create --name inferscale-llmd-limited --driver docker-container \
  --driver-opt cpu-period=100000,cpu-quota=200000,memory=4g,memory-swap=4g
docker buildx build --builder inferscale-llmd-limited \
  --build-context upstream=/tmp/inferscale-llmd-build-source \
  -f deploy/experimental/llm-d/Dockerfile.epp-local \
  -t ghcr.io/inferscale/llm-d-epp:v0.9.0-inferscale-round-robin --load .
```

The build checks Go formatting and runs the picker tests under Go's race
detector before producing the EPP binary. The tests cover order, candidate
reordering and churn, scores, empty pools, multiple selections, endpoint
identity, duplicate candidates, parameter validation, input immutability,
and exact distribution over concurrent calls. Tracing and header tests cover
service identity, sampled caller parent/state preservation, canonical header
replacement, missing-context cleanup, and baggage rejection. Package tests
establish these local contracts; live multi-worker routing and the full Gateway conformance suite
remain separate acceptance tests. See the validation records for their current
status; the source patch alone does not establish live conformance.

The source patch changes the new picker package, its registration, trace
resource identity, and trace header propagation with focused tests.
There are no module upgrades, Gateway changes, scorer changes, or inference
proxies in this candidate.
