# Controller/bootstrap templates

`tenant-baseline.yaml` is applied once when a tenant namespace is provisioned. It creates the namespace-local `inferscale-runtime` ServiceAccount with token automount disabled, a GPU-aware ResourceQuota, a LimitRange, and default-deny/explicit-path NetworkPolicies. Kubernetes does not permit a control-namespace ServiceAccount to be referenced from tenant pods.

`GPU_QUOTA` is the API-enforced steady-state tenant budget.
`GPU_RESOURCE_QUOTA` is rendered as twice that amount to reserve one
platform-controlled stable/candidate rollout surge; it is not additional
tenant-requestable capacity.

The bootstrap script validates and replaces the three uppercase placeholders.
The production tenant lifecycle renders the same resources idempotently from
durable tenant policy through `internal/tenant.NamespaceProvisioner`; this
template remains an operator/bootstrap reference and drift test fixture.
