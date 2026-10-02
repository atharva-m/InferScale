# Deployment revision lifecycle

1. Authenticate tenant and validate/canonicalize the requested spec.
2. Resolve a model reference to an immutable commit before revision creation.
3. Classify the patch. Serving-contract changes create a new spec hash/revision; policy-only changes update desired policy with optimistic concurrency.
4. For `backend:auto`, select only an exact compatible measured profile. Otherwise choose vLLM and mark the revision unbenchmarked.
5. Commit deployment revision and outbox operation atomically in PostgreSQL.
6. Idempotently apply the CRD. A retry with the same idempotency key and logical request returns the existing operation. PostgreSQL stores the original mutation fingerprint (including operation kind, tenant, target, full desired spec, expected generation, and explicit backend reselection where applicable), so later desired-state changes and Valkey response-cache expiry cannot make a different request look like a retry. A changed request receives `idempotency_conflict`. The PATCH API still requires its current optimistic `If-Match` precondition.

7. Controller verifies/prefetches weights, renders runtime and routing resources, and waits for observed readiness.
8. A candidate follows Ready -> Shadow -> Canary 5 -> 25 -> 50 -> 100 -> Stable. Missing/stale metrics or too few samples pause promotion.
9. A failed gate sets candidate weight to zero, restores stable to 100%, persists the evidence, and retains the failed candidate for inspection.
10. Deletion removes traffic first, then owned resources through a finalizer. Immutable revisions, usage, rollout history, and benchmark provenance remain; shared cache content is not deleted with the deployment.

Every controller transition is generation-aware and idempotent. Persist desired/observed weights and stage before promotion so a restart can resume or conservatively roll back.

Migration `000006` leaves historical operation fingerprints empty because the
original requests cannot be recovered reliably from mutable deployment rows.
Replaying one of those old keys without a surviving validated response-cache
entry fails closed with `idempotency_conflict`; inspect the existing operation
and use a new key for new work. Existing deployments, operations, and outbox
events continue to reconcile normally. The fingerprint is internal storage
metadata and is not exposed in operation API responses.
