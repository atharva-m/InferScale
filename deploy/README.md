# Kubernetes deployment

The base contains development PostgreSQL/Valkey, a schema-versioned migration Job, the three InferScale processes, Gateway API objects, the shared llm-d namespace/RBAC, and a self-contained Prometheus/Grafana/Tempo/OpenTelemetry stack. Each control-plane Deployment has an init gate that checks the current schema (`000005`, including benchmark leases, immutable benchmark execution snapshots, revision-attributed usage, and permanent tenant deployment-name identity), so it cannot start against an unmigrated database. Deployment-specific runtime workers, model-prefetch Jobs, EPP instances, InferencePools, routes, autoscalers, and network policies are rendered by the controller.

Install dependencies and render without changing the cluster:

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

Remote overlays terminate TLS with an `inferscale-tls` Secret in `inferscale-gateway`; create it before applying. They are benchmark environments on a single physical GPU node, not multi-cloud production templates. Only the `local-wsl` overlay contains a checked-in development storage Secret. Remote overlays deliberately render no `inferscale-storage` Secret and remain unavailable until an operator supplies externally managed PostgreSQL/Valkey credentials. Restrict dashboard access as well; anonymous Grafana access is appropriate only on an isolated development network.

The checked-in remote template also carries the invalid HTTP origin
`release-render-required.invalid`. `scripts/render-remote-release.sh` requires
`PUBLIC_BASE_URL` to be a credential-free HTTPS origin and replaces that
sentinel together with all control-plane image sentinels. Controller startup
rejects an empty/HTTP remote origin, so applying the template directly cannot
silently publish incorrect inference endpoints.

The externally managed remote `inferscale-storage` Secret must provide
`database_url`, `valkey_addr`, `benchmark_callback_signing_key`, and immutable
`@sha256:` values for `benchmark_runner_image`, `runtime_vllm_image`,
`runtime_trtllm_image`, and `modelcache_image`. The local overlay supplies
development tags only for images imported into k3d. External compatibility
versions used by scripts and manifests are recorded in
`docs/development/setup.md`.

`local-wsl` is also the only overlay that enables
`INFERSCALE_FAKE_RUNTIME=true`. Its `runtime_vllm_image` points to the CPU fake
server imported by `scripts/dev/up.sh`; remote overlays reject that image and
retain the real digest-pinned vLLM adapter. The fake remains an implementation
override under `backend: vllm`, not a CRD or API backend.
