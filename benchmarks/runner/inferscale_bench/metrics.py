from __future__ import annotations

import json
import math
import re
import urllib.parse
import urllib.request
from collections.abc import Iterable
from dataclasses import asdict

from .client import MeasurementInterval, RequestResult
from .scenario import SLO


def percentile(values: Iterable[float], quantile: float) -> float | None:
    ordered = sorted(float(value) for value in values)
    if not ordered:
        return None
    if quantile < 0 or quantile > 1:
        raise ValueError("quantile must be between zero and one")
    rank = max(0, math.ceil(quantile * len(ordered)) - 1)
    return ordered[rank]


def _distribution(values: Iterable[float]) -> dict[str, float | None]:
    materialized = list(values)
    return {
        "p50": percentile(materialized, 0.50),
        "p95": percentile(materialized, 0.95),
        "p99": percentile(materialized, 0.99),
    }


def aggregate(
    results: list[RequestResult], wall_time_s: float, slo: SLO
) -> dict[str, object]:
    successful = [result for result in results if result.ok]
    prompt_tokens = sum(result.prompt_tokens for result in successful)
    output_tokens = sum(result.output_tokens for result in successful)
    slo_eligible = [
        result
        for result in successful
        if (slo.ttft_p95_ms is None or (result.ttft_ms or math.inf) <= slo.ttft_p95_ms)
        and (slo.tpot_p95_ms is None or (result.tpot_ms or math.inf) <= slo.tpot_p95_ms)
    ]
    return {
        "requests": len(results),
        "successful_requests": len(successful),
        "failed_requests": len(results) - len(successful),
        "request_failure_rate": (len(results) - len(successful)) / len(results)
        if results
        else 0,
        "wall_time_s": wall_time_s,
        "ttft_ms": _distribution(
            result.ttft_ms for result in successful if result.ttft_ms is not None
        ),
        "tpot_ms": _distribution(
            result.tpot_ms for result in successful if result.tpot_ms is not None
        ),
        "e2e_ms": _distribution(result.e2e_ms for result in successful),
        "prompt_tokens": prompt_tokens,
        "output_tokens": output_tokens,
        "input_tokens_per_s": prompt_tokens / wall_time_s if wall_time_s else None,
        "output_tokens_per_s": output_tokens / wall_time_s if wall_time_s else None,
        "total_tokens_per_s": (prompt_tokens + output_tokens) / wall_time_s
        if wall_time_s
        else None,
        "slo_attainment": len(slo_eligible) / len(successful) if successful else 0,
        "errors": [asdict(result) for result in results if not result.ok],
    }


PUBLISHABLE_REQUIRED_QUERIES = frozenset(
    {
        "gpu_utilization",
        "gpu_memory_used_bytes",
        "gpu_power_watts",
        "waiting_requests",
        "running_requests",
        "kv_cache_utilization",
        "queue_p95_seconds",
    }
)


def build_prometheus_queries(
    deployment_namespace: str,
    runtime_pod_prefix: str,
    epp_service: str,
    interval: MeasurementInterval,
) -> dict[str, str]:
    """Aggregate the scheduled revision's samples from the measured interval."""

    dns_label = re.compile(r"[a-z0-9](?:[-a-z0-9]*[a-z0-9])?")
    identities = {
        "deployment namespace": deployment_namespace,
        "runtime pod prefix": runtime_pod_prefix,
        "EPP service": epp_service,
    }
    for name, value in identities.items():
        if len(value) > 63 or not dns_label.fullmatch(value):
            raise ValueError(f"invalid Kubernetes {name} for telemetry query")
    runtime_selector = (
        f'namespace="{deployment_namespace}",pod=~"^{runtime_pod_prefix}-.*$"'
    )
    epp_selector = f'namespace="{deployment_namespace}",service="{epp_service}"'
    # PromQL durations have millisecond resolution. Round inward so a range
    # selector never includes a sample preceding the measured interval.
    duration_ms = math.floor((interval.end - interval.start) * 1000)
    if duration_ms < 1:
        raise ValueError("measurement interval is too short for Prometheus")
    window = f"{duration_ms}ms"

    def fresh(expression: str, source_metric: str, selector: str) -> str:
        # Prometheus stamps an instant-query result with evaluation time, even
        # when the selected source sample is older. Gate the aggregate on the
        # source timestamp itself so the runner's later timestamp check cannot
        # mistake a lookback-restored value for fresh telemetry.
        source = f"{source_metric}{{{selector}}}"
        return f"({expression}) and on() ((time() - min(timestamp({source}))) < 60)"

    def average(source_metric: str, selector: str) -> str:
        return f"avg_over_time({source_metric}{{{selector}}}[{window}])"

    return {
        "gpu_utilization": fresh(
            f"avg({average('DCGM_FI_DEV_GPU_UTIL', runtime_selector)})",
            "DCGM_FI_DEV_GPU_UTIL",
            runtime_selector,
        ),
        "gpu_memory_used_bytes": fresh(
            f"sum({average('DCGM_FI_DEV_FB_USED', runtime_selector)}) * 1024 * 1024",
            "DCGM_FI_DEV_FB_USED",
            runtime_selector,
        ),
        "gpu_power_watts": fresh(
            f"sum({average('DCGM_FI_DEV_POWER_USAGE', runtime_selector)})",
            "DCGM_FI_DEV_POWER_USAGE",
            runtime_selector,
        ),
        "waiting_requests": fresh(
            f"sum({average('inferscale_runtime_waiting_requests', runtime_selector)})",
            "inferscale_runtime_waiting_requests",
            runtime_selector,
        ),
        "running_requests": fresh(
            f"sum({average('inferscale_runtime_running_requests', runtime_selector)})",
            "inferscale_runtime_running_requests",
            runtime_selector,
        ),
        "kv_cache_utilization": fresh(
            f"avg({average('inferscale_runtime_kv_cache_utilization_ratio', runtime_selector)})",
            "inferscale_runtime_kv_cache_utilization_ratio",
            runtime_selector,
        ),
        "queue_p95_seconds": fresh(
            "histogram_quantile(0.95, sum by (le) (increase("
            f"inferscale_router_flow_control_wait_seconds_bucket{{{epp_selector}}}[{window}]"
            ")))",
            "inferscale_router_flow_control_wait_seconds_bucket",
            epp_selector,
        ),
        "gpu_inventory": fresh(
            "count by (UUID, modelName, namespace, pod) (count_over_time("
            f"DCGM_FI_DEV_GPU_UTIL{{{runtime_selector}}}[{window}]))",
            "DCGM_FI_DEV_GPU_UTIL",
            runtime_selector,
        ),
    }


