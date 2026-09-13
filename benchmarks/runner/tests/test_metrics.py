import io
import json
import urllib.parse

import pytest
from inferscale_bench import metrics
from inferscale_bench.client import MeasurementInterval, RequestResult
from inferscale_bench.metrics import (
    PUBLISHABLE_REQUIRED_QUERIES,
    aggregate,
    build_prometheus_queries,
    percentile,
    scrape_prometheus,
    validate_publishable_telemetry,
)
from inferscale_bench.scenario import SLO


def result(index: int, ttft: float, tpot: float) -> RequestResult:
    return RequestResult(index, True, 200, ttft, tpot, ttft + tpot * 3, 10, 4, 10.0)


def test_percentile_uses_nearest_rank() -> None:
    values = list(range(1, 101))
    assert percentile(values, 0.50) == 50
    assert percentile(values, 0.95) == 95
    assert percentile([], 0.95) is None


def test_aggregate_reports_tokens_failures_and_slo() -> None:
    results = [
        result(0, 100, 10),
        result(1, 200, 20),
        RequestResult(
            2, False, 503, None, None, 50, 0, 0, None, "http_error", "unavailable"
        ),
    ]
    metrics = aggregate(results, 2.0, SLO(ttft_p95_ms=150, tpot_p95_ms=15))
    assert metrics["successful_requests"] == 2
    assert metrics["failed_requests"] == 1
    assert metrics["prompt_tokens"] == 20
    assert metrics["output_tokens_per_s"] == 4
    assert metrics["slo_attainment"] == 0.5


def test_publishable_telemetry_requires_exact_gpu_inventory() -> None:
    timestamp = 1000.0
    interval = MeasurementInterval(timestamp - 30, timestamp)
    snapshot: dict[str, object] = {
        name: [{"value": [timestamp, "0"]}] for name in PUBLISHABLE_REQUIRED_QUERIES
    }
    snapshot["gpu_inventory"] = [
        {
            "metric": {
                "UUID": "GPU-1",
                "modelName": "NVIDIA GeForce RTX 5090",
                "namespace": "tenant-a",
                "pod": "qwen-vllm-1",
            },
            "value": [timestamp, "1"],
        }
    ]
    evidence = validate_publishable_telemetry(snapshot, 1, "RTX_5090", interval)
    assert evidence["valid"] is True
    with pytest.raises(ValueError, match="accelerator count"):
        validate_publishable_telemetry(snapshot, 2, "RTX_5090", interval)
    # A fresh instant value after the run is not evidence of its workload.
    snapshot["gpu_utilization"] = [{"value": [timestamp + 10, "0"]}]
    with pytest.raises(ValueError, match="stale"):
        validate_publishable_telemetry(snapshot, 1, "RTX_5090", interval)


def test_prometheus_queries_are_revision_scoped() -> None:
    queries = build_prometheus_queries(
        "tenant-a",
        "qwen-deadbeef-vllm",
        "qwen-deadbeef-epp",
        MeasurementInterval(1000, 1020),
    )
    for name in (
        "gpu_utilization",
        "gpu_memory_used_bytes",
        "gpu_power_watts",
        "waiting_requests",
        "running_requests",
        "kv_cache_utilization",
        "gpu_inventory",
    ):
        assert 'namespace="tenant-a"' in queries[name]
        assert 'pod=~"^qwen-deadbeef-vllm-.*$"' in queries[name]
    assert 'namespace="tenant-a"' in queries["queue_p95_seconds"]
    assert 'service="qwen-deadbeef-epp"' in queries["queue_p95_seconds"]
    assert (
        "inferscale_router_flow_control_wait_seconds_bucket"
        in queries["queue_p95_seconds"]
    )
    for query in queries.values():
        assert "timestamp(" in query
        assert "< 60" in query
        assert "[20000ms]" in query
        assert "[5m]" not in query
    assert "avg_over_time(" in queries["gpu_utilization"]
    assert "increase(" in queries["queue_p95_seconds"]


def test_prometheus_queries_reject_unscoped_or_invalid_identity() -> None:
    with pytest.raises(ValueError, match="EPP service"):
        build_prometheus_queries(
            "tenant-a", "runtime-a", "not/valid", MeasurementInterval(1000, 1020)
        )


def test_collection_uses_run_end_after_reporting_delay(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    interval = MeasurementInterval(1000, 1030)
    requested: list[dict[str, list[str]]] = []

    def open_query(url: str, **_kwargs: object) -> io.BytesIO:
        params = urllib.parse.parse_qs(urllib.parse.urlparse(url).query)
        requested.append(params)
        # This endpoint's current value has fallen idle; only the measured
        # historical window should reach the normalized artifact.
        measured = params.get("time") == ["1030"] and "[30000ms]" in params["query"][0]
        return io.BytesIO(
            json.dumps(
                {"data": {"result": [{"value": [1030, "85" if measured else "0"]}]}}
            ).encode()
        )

    monkeypatch.setattr(metrics.urllib.request, "urlopen", open_query)
    snapshot = scrape_prometheus(
        "http://prometheus.example",
        interval,
        deployment_namespace="tenant-a",
        runtime_pod_prefix="runtime-a",
        epp_service="epp-a",
    )
    assert len(requested) == 8
    assert snapshot["gpu_utilization"] == [{"value": [1030, "85"]}]


@pytest.mark.parametrize(
    "start,end", [(10, 10), (10, 9), (float("nan"), 10), (10, float("inf"))]
)
def test_measurement_interval_rejects_invalid_boundaries(
    start: float, end: float
) -> None:
    with pytest.raises(ValueError, match="interval"):
        MeasurementInterval(start, end)
