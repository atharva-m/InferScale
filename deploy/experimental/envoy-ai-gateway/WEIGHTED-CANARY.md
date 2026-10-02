# Weighted InferencePool qualification candidate

This candidate implements the missing Gateway translation needed by ADR 0011.
It consists of two small dependency patches: Envoy Gateway v1.8.1 and Envoy AI
Gateway v0.7.0. Both use the same Envoy protobuf dependency. The previous
`epp-filter-dedup` image and its recorded GPU evidence remain unchanged.

The code is implemented and reproducible. The provenance file records test and
live-validation status separately; a successful package test does not establish
live Gateway conformance. The production lock remains pending until the exact
candidate images pass the required suite.

The [candidate release renderer](CANDIDATE-RELEASE.md) provides a checksum-verified,
opt-in dependency bundle, scoped addon permissions, ordered installation and
remote endpoint-picker image selection. It keeps production qualification pending.

## What changed

Envoy Gateway previously returned early when translating a custom backend.
That discarded backend request-header filters, combined distinct pools in one
cluster, and passed every rule's pool resource to every cluster hook. The patch
preserves the backend metadata and filters, retains custom backends when forming
weighted clusters, and passes each cluster its own pool resource. Route-level
extension resources remain available to their hooks.

The AI Gateway patch maps each translated cluster to its pool by metadata. It
disables EPP filters at the route level and enables exactly one through the
selected weighted cluster's override. A native Envoy header-mutation filter
copies the selected backend's literal objective overwrite before EPP executes.
The filter order remains authorization, objective mutation, EPP, router.
Endpoint-picker responses retain the authorized route and selected backend in
the route cache. Upstream objective header behavior, weights, URL rewriting,
fractional Service mirroring, and native SSE remain in Envoy's existing path.
EPP filters and their upstream clusters are deduplicated across priority rules;
their configuration order is stable, and the message timeout is 900 seconds.
The addon also exposes `--secretNamespaces` so cluster-wide discovery of newly
created tenant pools can coexist with namespace-scoped Secret caching and RBAC.

This follows Envoy's existing [weighted-cluster filter configuration](https://www.envoyproxy.io/docs/envoy/v1.38.0/api-v3/config/route/v3/route_components.proto)
and [header-mutation filter](https://www.envoyproxy.io/docs/envoy/v1.38.0/api-v3/extensions/filters/http/header_mutation/v3/header_mutation.proto)
contracts. The source-level Gateway defects are in its
[custom backend translation](https://github.com/envoyproxy/gateway/blob/v1.8.1/internal/gatewayapi/route.go)
and [cluster hook setup](https://github.com/envoyproxy/gateway/blob/v1.8.1/internal/xds/translator/translator.go).

## Reproduce the sources

1. Obtain the official AI Gateway v0.7.0 source archive and verify its SHA-256
   against `weighted-canary.provenance.json`. Extract it into a new directory.
   The archive extraction must be pristine; do not apply the older dedup patch
   first because the new patch includes that correction.
2. Obtain `github.com/envoyproxy/gateway@v1.8.1` with `go mod download -json` and
   verify the module and go.mod sums against the provenance file. Use the returned
   `Dir` as the source directory. The Go module source and GitHub release archive
   have different file inventories; this candidate records the module source.
3. From the InferScale repository, run:

   ```bash
   python3 deploy/experimental/envoy-ai-gateway/prepare-canary-adapter.py \
     --ai-source /absolute/path/to/pristine/ai-gateway \
     --eg-source /absolute/path/to/gateway-module \
     --output /tmp/inferscale-canary-build
   ```

   The preparer validates complete input trees, patch and build-file checksums,
   exact patched trees, and unchanged module files. It creates new copies and
   leaves the original source directories intact. It downloads and builds nothing.

## Build and qualify locally

Use an existing BuildKit builder capped at two CPUs and 4 GiB RAM, with swap
disabled or capped to the same total. Run one build at a time and keep GPU
compilation/inference tests separate from builds on the laptop.

```bash
docker buildx build --builder inferscale-limited --load \
  -f deploy/experimental/envoy-ai-gateway/Dockerfile.gateway-canary \
  -t ghcr.io/inferscale/envoy-gateway:v1.8.1-inferscale-canary \
  /tmp/inferscale-canary-build/envoy-gateway

docker buildx build --builder inferscale-limited --load \
  -f deploy/experimental/envoy-ai-gateway/Dockerfile.controller-canary \
  -t ghcr.io/inferscale/ai-gateway-controller:v0.7.0-inferscale-canary \
  /tmp/inferscale-canary-build/ai-gateway
```

These are local build tags, not portable registry digests. For remote use,
publish the exact reviewed builds through the normal release process and record
their registry `repository@sha256:...` references together with build/test logs.
Do not substitute a Docker image configuration ID for the registry manifest
digest. Build and record the patched
[endpoint picker](../llm-d/README.md) as part of the same candidate.

Each build verifies module downloads, runs the affected upstream package suites,
and compiles with network access disabled after dependency preparation. The
regressions cover all priority rules, 95/5 weighted pools, 7% mirroring, literal
objective overwrite before EPP, unrelated-route isolation, repeated translation,
fail-closed full-duplex configuration, and the 900-second timeout.

Use the [candidate dependency workflow](CANDIDATE-RELEASE.md) to fetch and verify
all seven upstream artifacts, render the dependency bundle with those exact
three image digests, and review its configuration/RBAC/network policies. It
includes all required CRDs and webhook TLS bootstrap. Keep the generated private
TLS bundle out of source control. Pass `--context "$TARGET_KUBE_CONTEXT"` to the
staged installer, then set `INFERSCALE_CANDIDATE_DEPENDENCY_BUNDLE` for the remote
platform renderer so it uses the same endpoint-picker digest. The candidate
requires the patched addon image with `--secretNamespaces`; the older dedup-only
image does not implement this flag.

For an existing local k3d installation, first capture its Deployment images,
dependency ConfigMap and relevant manifests, and import the exact candidate
images. Preserve the existing external authorization and TLS service
configuration. Wait for both controller rollouts and inspect HTTPRoute status
and Envoy's accepted xDS before generating test traffic. Preserve the previous
private dependency bundle and platform manifest when using the staged workflow;
restoring only the Envoy Gateway image would leave the addon configuration and
picker at a different candidate revision.

Use the CPU fake-worker Gateway suite for healthy dual-revision canary, mirror,
authorization, native JSON/SSE, rejection under load, and cancellation. These
tests do not require additional physical GPUs. Run the real local GPU smoke
separately for runtime compatibility. Preserve raw results and exact image IDs;
only then update the release integration matrix and its live conformance status.
If qualification fails, restore the captured controller image references and
configuration, and keep the failed candidate evidence for diagnosis.