def scrape_prometheus(
    base_url: str | None,
    interval: MeasurementInterval,
    timeout_s: float = 10.0,
    deployment_namespace: str | None = None,
    runtime_pod_prefix: str | None = None,
    epp_service: str | None = None,
) -> dict[str, object]:
    if not base_url:
        return {}
    snapshots: dict[str, object] = {}
    if not deployment_namespace or not runtime_pod_prefix or not epp_service:
        raise ValueError(
            "Prometheus collection requires deployment namespace, runtime pod "
            "prefix, and EPP service selectors"
        )
    queries = build_prometheus_queries(
        deployment_namespace, runtime_pod_prefix, epp_service, interval
    )
    for name, query in queries.items():
        parameters = urllib.parse.urlencode({"query": query, "time": interval.end})
        url = f"{base_url.rstrip('/')}/api/v1/query?{parameters}"
        try:
            with urllib.request.urlopen(url, timeout=timeout_s) as response:
                payload = json.load(response)
            snapshots[name] = payload.get("data", {}).get("result", [])
        except Exception as exc:  # noqa: BLE001 - telemetry failure is recorded in the artifact
            snapshots[name] = {"error": f"{type(exc).__name__}: {exc}"}
    return snapshots


def _gpu_sku_matches(expected: str, observed: str) -> bool:
    def normalize(value: str) -> str:
        return re.sub(r"[^a-z0-9]", "", value.lower())

    expected_value = normalize(expected)
    observed_value = normalize(observed)
    return bool(expected_value) and (
        expected_value == observed_value or expected_value in observed_value
    )


def validate_publishable_telemetry(
    snapshot: dict[str, object],
    expected_gpu_count: int,
    expected_gpu_sku: str,
    interval: MeasurementInterval,
) -> dict[str, object]:
    """Reject missing, stale, or ambiguous remote benchmark telemetry."""

    for name in sorted(PUBLISHABLE_REQUIRED_QUERIES | {"gpu_inventory"}):
        series = snapshot.get(name)
        if not isinstance(series, list) or not series:
            raise ValueError(f"publishable benchmark telemetry is missing {name}")
        for sample in series:
            if not isinstance(sample, dict):
                raise TypeError(f"publishable benchmark telemetry {name} is malformed")
            value = sample.get("value")
            if not isinstance(value, list) or len(value) != 2:
                raise ValueError(f"publishable benchmark telemetry {name} has no value")
            timestamp = float(value[0])
            measured = float(value[1])
            if (
                not math.isfinite(measured)
                or not math.isfinite(timestamp)
                or abs(timestamp - interval.end) > 0.001
            ):
                raise ValueError(
                    f"publishable benchmark telemetry {name} is stale or non-finite"
                )
    inventory = snapshot.get("gpu_inventory")
    if not isinstance(inventory, list) or len(inventory) != expected_gpu_count:
        raise ValueError(
            "observed GPU inventory does not match deployment accelerator count"
        )
    observed: list[dict[str, str]] = []
    seen: set[str] = set()
    for sample in inventory:
        if not isinstance(sample, dict) or not isinstance(sample.get("metric"), dict):
            raise TypeError("observed GPU inventory is malformed")
        labels = sample["metric"]
        gpu_uuid = str(labels.get("UUID", ""))
        model = str(labels.get("modelName", ""))
        pod = str(labels.get("pod", ""))
        namespace = str(labels.get("namespace", ""))
        if not gpu_uuid or gpu_uuid in seen or not model or not pod or not namespace:
            raise ValueError(
                "observed GPU inventory lacks unique Kubernetes/DCGM labels"
            )
        if not _gpu_sku_matches(expected_gpu_sku, model):
            raise ValueError("observed GPU SKU does not match the scenario contract")
        seen.add(gpu_uuid)
        observed.append(
            {"uuid": gpu_uuid, "model": model, "pod": pod, "namespace": namespace}
        )
    return {
        "valid": True,
        "required_queries": sorted(PUBLISHABLE_REQUIRED_QUERIES),
        "expected_gpu_count": expected_gpu_count,
        "expected_gpu_sku": expected_gpu_sku,
        "observed_gpus": observed,
    }
