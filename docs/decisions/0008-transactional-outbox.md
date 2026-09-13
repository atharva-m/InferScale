# ADR 0008: Bridge PostgreSQL and CRDs with a transactional outbox

## Context

Deployment creation must persist public identity/revision history and apply a Kubernetes CRD. PostgreSQL and Kubernetes cannot commit atomically, so direct dual writes produce orphan rows/resources after crashes.

## Decision

PostgreSQL owns tenant/key identities, public deployment IDs, immutable revision/audit data, and an operation outbox. The CRD owns live Kubernetes desired state/status. API mutations transactionally insert revision plus operation, return `202`, and an idempotent dispatcher server-side-applies the CRD. Operation state exposes retries/errors. Status synchronization is generation-aware.

## Alternatives considered

- CRD as the only database: rejected for benchmark/usage/key/query requirements.
- PostgreSQL-only controller polling: rejected because it abandons Kubernetes-native declarative state.
- Synchronous DB then CRD write: rejected due unavoidable partial failure.

## Consequences

The API is eventually consistent and requires idempotency/operation status. Duplicate applies are harmless; reconciliation can repair either side after restart.
