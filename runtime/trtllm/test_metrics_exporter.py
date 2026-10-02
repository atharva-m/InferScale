from __future__ import annotations

import importlib.util
import json
from pathlib import Path
from types import ModuleType

import pytest


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


@pytest.mark.parametrize("hit_ratio", [0, 0.75, 1])
def test_exports_observed_native_prefix_cache_hit_ratio(hit_ratio: float) -> None:
    metrics = exporter.parse_snapshot({"kvCacheStats": {"cacheHitRate": hit_ratio}})

    assert metrics["inferscale_trtllm_prefix_cache_hit_ratio"] == hit_ratio


def test_parses_json_encoded_native_iteration_entries() -> None:
    metrics = exporter.parse_snapshot(
        [json.dumps({"kvCacheStats": {"cacheHitRate": 0.5}})]
    )

    assert metrics["inferscale_trtllm_prefix_cache_hit_ratio"] == 0.5


@pytest.mark.parametrize("payload", [["broken-json"], ["null"], ["[]"], []])
def test_rejects_invalid_native_iteration_entries(payload: object) -> None:
    with pytest.raises((TypeError, ValueError)):
        exporter.parse_snapshot(payload)


@pytest.mark.parametrize("hit_ratio", [None, True, "0.5", -1, 1.01, float("nan"), float("inf")])
def test_omits_missing_or_invalid_prefix_cache_hit_ratio(hit_ratio: object) -> None:
    metrics = exporter.parse_snapshot({"kvCacheStats": {"cacheHitRate": hit_ratio}})

    assert "inferscale_trtllm_prefix_cache_hit_ratio" not in metrics
