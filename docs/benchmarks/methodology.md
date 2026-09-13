# Benchmark methodology

Only remote runs on a documented physical Linux RTX 5090 host are publishable. Local RTX 4070 runs are correctness/development evidence.

## Reproducibility record

Every result must contain scenario YAML, Git commit and dirty state, runtime image digest/version, immutable model revision, GPU name/count, driver/CUDA/kernel, provider and hourly price, cache state, routing policy, timestamp, token counts, request-level latency values, aggregate percentiles, and Prometheus/DCGM snapshots. Never merge results whose compatibility keys differ.

Publishable backend-selection evidence uses the exact pinned tool identity
`guidellm-0.7.0`. Its `synthetic_text` generator is configured with zero
variance and equal min/target/max prompt and output token lengths; the runner
rejects observed per-request token counts that differ. Scheduled jobs never
fall back to another client. The `inferscale-native-http-v1` adapter remains
for local correctness and behavioral routing/fairness/overload scenarios, all
of which declare `publishable: false`. Tool identity is part of the scenario
digest and provenance, preventing evidence from the two adapters from mixing.
API-scheduled selection evidence is restricted to the process-warm state of a
Ready, stable-only deployment, and its declared routing policy must equal the
live deployment policy. Remote-cold and cache-warm timings are separate
operator lifecycle experiments rather than backend-selection profiles.

The backend profile key is model revision, backend and version/image digest, GPU SKU/count, precision/quantization, TP, and maximum-context bucket. Auto-selection removes measured SLO violators and chooses lowest measured cost among exact compatible survivors. No TensorRT-LLM interpolation or fabricated value is allowed.

## Run discipline

1. Verify all requested GPUs are allocatable on one Kubernetes node.
2. Record rental price and host/provider metadata.
3. Apply exactly one versioned scenario and wait for the deployment's current generation to become fully `Ready`. Benchmark creation is rejected unless there is exactly one stable revision and no candidate rollout.
4. Prepare `remote-cold`, `cache-warm`, or `process-warm` state explicitly; never combine them.
5. Send unmeasured warm-up traffic for process-warm experiments.
6. Run at least 500 measured requests for percentile comparisons unless the scenario is explicitly a smoke test.
7. Repeat comparison scenarios at least three times in alternating order. Report every run and aggregate confidence/variance; do not cherry-pick.
8. Capture P50/P95/P99 TTFT, TPOT, E2E and queue latency; token throughput; rejects/errors/SLO attainment; GPU use/memory/power; KV/prefix cache; startup phases; GPU-hours and separate input/output cost.
9. Generate tables/plots only from checked runner result JSON.

Each scheduled job receives a run-scoped inference token bound to the stable
revision recorded by the benchmark run. Admission rejects that token if a
candidate appears or the stable revision changes while the run is active, so
an update cannot silently mix stable and candidate traffic into one immutable
runtime profile. Ordinary tenant inference keys continue to follow the active
rollout policy.

TTFT starts immediately before the HTTP request and ends at the first non-empty streamed content delta. TPOT is `(last_content_time - first_content_time) / (completion_tokens - 1)` and is undefined for fewer than two output tokens. E2E ends when the stream closes. Server-reported usage is preferred; chunk count is a clearly imperfect smoke-test fallback.

Shadow traffic is excluded from customer token billing but its GPU usage is accounted as platform rollout cost. Publishable runs fail unless all required Prometheus queries are fresh, finite, and nonempty and the namespace/pod-selected DCGM inventory exactly matches the scenario's GPU count and SKU. Failed telemetry produces no eligible profile.
