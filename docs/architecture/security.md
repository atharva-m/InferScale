# Security and privacy boundaries

## Tenant boundary

The gateway strips untrusted internal identity headers. Admission derives tenant identity from the API key, authorizes deployment ownership, atomically checks the tenant rate-limit bucket in Valkey, and attaches bounded tenant/fairness/priority metadata. llm-d owns request concurrency and queue limits, as recorded in ADR 0009. Request bodies cannot choose a tenant or namespace. Cross-tenant lookups return `404` to avoid enumeration.

Every tenant has a dedicated namespace and dedicated runtime/EPP pool. Runtime Services and InferencePool selectors include tenant, deployment, and revision labels. Default-deny policies allow only gateway-to-serving, monitoring scrapes, DNS, required telemetry, and model-registry egress. v1 never shares a runtime process, KV cache, or unsalted prefix cache across tenants.

Runtime and EPP metrics use HTTP without bearer authentication inside the cluster. Tenant NetworkPolicies restrict EPP metrics ingress to Prometheus Pods in the monitoring namespace; the public Gateway does not expose that port. InferScale explicitly disables the pinned EPP's optional Kubernetes metrics authentication and profiling endpoints, so scrapes use the same network boundary as runtime metrics without granting tenant EPP accounts cluster-wide token-review permissions. EPP inference remains TLS-enabled. Readiness uses EPP's separate gRPC health service, which checks pool synchronization and protocol compatibility instead of merely accepting a TCP connection.

## Credentials and untrusted input

- Store API-key prefix plus a constant-time-verifiable hash; reveal plaintext only at creation.
- Uncached authentication shares verification for identical credentials and permits at most two concurrent Argon2 checks per service instance. Excess uncached checks receive a retryable `503`; cached authenticated requests continue.
- Keep PostgreSQL, registry, and TLS credentials in Kubernetes Secrets or an external secret manager; never ConfigMaps, logs, traces, or benchmark JSON.
- Accept only `hf://owner/repository` model URIs and immutable 40-character commit SHAs in v1.
- Runtime images and release dependencies must be digest-pinned after M0 conformance.
- `trust_remote_code` and arbitrary container images/arguments are forbidden.
- Cache paths are derived from SHA-256 of canonical URI plus NUL plus lowercase revision. Workers mount completed entries read-only.

## Telemetry privacy

Prompt/completion text, request bodies, authorization headers, API keys, and raw model URIs are forbidden in telemetry. Request IDs are permitted in structured logs and spans but never metric labels; client values are accepted only as bounded opaque tokens and otherwise replaced. Metrics use bounded tenant/deployment/revision/backend/priority/outcome/reason labels. Shared Prometheus/Grafana is an operator surface and is not directly tenant-accessible in v1.

## Failure posture

Admission fails closed with a controlled retryable response when Valkey cannot safely check the tenant rate limit. Existing streams continue. A telemetry outage does not stop serving: rollout pauses and autoscaling holds a safe replica count. A warming candidate does not block admission to an existing stable revision. Cache corruption withdraws affected consumers until repair completes. A stable pool scaled to zero keeps its route and admission active so queued requests can wake workers. The public Gateway exposes liveness only; dependency readiness stays cluster-internal and returns bounded component states while logs identify only the failed component. Generic server errors never expose their underlying dependency text. TLS is mandatory outside the isolated local overlay.
