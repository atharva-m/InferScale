# Kubernetes deployment

The [Vast.ai 4/8-GPU walkthrough](../docs/deployment/vast-5090.md) covers VM
selection, source/image transfer, credentials, first inference, and qualification.
Eight physical GPUs are supported as host capacity, with TP1/2/4 per worker.
All workers and model/engine cache Jobs must select the same physical host.

The base contains development PostgreSQL/Valkey, a schema-versioned migration Job, the three InferScale processes, Gateway API objects, the shared llm-d namespace/RBAC, and a self-contained Prometheus/Grafana/Tempo/OpenTelemetry stack. Each control-plane Deployment has an init gate that checks the current schema (`000006`, including benchmark leases, immutable benchmark execution snapshots, revision-attributed usage, permanent tenant deployment-name identity, and durable operation request fingerprints), so it cannot start against an unmigrated database. Deployment-specific runtime workers, model-prefetch Jobs, EPP instances, InferencePools, routes, autoscalers, and network policies are rendered by the controller.

Install the locked baseline dependencies, render locally, then apply explicitly:

```bash
scripts/install-platform-dependencies.sh
kubectl kustomize deploy/overlays/local-wsl >/tmp/inferscale-local.yaml
kubectl apply --server-side --field-manager=inferscale-bootstrap -k deploy/overlays/local-wsl
```

The dependency installer reads exact URLs, artifact SHA-256 values, and image
digests from `versions.lock.yaml`. It downloads and verifies the entire set
before the first `kubectl apply`, rewrites the checksummed Envoy Gateway and
KEDA manifests to digest-only workload images, and rejects any remaining
tag-only image. This includes Envoy Gateway's controller, certificate Job,
generated shutdown-manager and rate-limit settings, plus the KEDA operator and
metrics API server. The locked KEDA core artifact does not install an admission
webhook workload; if a future artifact introduces one, installation fails until
its image is explicitly locked. The `EnvoyProxy` also pins the separately
generated Envoy data-plane container by digest.

The installer explicitly selects Envoy Gateway's `GatewayNamespace` deployment
mode so proxy Pods run beside the Gateway in `inferscale-gateway`, as required
by the control-plane and tenant NetworkPolicies. Additional infrastructure write
access is scoped to that namespace; a separate cluster permission allows only creation of
TokenReviews for proxy-to-controller authentication. The controller still
watches tenant routes across namespaces. When upgrading an existing default-mode
installation, restart `deployment/envoy-gateway` in `envoy-gateway-system` after
applying the installer changes, then verify the replacement proxy is Ready in
`inferscale-gateway` before removing any obsolete proxy resources in the old
namespace.

Remote overlays terminate TLS with an `inferscale-tls` Secret in `inferscale-gateway`; create it before applying. They are qualification/benchmark environments on a single physical GPU node, not multi-cloud production templates. Only the `local-wsl` overlay contains a checked-in development storage Secret. Remote overlays deliberately render no `inferscale-storage` Secret and remain unavailable until an operator supplies PostgreSQL/Valkey credentials. Remote Grafana disables anonymous access and requires an `inferscale-monitoring/grafana-admin` Secret with `username` and `password`; keep dashboard access private.

The checked-in remote template also carries the invalid HTTP origin
`release-render-required.invalid`. `scripts/render-remote-release.sh` requires
`PUBLIC_BASE_URL` to be a credential-free HTTPS origin and replaces that
sentinel together with all control-plane image sentinels. Controller startup
rejects an empty/HTTP remote origin, so applying the template directly cannot
silently publish incorrect inference endpoints.

The externally managed remote `inferscale-storage` Secret must provide
`database_url`, either `valkey_url` or `valkey_addr`, `benchmark_callback_signing_key`, and immutable
`@sha256:` values for `benchmark_runner_image`, `runtime_vllm_image`,
`runtime_trtllm_image`, and `modelcache_image`. The local overlay supplies
development tags only for images imported into k3d. External compatibility
versions used by scripts and manifests are recorded in
`docs/development/setup.md`.

