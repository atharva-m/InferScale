# Metric contract

All durations are Prometheus histograms in seconds, counters end in `_total`, ratios are `0..1`, and token direction is the bounded `input|output` label.

| Owner | Metric family | Required bounded labels |
|---|---|---|
| Management API | `inferscale_api_http_requests_total`, `inferscale_api_http_request_duration_seconds`, `inferscale_outbox_pending`, `inferscale_usage_aggregation_*` | route, method, status class, outcome |
| Admission | `inferscale_admission_requests_total`, `inferscale_admission_request_duration_seconds`, `inferscale_tenant_throttles_total` | decision, bounded reason |
| Controller | `inferscale_deployment_ready`, `inferscale_deployment_replicas`, `inferscale_reconcile_duration_seconds`, `inferscale_rollout_*` | tenant, deployment, revision, backend, state/stage/role |
| Router/EPP | `inferscale_requests_total`, `inferscale_ttft_seconds`, `inferscale_tpot_seconds`, `inferscale_tokens_total`, `inferscale_router_*`, `inferscale_request_rejections_total` | tenant, deployment, revision, backend, total/error outcome, token direction, policy/reason |
| Shadow runtime | `inferscale_shadow_runtime_requests_total`, `inferscale_shadow_runtime_ttft_seconds`, `inferscale_shadow_runtime_tpot_seconds` | namespace, service, deployment, revision, backend, bounded total/error outcome |
| Runtime adapter | `inferscale_runtime_running_requests`, `inferscale_runtime_waiting_requests`, `inferscale_runtime_kv_cache_utilization_ratio`, native runtime metrics | tenant, deployment, revision, backend |
| Cache | `inferscale_model_download_duration_seconds`, `inferscale_model_cache_events_total` | model key, outcome/cache state |
| Rollout | `inferscale_rollout_stage`, `inferscale_rollout_transitions_total` | deployment, candidate revision, stage, outcome |
| Usage allocation source | `inferscale_gpu_seconds_total` | tenant, deployment, revision, billing scope (`tenant|platform`) |

Raw request IDs, pod-generated error text, model URIs, and user data are forbidden metric labels. Worker/pod is allowed only on router/DCGM operational metrics where its cardinality is bounded by cluster capacity.

Management and admission metrics never use tenant or deployment IDs. Runtime and controller resource identity is bounded by tenant quotas. Prometheus target relabeling supplies `service` from the Kubernetes Service name (or the pod application label for annotated control-plane pods); this is the contract used by KEDA and rollout queries.
