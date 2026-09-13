# Runbook: dependency outage

- **PostgreSQL:** mutation APIs return retryable `503` before Kubernetes side effects. Existing CRDs/workers continue. Reconciliation that needs profiles/history reports degraded state without inventing data.
- **Deployment synchronization:** the operation endpoint reports `retrying` with a bounded public error while the outbox retries a failed Kubernetes sync. Successful synchronization changes it to `succeeded` and clears the error. Detailed dependency errors remain in the internal outbox record.
- **Valkey:** new admission fails closed; already admitted streams continue and release leases best-effort. Reconcile expired leases after recovery.
- **Prometheus:** serving continues, rollout pauses, and autoscaling holds its last safe replica count rather than scaling to zero.
- **OpenTelemetry/Tempo/Grafana/DCGM:** serving continues and health reports telemetry degradation. Missing GPU telemetry makes benchmark profiles ineligible.
- **Kubernetes API/controller:** existing data-plane traffic continues. After recovery, generation-aware idempotent reconciliation repairs drift.
- **Gateway/EPP:** return controlled unavailable/overload responses; never bypass tenant admission or route to unready/cross-tenant endpoints.

For every outage, record start/recovery time and confirm there was no duplicate deployment revision, quota-counter underflow, unsafe promotion, or request replay.
