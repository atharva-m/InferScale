# Deployment revision lifecycle

1. Authenticate tenant and validate/canonicalize the requested spec.
2. Resolve a model reference to an immutable commit before revision creation.
3. Classify the patch. Serving-contract changes create a new spec hash/revision; policy-only changes update desired policy with optimistic concurrency.
4. For `backend:auto`, select only an exact compatible measured profile. Otherwise choose vLLM and mark the revision unbenchmarked.
5. Commit deployment revision and outbox operation atomically in PostgreSQL.
6. Idempotently apply the CRD. A retry with the same idempotency key returns the existing operation.
7. Controller verifies/prefetches weights, renders runtime and routing resources, and waits for observed readiness.
8. A candidate follows Ready -> Shadow -> Canary 5 -> 25 -> 50 -> 100 -> Stable. Missing/stale metrics or too few samples pause promotion.
9. A failed gate sets candidate weight to zero, restores stable to 100%, persists the evidence, and retains the failed candidate for inspection.
10. Deletion removes traffic first, then owned resources through a finalizer. Immutable revisions, usage, rollout history, and benchmark provenance remain; shared cache content is not deleted with the deployment.

Every controller transition is generation-aware and idempotent. Persist desired/observed weights and stage before promotion so a restart can resume or conservatively roll back.
