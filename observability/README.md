# InferScale observability

InferScale sends OTLP traces to the OpenTelemetry Collector. Prometheus scrapes annotated platform pods, InferScale-managed Services, the collector, and the GPU telemetry installed on remote hosts. Kubernetes discovery stamps the actual Service name plus namespace, tenant, deployment, revision, and backend resource identity, so KEDA and rollout queries select exactly one revision. Grafana provisions Prometheus and Tempo plus six dashboards: overview, deployment, router, runtime, GPU, and rollout. The local Kustomize base carries deployment copies of these files so it remains self-contained.

Envoy Gateway, `inferscale-admission`, and vLLM propagate W3C trace identity.
The API emits separate management-request server spans. Admission replaces any
untrusted scheduling headers and forwards only the current
`traceparent`/`tracestate`; W3C baggage is deliberately not propagated. The
Gateway and runtime integrations remain subject to the live compatibility
gate in `tests/conformance/gateway`—a rendered manifest is not proof of
end-to-end trace continuity.

Privacy rules:

- Never attach prompt/completion text, authorization headers, API keys, raw model URIs, or request bodies to logs, spans, or metrics.
- Request IDs belong in logs and traces, never as metric labels.
- Tenant, deployment, revision, and backend are resource-identity labels bounded by admitted platform objects. Route, method, status class, priority, outcome, stage, state, and reason use closed enums.
- A missing Prometheus signal pauses rollout and holds autoscaling at a safe replica count; serving does not fail because telemetry is unavailable.

Gateway shadow mirrors bypass llm-d/EPP and target the candidate runtime Service. The `inferscale_shadow_runtime_*` recording series normalize the pinned vLLM completion, abort, TTFT, and inter-token metrics for that stage only. They are selected with bounded `namespace` and `service` identity; no request identifiers are labels. Candidate EPP telemetry and queue comparisons start at Canary5. A backend without the complete shadow runtime series is not promoted.

Validate JSON/YAML and Prometheus rules with `scripts/validate-observability.sh`.

## Hourly usage contract

The API recomputes the latest completed UTC hours every five minutes. llm-d EPP supplies the backend-neutral request, error, streaming latency, and token measurements: `inferscale_requests_total{outcome="total"}` is the request denominator, while `outcome="error"` is its subset and must not be added to the total. Input/output token histogram sums become the canonical monotonic `inferscale_tokens_total` series. GPU allocation comes from `inferscale_gpu_seconds_total`, with `billing_scope="tenant"` for stable serving allocation and `billing_scope="platform"` for candidate/shadow/build allocation. Every usage series must carry `tenant`, `deployment`, and `revision`.

`inferscale_gpu_seconds_total` counts allocated GPU-seconds, not utilization. The controller metrics collector integrates exclusive GPU limits only while a managed pod is scheduled to a node; it does not fabricate allocation from DCGM utilization. Controller restarts appear as ordinary Prometheus counter resets. If requests or tokens exist for an hour but that counter is absent, aggregation fails closed and preserves the previous database row. Re-running the lookback window is safe because PostgreSQL upserts the exact `(tenant, deployment, revision, hour)` value.
