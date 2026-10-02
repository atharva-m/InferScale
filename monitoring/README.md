# InferScale monitoring

Monitoring views for Qwen3-8B BF16 across 1, 2, 4, and 8 RTX 5090 GPUs.

[Open gallery](index.html)

## Configuration

| Item | Configuration |
| --- | --- |
| Model and precision | Qwen3-8B BF16 |
| GPU memory | RTX 5090, 32 GiB |
| GPU counts | 1, 2, 4, and 8 |
| Maximum serving context | 8,192 tokens |
| Replica baseline | TP=1 replicas |
| Tensor parallel comparison | TP=1/2/4/8 within a fixed 8-GPU budget |

| Workload | Input / output tokens |
| --- | --- |
| Interactive | 256 / 128 |
| Balanced | 2,048 / 256 |
| RAG | 6,144 / 256 |
| Agent | 4,096 / 512 |
| Long decode | 512 / 1,024 |

## SLOs, units, and accounting

The latency SLO counts successful requests meeting both TTFT ≤ 750 ms and
TPOT ≤ 50 ms, with a 95% attainment target. Availability targets 99.9% of
completed requests plus server rejections, excluding client cancellations.
Count-weighted 5-minute and 60-minute burn rates are reported separately.

Latency units are milliseconds. Fleet time uses elapsed hours; tenant and
rollout time use elapsed minutes. Repeat-run P95 values are means of run P95
values, not pooled percentiles. Rollout latency is the worst active user
revision P95. Shadow work is excluded from user goodput.

GPU cost uses a $1 per GPU-hour reference rate; energy covers GPU energy.
Input, output, and good-output unit costs use separate denominators, so the
full spend represented by those costs is not additive. Unit cost is undefined
when its output denominator is zero. Unavailable values remain gaps.

The fleet view spans 24 hours, tenant views span 180 minutes with dedicated
4/2/2-GPU pools, and six rollout scenarios span 30 minutes each.

## Operational events

| Elapsed hour | Event |
| ---: | --- |
| 3.5 | Worker loss and recovery |
| 6.5 | Valkey admission |
| 10 | Postgres and outbox |
| 13.5 | Prometheus metric staleness |
| 18 | KV pressure and restarts |

## Dashboards

| Dashboard | Files |
| --- | --- |
| Fleet overview | [PNG](./00-overview.png) · [SVG](./00-overview.svg) · [PDF](./00-overview.pdf) |
| Replica scaling | [PNG](./01-replica-scaling.png) · [SVG](./01-replica-scaling.svg) · [PDF](./01-replica-scaling.pdf) |
| Latency tails | [PNG](./02-latency-tails.png) · [SVG](./02-latency-tails.svg) · [PDF](./02-latency-tails.pdf) |
| Prefix caching | [PNG](./03-prefix-cache.png) · [SVG](./03-prefix-cache.svg) · [PDF](./03-prefix-cache.pdf) |
| Backend comparison | [PNG](./04-backends.png) · [SVG](./04-backends.svg) · [PDF](./04-backends.pdf) |
| Tensor parallelism | [PNG](./05-tensor-parallel.png) · [SVG](./05-tensor-parallel.svg) · [PDF](./05-tensor-parallel.pdf) |
| Workload profiles | [PNG](./06-workloads.png) · [SVG](./06-workloads.svg) · [PDF](./06-workloads.pdf) |
| SLO budgets | [PNG](./07-slo-budgets.png) · [SVG](./07-slo-budgets.svg) · [PDF](./07-slo-budgets.pdf) |
| Scheduler and KV cache | [PNG](./08-scheduler.png) · [SVG](./08-scheduler.svg) · [PDF](./08-scheduler.pdf) |
| Autoscaling | [PNG](./09-autoscaling.png) · [SVG](./09-autoscaling.svg) · [PDF](./09-autoscaling.pdf) |
| Startup and readiness | [PNG](./10-startup.png) · [SVG](./10-startup.svg) · [PDF](./10-startup.pdf) |
| GPU health | [PNG](./11-gpu-health.png) · [SVG](./11-gpu-health.svg) · [PDF](./11-gpu-health.pdf) |
| Request routing | [PNG](./12-routing.png) · [SVG](./12-routing.svg) · [PDF](./12-routing.pdf) |
| Tenant fairness | [PNG](./13-tenants.png) · [SVG](./13-tenants.svg) · [PDF](./13-tenants.pdf) |
| Request reliability | [PNG](./14-reliability.png) · [SVG](./14-reliability.svg) · [PDF](./14-reliability.pdf) |
| Control plane | [PNG](./15-control-plane.png) · [SVG](./15-control-plane.svg) · [PDF](./15-control-plane.pdf) |
| Cost and energy | [PNG](./16-cost-energy.png) · [SVG](./16-cost-energy.svg) · [PDF](./16-cost-energy.pdf) |
| Canary promotion and rollback | [PNG](./17-canary.png) · [SVG](./17-canary.svg) · [PDF](./17-canary.pdf) |
| Rollout strategies | [PNG](./18-rollout-strategies.png) · [SVG](./18-rollout-strategies.svg) · [PDF](./18-rollout-strategies.pdf) |
| Cache lifecycle | [PNG](./19-cache-lifecycle.png) · [SVG](./19-cache-lifecycle.svg) · [PDF](./19-cache-lifecycle.pdf) |

## Metric references

- [vLLM metrics](https://docs.vllm.ai/en/stable/design/metrics/)
- [vLLM automatic prefix caching](https://docs.vllm.ai/en/stable/features/automatic_prefix_caching/)
- [TensorRT-LLM executor](https://nvidia.github.io/TensorRT-LLM/_cpp_gen/executor.html)
- [NVIDIA GeForce RTX 5090](https://www.nvidia.com/en-us/geforce/graphics-cards/50-series/rtx-5090/)
- [Google SRE Workbook: Alerting on SLOs](https://sre.google/workbook/alerting-on-slos/)
