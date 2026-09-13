from __future__ import annotations

import importlib.util
from pathlib import Path
from types import ModuleType


def _load_exporter() -> ModuleType:
    path = Path(__file__).with_name("metrics_exporter.py")
    spec = importlib.util.spec_from_file_location("trtllm_metrics_exporter", path)
    assert spec is not None and spec.loader is not None
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


exporter = _load_exporter()


def test_parse_snapshot_uses_latest_iteration_and_computes_kv_ratio() -> None:
    metrics = exporter.parse_snapshot(
        [
            {"numActiveRequests": 99},
            {
                "numActiveRequests": 2,
                "numQueuedRequests": 3,
                "gpuMemUsage": 4096,
                "kvCacheStats": {"usedNumBlocks": 25, "freeNumBlocks": 75},
            },
        ]
    )

    assert metrics["inferscale_trtllm_active_requests"] == 2
    assert metrics["inferscale_trtllm_queued_requests"] == 3
    assert metrics["inferscale_trtllm_gpu_memory_usage_bytes"] == 4096
    assert metrics["inferscale_trtllm_kv_cache_utilization_ratio"] == 0.25


def test_parse_snapshot_does_not_invent_request_or_latency_metrics() -> None:
    metrics = exporter.parse_snapshot({"numActiveRequests": 1})

    assert all("ttft" not in metric for metric in metrics)
    assert all("tpot" not in metric for metric in metrics)
    assert all("requests_total" not in metric for metric in metrics)


def test_render_failure_exposes_only_exporter_health() -> None:
    body = exporter.render_metrics(
        {}, scrape_success=False, duration_seconds=0.25
    ).decode()

    assert "inferscale_trtllm_metrics_scrape_success 0" in body
    assert "inferscale_trtllm_active_requests" not in body
