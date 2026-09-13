from __future__ import annotations

import re
from pathlib import Path
from typing import Any

from .callback import bounded_error
from .scenario import GUIDELLM_TOOL, Scenario, ScenarioError


def _number(value: object) -> float:
    if value is None:
        return 0.0
    if isinstance(value, (int, float, str)):
        return float(value)
    return 0.0


def _percentile(aggregate: dict[str, object], metric: str, percentile: str) -> float:
    distribution = aggregate.get(metric)
    if not isinstance(distribution, dict):
        return 0.0
    return _number(distribution.get(percentile))


def _prometheus_scalar(snapshot: dict[str, object], name: str) -> float:
    series = snapshot.get(name)
    if not isinstance(series, list) or not series:
        return 0.0
    first = series[0]
    if not isinstance(first, dict):
        return 0.0
    value = first.get("value")
    if not isinstance(value, list) or len(value) != 2:
        return 0.0
    try:
        return float(value[1])
    except (TypeError, ValueError):
        return 0.0


def successful_report(
    scenario: Scenario,
    result_payload: dict[str, object],
    artifact: Path,
) -> dict[str, object]:
    """Map the normalized runner artifact onto benchmark.ResultReport."""

    aggregate = result_payload.get("aggregate")
    cost = result_payload.get("cost")
    provenance = result_payload.get("provenance")
    prometheus = result_payload.get("prometheus_snapshot")
    if (
        not isinstance(aggregate, dict)
        or not isinstance(cost, dict)
        or not isinstance(provenance, dict)
    ):
        raise ScenarioError("normalized benchmark result is incomplete")
    if str(provenance.get("benchmark_tool") or "") != scenario.measurement_tool:
        raise ScenarioError("benchmark tool provenance does not match the scenario")
    if scenario.publishable:
        if scenario.measurement_tool != GUIDELLM_TOOL:
            raise ScenarioError("publishable profiles require GuideLLM 0.7.0")
        telemetry = provenance.get("telemetry_evidence")
        if not isinstance(telemetry, dict) or telemetry.get("valid") is not True:
            raise ScenarioError(
                "publishable profile requires validated Prometheus/DCGM telemetry"
            )
    prometheus = prometheus if isinstance(prometheus, dict) else {}
    runtime_digest = str(
        provenance.get("runtime_image_digest")
        or provenance.get("container_image_digest")
        or ""
    )
    if not runtime_digest:
        raise ScenarioError(
            "benchmark profile requires a verified runtime image digest"
        )
    selection_digest = str(result_payload.get("selection_scenario_digest") or "")
    if not re.fullmatch(r"[0-9a-f]{64}", selection_digest):
        raise ScenarioError(
            "benchmark callback requires the authoritative selection-scenario digest"
        )
    model_uri = scenario.deployment.model
    if not model_uri.startswith("hf://"):
        model_uri = "hf://" + model_uri
    measurements = {
        "ttft_p50_ms": _percentile(aggregate, "ttft_ms", "p50"),
        "ttft_p95_ms": _percentile(aggregate, "ttft_ms", "p95"),
        "ttft_p99_ms": _percentile(aggregate, "ttft_ms", "p99"),
        "tpot_p50_ms": _percentile(aggregate, "tpot_ms", "p50"),
        "tpot_p95_ms": _percentile(aggregate, "tpot_ms", "p95"),
        "tpot_p99_ms": _percentile(aggregate, "tpot_ms", "p99"),
        "e2e_p95_ms": _percentile(aggregate, "e2e_ms", "p95"),
        "queue_p95_ms": _prometheus_scalar(prometheus, "queue_p95_seconds") * 1000,
        "input_tokens_per_second": _number(aggregate.get("input_tokens_per_s")),
        "output_tokens_per_second": _number(aggregate.get("output_tokens_per_s")),
        "gpu_hours": _number(cost.get("gpu_hours")),
        "cost_per_million_input_tokens": _number(cost.get("cost_per_1m_input_tokens")),
        "cost_per_million_output_tokens": _number(
            cost.get("cost_per_1m_output_tokens")
        ),
        "cost_per_successful_request": _number(cost.get("cost_per_successful_request")),
        "slo_attainment_percent": _number(aggregate.get("slo_attainment")) * 100,
    }
    profile_key = {
        "model_uri": model_uri,
        "model_revision": scenario.deployment.model_revision,
        "backend": scenario.deployment.backend,
        "backend_version": scenario.deployment.backend_version,
        "runtime_image_digest": runtime_digest,
        "gpu_sku": scenario.deployment.gpu_type,
        "gpu_count": scenario.deployment.gpu_count,
        "precision": scenario.deployment.precision,
        "quantization": scenario.deployment.quantization,
        "tensor_parallelism": scenario.deployment.tensor_parallelism,
        "max_context_bucket": scenario.deployment.max_model_len,
        "driver_cuda_fingerprint": str(provenance.get("driver_cuda_fingerprint") or ""),
        # The control plane freezes this digest before Job creation. It
        # deliberately excludes backend/runtime identity, which is carried by
        # the other profile-key fields, while including workload/cache/routing.
        "scenario_digest": selection_digest,
    }
    if not profile_key["driver_cuda_fingerprint"]:
        raise ScenarioError("benchmark profile requires driver/CUDA provenance")
    configuration_digest = str(
        result_payload.get("scenario_configuration_digest") or ""
    )
    if not re.fullmatch(r"[0-9a-f]{64}", configuration_digest):
        raise ScenarioError(
            "benchmark callback requires the scheduled scenario configuration digest"
        )
    return {
        "state": "succeeded",
        "measurements": measurements,
        "profile_key": profile_key,
        "provenance": provenance,
        "scenario_configuration_digest": configuration_digest,
        "artifact_uri": artifact.resolve().as_uri(),
    }


def failed_report(error: BaseException | str) -> dict[str, Any]:
    return {"state": "failed", "error": bounded_error(error)}
