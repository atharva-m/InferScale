# Runbook: model cache failure

The controller verifies completed entries every five minutes. Once a worker's
physical node is known, revisions and tenants using the same cache path on that
node share a read-only `model-cache-verify-*` Job in `inferscale-system` (or the
configured control namespace). Before placement is known, and while consuming
existing verifier results during migration, verification Jobs remain in the
tenant namespace. Terminal evidence is retained until the controller consumes
it; a checksum failure must not disappear through Job TTL cleanup during an
outage.

On failure, a shared repair Lease elects one owner. It closes admission, holds
every worker using the affected entry, waits for their Pods to drain, then
quarantines and refetches the entry. Followers resume only after the owner
publishes the completed repair epoch. A failed verifier from that new epoch
starts another repair before held workers are restored.

Repair also precedes rollout abort, candidate retirement, and readiness
evaluation. If the desired model changes during repair, the owner finishes the
old entry using its immutable identity retained on the Lease before checking
the new desired entry. Older Leases recover this identity from the owner's
prefetch Job. A canary resumes from its readiness stage with fresh measurements
after repair; an outstanding operator abort is then applied.

1. Inspect the deployment `ModelCached` condition and prefetch Job termination message. It contains cache hit, download duration, key, revision, tree checksum, and file count, but no registry token. The full manifest is the cache entry's `.complete` marker.
2. Classify retryable registry/network failure versus terminal invalid URI/revision/checksum/disk error.
3. Confirm the target is `sha256-<digest>` derived from URI, NUL, lowercase revision; never operate on a user-provided path.
4. An incomplete directory has no `.complete` and must never be mounted by a worker. The next lock holder atomically moves that derived target into `.quarantine` and retries; inspect its JSON sidecar and retained files before any cleanup.
5. For corruption, enable full verification, stop every pod using that exact entry, and rerun prefetch so the corrupt target is quarantined before replacement. Do not delete the whole cache, another revision, or quarantine evidence during diagnosis.
6. For disk full, add capacity or remove only verified unused entries. Cache eviction is manual in v1.
7. Confirm full checksum verification and read-only worker mount before restoring route readiness. Automatic quarantine pruning is disabled by default; if disk policy requires a bound, explicitly set `INFERSCALE_QUARANTINE_MAX_ENTRIES` and document the evidence-retention value.
