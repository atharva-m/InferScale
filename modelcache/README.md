# Node-local model cache

The prefetch image accepts only `hf://owner/repository` and a 40-character immutable Hugging Face commit SHA. It serializes downloads with a POSIX filesystem lock, downloads into a unique partial directory, rejects symlinks, hashes every file, writes and fsyncs `.complete`, then atomically renames the entry to `sha256-<key>`. An existing incomplete, invalid, or fully verified corrupt target is atomically moved to `.quarantine/<key>-<timestamp>-<uuid>` with a diagnostic sidecar before replacement; evidence is never deleted by default.

Build and run:

```bash
docker build -t inferscale-modelcache:dev modelcache
docker run --rm \
  -v /var/lib/inferscale/models:/cache \
  -e HF_TOKEN \
  inferscale-modelcache:dev \
  --uri hf://Qwen/Qwen3-8B \
  --revision <40-character-commit-sha> --cache-root /cache \
  --full-verification
```

The successful termination message is bounded JSON containing `cache_hit`, `download_duration_s`, path, cache key/revision, tree checksum, and file count so it remains below Kubernetes' termination-message limit. The full per-file checksum manifest stays in the entry's `.complete` marker. The node directory must already exist and be writable by UID/GID `65532`; `scripts/bootstrap-gpu-host.sh` creates it. Runtime workers mount only the completed cache entry and use a read-only volume mount. The checked-in Job is a controller rendering reference, not a directly applied manifest: uppercase placeholders must be replaced from a validated deployment revision.

Quarantine retention is operator-controlled. `--quarantine-max-entries N` (or `INFERSCALE_QUARANTINE_MAX_ENTRIES`) keeps the newest `N` entries across the cache and deletes older evidence only when `N` is explicitly greater than zero. The default `0` disables automatic pruning. `scripts/clear-model-cache.sh` intentionally leaves `.quarantine` untouched.
