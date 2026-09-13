#!/usr/bin/env python3
"""Expose pinned TensorRT-LLM iteration statistics as Prometheus gauges.

TensorRT-LLM 1.0's ``/metrics`` endpoint returns a bounded JSON snapshot rather
than Prometheus exposition.  This sidecar deliberately exports only values that
are present in that snapshot.  It does not synthesize request counters or
latency histograms; rollout promotion therefore pauses when those measurements
are unavailable.
"""

from __future__ import annotations

import json
import math
import os
import time
import urllib.error
import urllib.request
from collections.abc import Mapping
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

DEFAULT_SOURCE_URL = "http://127.0.0.1:8000/metrics"
DEFAULT_LISTEN_ADDRESS = "0.0.0.0"
DEFAULT_LISTEN_PORT = 9000
MAX_RESPONSE_BYTES = 1024 * 1024


def _number(value: object) -> float | None:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    result = float(value)
    if not math.isfinite(result) or result < 0:
        return None
    return result


def _latest_snapshot(payload: object) -> Mapping[str, Any]:
    if isinstance(payload, list):
        if not payload:
            raise ValueError("TensorRT-LLM metrics response is empty")
        payload = payload[-1]
    if not isinstance(payload, Mapping):
        raise TypeError("TensorRT-LLM metrics response must be an object or array")
    return payload


def parse_snapshot(payload: object) -> dict[str, float]:
    """Return only trustworthy gauge values present in an iteration snapshot."""

    snapshot = _latest_snapshot(payload)
    output: dict[str, float] = {}
    fields = {
        "numActiveRequests": "inferscale_trtllm_active_requests",
        "numQueuedRequests": "inferscale_trtllm_queued_requests",
        "gpuMemUsage": "inferscale_trtllm_gpu_memory_usage_bytes",
        "cpuMemUsage": "inferscale_trtllm_cpu_memory_usage_bytes",
        "pinnedMemUsage": "inferscale_trtllm_pinned_memory_usage_bytes",
    }
    for source, metric in fields.items():
        value = _number(snapshot.get(source))
        if value is not None:
            output[metric] = value

    cache = snapshot.get("kvCacheStats")
    if isinstance(cache, Mapping):
        used = _number(cache.get("usedNumBlocks"))
        free = _number(cache.get("freeNumBlocks"))
        maximum = _number(cache.get("maxNumBlocks"))
        if used is not None:
            output["inferscale_trtllm_kv_cache_used_blocks"] = used
        if free is not None:
            output["inferscale_trtllm_kv_cache_free_blocks"] = free
        denominator = maximum
        if denominator is None and used is not None and free is not None:
            denominator = used + free
        if used is not None and denominator is not None and denominator > 0:
            output["inferscale_trtllm_kv_cache_utilization_ratio"] = min(
                used / denominator, 1.0
            )
    return output


def fetch_snapshot(source_url: str, timeout_seconds: float) -> dict[str, float]:
    request = urllib.request.Request(
        source_url,
        headers={
            "Accept": "application/json",
            "User-Agent": "inferscale-trtllm-exporter/1",
        },
    )
    with urllib.request.urlopen(request, timeout=timeout_seconds) as response:
        content_length = response.headers.get("Content-Length")
        if content_length is not None and int(content_length) > MAX_RESPONSE_BYTES:
            raise ValueError("TensorRT-LLM metrics response exceeds size limit")
        body = response.read(MAX_RESPONSE_BYTES + 1)
    if len(body) > MAX_RESPONSE_BYTES:
        raise ValueError("TensorRT-LLM metrics response exceeds size limit")
    return parse_snapshot(json.loads(body))


def render_metrics(
    metrics: Mapping[str, float], *, scrape_success: bool, duration_seconds: float
) -> bytes:
    lines = [
        "# HELP inferscale_trtllm_metrics_scrape_success Whether the last TensorRT-LLM JSON scrape succeeded.",
        "# TYPE inferscale_trtllm_metrics_scrape_success gauge",
        f"inferscale_trtllm_metrics_scrape_success {1 if scrape_success else 0}",
        "# HELP inferscale_trtllm_metrics_scrape_duration_seconds Time spent fetching the TensorRT-LLM JSON snapshot.",
        "# TYPE inferscale_trtllm_metrics_scrape_duration_seconds gauge",
        f"inferscale_trtllm_metrics_scrape_duration_seconds {duration_seconds:.9g}",
    ]
    for name, value in sorted(metrics.items()):
        lines.extend(
            (
                f"# TYPE {name} gauge",
                f"{name} {value:.17g}",
            )
        )
    return ("\n".join(lines) + "\n").encode("ascii")


def handler(source_url: str, timeout_seconds: float) -> type[BaseHTTPRequestHandler]:
    class MetricsHandler(BaseHTTPRequestHandler):
        def do_GET(self) -> None:
            if self.path == "/healthz":
                self.send_response(200)
                self.send_header("Content-Type", "text/plain; charset=utf-8")
                self.end_headers()
                self.wfile.write(b"ok\n")
                return
            if self.path != "/metrics":
                self.send_error(404)
                return

            started = time.monotonic()
            values: dict[str, float] = {}
            success = False
            try:
                values = fetch_snapshot(source_url, timeout_seconds)
                success = True
            except (
                OSError,
                TypeError,
                ValueError,
                json.JSONDecodeError,
                urllib.error.URLError,
            ):
                # Prometheus must still be able to scrape the exporter and alert
                # on the explicit failure gauge. Never return stale runtime data.
                pass
            body = render_metrics(
                values,
                scrape_success=success,
                duration_seconds=time.monotonic() - started,
            )
            self.send_response(200)
            self.send_header("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, _format: str, *_args: object) -> None:
            return

    return MetricsHandler


def main() -> None:
    source_url = os.environ.get("TRTLLM_METRICS_URL", DEFAULT_SOURCE_URL)
    listen_address = os.environ.get("METRICS_LISTEN_ADDRESS", DEFAULT_LISTEN_ADDRESS)
    listen_port = int(os.environ.get("METRICS_LISTEN_PORT", str(DEFAULT_LISTEN_PORT)))
    timeout_seconds = float(os.environ.get("TRTLLM_METRICS_TIMEOUT_SECONDS", "2"))
    server = ThreadingHTTPServer(
        (listen_address, listen_port), handler(source_url, timeout_seconds)
    )
    server.serve_forever()


if __name__ == "__main__":
    main()
