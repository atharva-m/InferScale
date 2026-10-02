# Controller/bootstrap templates

`tenant-baseline.yaml` is applied once when a tenant namespace is provisioned. It creates the namespace-local `inferscale-runtime` ServiceAccount with token automount disabled, a GPU-aware ResourceQuota, a LimitRange, and default-deny/explicit-path NetworkPolicies. Kubernetes does not permit a control-namespace ServiceAccount to be referenced from tenant pods.

`GPU_QUOTA` is the API-enforced steady-state tenant budget.
`GPU_RESOURCE_QUOTA` is rendered as twice that amount to reserve one
platform-controlled stable/candidate rollout surge; it is not additional
tenant-requestable capacity.

The bootstrap script validates and replaces the four uppercase placeholders.
The production tenant lifecycle renders the same resources idempotently from
durable tenant policy through `internal/tenant.NamespaceProvisioner`; this
template remains an operator/bootstrap reference and drift test fixture.

The LimitRange requires at least 10m CPU and 16Mi memory per container and
supplies requests of 100m CPU and 128Mi memory when omitted. It sets no blanket
CPU/memory limit: Kubernetes would derive default limits from container maxima,
reserving excessive resources for small EPP containers, while small defaults
could kill model workers and engine builds. Explicit workload limits are left
intact, and the GPU ResourceQuota remains the tenant allocation boundary.
Prometheus ingress is restricted to TCP ports 8000 (vLLM), 9000 (TensorRT-LLM
exporter), and 9090 (EPP).

Tenant provisioning runs in `inferscalectl` with the operator's Kubernetes
credentials. Those credentials must allow server-side apply (create/patch) of
namespaces, serviceaccounts, resourcequotas, limitranges, and networkpolicies.
The API and deployment controller do not provision namespaces and do not need
additional LimitRange permissions. For remote API endpoints that use a port
other than 443, provision with `inferscalectl` and the explicit API endpoint
CIDR/port settings; this reference template uses the portable TCP/443 fallback.

When upgrading a namespace provisioned by the older bootstrap template, apply
the new baseline first, then remove its obsolete `allow-platform-paths`
NetworkPolicy and `inferscale-tenant-quota` ResourceQuota. NetworkPolicy allows
are additive, so retaining the old broad monitoring rule would defeat the new
Prometheus-only restriction. The new quota is named `inferscale-tenant`, matching
the operator provisioner. Review any operator customizations before removing
those old objects.