Use `valkey_url=rediss://USER:PASSWORD@HOST:PORT/DB` for authenticated,
certificate-verified TLS to a managed single-primary Valkey endpoint. It takes
precedence over `valkey_addr`; URL-encode credentials and do not put it in a
ConfigMap. Redis Cluster/Sentinel discovery is not supported by this client.

`scripts/render-remote-release.sh vast-8x5090` renders the eight-GPU profile.
Every Vast render requires `INFERSCALE_GPU_NODE_NAME`, which is the selected
node's **`kubernetes.io/hostname` label value** (not necessarily its object name).
The renderer pins runtime/model cache selectors to that host and sets the TLS
listener hostname from `PUBLIC_BASE_URL`.

Set `REMOTE_EXTERNAL_STORAGE=true` to omit the bundled PostgreSQL/Valkey
workloads, Services, and their storage-only NetworkPolicy. This only changes
new output; it does not migrate or remove live data. Set
`CLUSTER_POSTGRES_CIDRS` and `CLUSTER_VALKEY_CIDRS` to JSON arrays of private
destination CIDRs, with optional `CLUSTER_POSTGRES_PORT` (5432) and
`CLUSTER_VALKEY_PORT` (6379). For k3s API egress after service translation, set
`CLUSTER_KUBERNETES_API_CIDRS` to exact private `/32` or `/128` endpoints and
`CLUSTER_KUBERNETES_API_PORT` (6443). The latter values also populate
`INFERSCALE_KUBERNETES_API_CIDRS`/`INFERSCALE_KUBERNETES_API_PORT`; pass those same
settings to `inferscalectl tenant create`/`tenant provision` so new and existing
tenant EPP policies allow the intended endpoint. Non-default API ports without
explicit endpoint addresses are rejected.

None of these renderer settings bypasses the live Gateway conformance gate.
The pinned stock Gateway integration is still an unresolved release prerequisite.

For the patched weighted-InferencePool qualification candidate, follow the
[candidate dependency workflow](experimental/envoy-ai-gateway/CANDIDATE-RELEASE.md).
It downloads only recorded sources in a separate acquisition step, then renders
an offline, checksum-verified bundle from operator-supplied Envoy Gateway, AI
Gateway and endpoint-picker image digests. The bundle contains private webhook
TLS keys and must be kept private. Apply the reviewed bundle with an explicit
cluster context:

```bash
scripts/install-platform-dependencies.sh \
  --candidate-bundle /absolute/path/to/reviewed-candidate-bundle \
  --context "$TARGET_KUBE_CONTEXT"
export INFERSCALE_CANDIDATE_DEPENDENCY_BUNDLE=/absolute/path/to/reviewed-candidate-bundle
scripts/render-remote-release.sh vast-4x5090 > /absolute/path/to/platform-candidate.yaml
```

The staged installer waits for CRDs and the addon before starting Envoy Gateway.
The addon discovers new tenant InferencePools automatically while Secret access
stays limited to its namespace and the Gateway namespace. The platform renderer
uses the picker digest recorded in the same bundle. Preserve previous dependency
and platform manifests for rollback; exact-image live conformance remains
pending until the candidate actually passes the required suite. This opt-in path
does not change the production lock or its release gate.

For private application images, set `INFERSCALE_IMAGE_PULL_SECRET` when rendering.
The renderer adds it to system workloads and controller configuration. Create
that named pull Secret in the system, shared-cache, and every tenant namespace;
benchmark Jobs additionally use the tenant default ServiceAccount's pull Secrets.

`local-wsl` is also the only overlay that enables
`INFERSCALE_FAKE_RUNTIME=true`. Its `runtime_vllm_image` points to the CPU fake
server imported by `scripts/dev/up.sh`; remote overlays reject that image and
retain the real digest-pinned vLLM adapter. The fake remains an implementation
override under `backend: vllm`, not a CRD or API backend.
