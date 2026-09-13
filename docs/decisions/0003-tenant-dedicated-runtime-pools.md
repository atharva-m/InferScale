# ADR 0003: Use tenant-dedicated runtime pools

## Context

Sharing model processes or KV/prefix cache between untrusted tenants complicates isolation, attribution, fairness, cache privacy, and incident analysis.

## Decision

Use a multi-tenant cluster/control plane with one namespace and dedicated runtime/EPP pools per tenant deployment. Tenant namespaces receive a local tokenless `inferscale-runtime` ServiceAccount, ResourceQuota, LimitRange, and default-deny policies. No cross-tenant process, KV cache, or prefix-cache reuse is allowed in v1.

PostgreSQL enforces each tenant's configured GPU limit as the steady-state
allocation. Kubernetes receives twice that GPU limit: the additional half is a
platform-controlled surge reserve so one immutable candidate or TensorRT engine
build can coexist with the stable revision. Tenant users cannot submit arbitrary
images or workloads, and retired candidates are scaled to zero before their
resources are removed, so this reserve is not general tenant capacity.

## Alternatives considered

- Shared model server with logical tenant IDs: rejected for v1 isolation risk.
- Cluster per tenant: rejected as operationally excessive for the project scope.

## Consequences

GPU utilization can be lower than aggressive sharing, and popular models may be loaded repeatedly. Isolation and per-tenant cost/usage reasoning remain simple and testable.
