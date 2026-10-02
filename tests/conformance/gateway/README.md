# Gateway and llm-d conformance gate

`go test ./tests/conformance/gateway/...` is the hardware-independent half of
the gate. It verifies that InferScale renders the pinned stable `InferencePool`
API, fail-close EPP reference, flow-control/plugin configuration, weighted
revision pools, objective header filters, URL rewrite, fractional Service
mirror, and fail-closed 1 MiB Envoy external-authorization policy.

Static object inspection is not proof that the pinned Envoy Gateway and llm-d
versions implement those fields. Before a release, deploy the exact digests in
`versions.lock.yaml` to a disposable cluster and capture evidence for every
entry in `conformance.requiredFeatures`:

`scripts/e2e/live-gateway-conformance.sh` now provides the executable CPU-only
weighted-backend/mirror/auth-spoof/SSE/cancellation/backpressure experiments.
It creates isolated test routes and fake revision pools against an existing
authorized deployment; see `scripts/e2e/README.md` for setup, limits, cleanup,
and the remaining evidence. Set `INFERSCALE_CONFORMANCE_REQUESTS=1000` for
release-size traffic samples. Its artifact intentionally remains partial live
evidence, not the completed release-gate document described below.

`scripts/e2e/live-trace-conformance.sh` supplies a separate redacted summary
for the successful non-streaming vLLM trace case in item 7. It requires
existing loopback Gateway/Tempo forwards and verifies parent linkage through
all four services, plus exported-data privacy during a bounded settle window.
See `scripts/e2e/README.md` for credentials, exact service identities, and
coverage limits. Its result is partial evidence with `release_signoff:false`;
streaming/error paths and exact image identities still require qualification.

1. Show an invalid/revoked key is rejected before EPP and an ext-auth/Valkey
   outage returns 503 for new requests.
2. Stream a native completion through EPP, observe incremental SSE and
   `[DONE]`, cancel a second stream, and show the runtime sees cancellation.
3. Prove the public UUID path reaches an `InferencePool` after rewriting to
   `/v1/chat/completions`.
4. Send client-spoofed `x-llm-d-*` headers and show EPP receives only the
   admission-injected fairness, objective, model, and SLO values.
5. Generate at least 1,000 requests at each canary weight and record the
   stable/candidate request counts and statistical tolerance used.
6. Enable a non-zero shadow percentage and show candidate Service request
   counts increase while client responses remain from stable.
7. Start a caller span and prove the same trace is correlated across Gateway,
   admission, EPP, and the selected runtime. Inspect the exported spans and
   prove prompts, completions, bearer values, and request bodies are absent.
8. Record Gateway/HTTPRoute accepted/programmed conditions, component image
   IDs, CRD versions, test commands, Prometheus snapshots, and raw test output.

Create `tests/conformance/gateway/evidence.json` conforming to
`evidence.schema.json`. Its `versionsLockSHA256` is the SHA-256 of the complete
lock file after `conformance.status` is set to `verified`. Evidence is valid for
30 days. The matrix entries for Envoy Gateway, Gateway API, Inference
Extension, llm-d/EPP, KEDA, vLLM, and TensorRT-LLM must exactly match the
version-and-digest identities derived by the gate from the lock file. Generated
evidence is intentionally gitignored unless the project chooses to publish it
separately.

Run the full blocking gate with:

```bash
make release-check
```

The command fails while conformance is `pending`, when evidence is absent or
stale, when a required feature did not pass, or when the lock changed after the
test run. There is no custom-proxy fallback.
