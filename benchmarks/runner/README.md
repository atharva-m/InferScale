# InferScale benchmark runner

The runner validates a versioned YAML scenario and dispatches one of two pinned
measurement adapters. Publishable selection scenarios use GuideLLM 0.7.0 with
exact `synthetic_text` prompt/output lengths. Behavioral scenarios use the
native streaming client and are always non-publishable. Both produce the same
redacted normalized result schema.

```bash
python -m venv .venv
. .venv/bin/activate
pip install -e 'benchmarks/runner[test,analysis]'
inferscale-bench validate benchmarks/scenarios/*.yaml
inferscale-bench run --scenario benchmarks/scenarios/local-smoke.yaml --allow-unauthenticated
```

Build the non-root runner image through `scripts/build-release-images.sh` with
`PYTHON_BASE_IMAGE`, `BENCHMARK_IMAGE`, and the other release image variables
set to immutable digest references. The Dockerfile installs the exact
`guidellm[recommended]==0.7.0` dependency and the release script embeds the
full source commit. Publish the resulting image and configure the scheduler
with its registry digest; publishable runs reject a missing runner digest.

Set `INFERSCALE_API_KEY` for the platform API. A scenario with `target.api_mode: openai` uses the direct `/v1/chat/completions` endpoint and its `deployment` value as the served model name; it is intended for the local baseline. `--allow-unauthenticated` is an explicit override for another isolated test endpoint. Raw prompts and generated content are intentionally excluded from result files.

The control-plane scheduler runs the same CLI as:

```bash
inferscale-bench run --run-id <uuid> --scenario /config/scenario.json \
  --deployment-id <deployment-uuid> \
  --provider <operator-configured-provider> \
  --runtime-version <frozen-version> \
  --container-image-digest sha256:<frozen-runtime-digest> \
  --runner-image-digest sha256:<runner-digest> \
  --driver-version <verified-driver> --cuda-version <verified-cuda> \
  --prometheus-url http://prometheus:9090 \
  --deployment-namespace <tenant-namespace> \
  --runtime-pod-prefix <revision-runtime-pod-prefix> \
  --epp-service <revision-epp-service> \
  --configuration-digest <server-authored-scenario-sha256> \
  --selection-scenario-digest <server-authored-selection-sha256> \
  --callback http://inferscale-api.inferscale-system.svc.cluster.local:8080/internal/v1/benchmarks/<uuid>/result
```

It injects `INFERSCALE_BENCHMARK_TOKEN`, a signed run-scoped bearer accepted for both inference and the callback only while that run is active. Remote configuration metadata must include the measured provider, GPU-hour price, NVIDIA driver version, CUDA compatibility version, and immutable runtime image digest; missing evidence fails closed before traffic is generated. Scheduled mode never persists the token, response bodies, prompts, or generated text. A terminal failure is redacted and limited to 1,024 characters before it is posted.

Scheduled runs require `measurement_tool: guidellm-0.7.0`,
`publishable: true`, and `workload.dataset: synthetic`; there is no silent
fallback to the native client. The GuideLLM bearer token is written only to a
mode-0600 temporary YAML config, is absent from the child argv/environment,
and the config plus raw GuideLLM report are deleted after strict schema and
token-contract validation. The adapter identity is part of the selection
scenario digest, so native-client and GuideLLM profiles cannot be mixed.

The API scheduler creates reusable selection profiles only from a stable-only,
Ready deployment. Its scenario must declare `cache_state: process-warm` and
the deployment's current routing policy. The server verifies both fields and a
run-scoped token stops authorizing inference if a candidate appears or the
stable revision or routing policy changes. Cold/cache-warm lifecycle
experiments remain explicit operator runs and are never mislabeled as reusable
backend-selection evidence.

A scheduled success also requires every allowlisted Prometheus query to return
fresh finite data. Runtime/DCGM queries are selected by namespace and exact
runtime-pod prefix, while router queries are selected by namespace and the
frozen revision EPP service. The selected DCGM inventory must contain exactly
the scenario GPU count and SKU. The artifact records `measurement_interval`
with `start_unix_s` and `end_unix_s` from GuideLLM's measured benchmark interval
(or the native client's request interval). Prometheus queries evaluate at that
end timestamp: gauges average samples within the interval, and queue latency
uses histogram increases over the same interval. Startup and result collection
delays therefore do not enter the measurement. Runs need enough scrapes during
the interval to produce each required metric. Missing telemetry,
GuideLLM schema drift, a token-length mismatch, or any request failure makes
the run fail closed. Normalized artifacts never retain generated output,
prompts, raw request arguments, or GuideLLM request IDs.
