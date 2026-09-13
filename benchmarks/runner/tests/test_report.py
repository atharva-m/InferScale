from pathlib import Path

import pytest
from inferscale_bench.report import successful_report
from inferscale_bench.scenario import load_scenario


def test_successful_report_matches_go_callback_contract(tmp_path: Path) -> None:
    scenario_path = Path(__file__).parents[2] / "scenarios" / "qwen3-8b-vllm-1gpu.yaml"
    scenario = load_scenario(scenario_path)
    artifact = tmp_path / "result.json"
    artifact.write_text("{}", encoding="utf-8")
    payload: dict[str, object] = {
        "aggregate": {
            "ttft_ms": {"p50": 10.0, "p95": 20.0, "p99": 30.0},
            "tpot_ms": {"p50": 2.0, "p95": 3.0, "p99": 4.0},
            "e2e_ms": {"p50": 100.0, "p95": 120.0, "p99": 130.0},
            "input_tokens_per_s": 1000.0,
            "output_tokens_per_s": 200.0,
            "slo_attainment": 0.98,
        },
        "cost": {
            "gpu_hours": 0.5,
            "cost_per_1m_input_tokens": 1.0,
            "cost_per_1m_output_tokens": 2.0,
            "cost_per_successful_request": 0.01,
        },
        "provenance": {
            "container_image_digest": "sha256:" + "a" * 64,
            "driver_cuda_fingerprint": "driver-cuda",
            "benchmark_tool": "guidellm-0.7.0",
            "telemetry_evidence": {"valid": True},
        },
        "selection_scenario_digest": "c" * 64,
        "scenario_configuration_digest": "b" * 64,
        "prometheus_snapshot": {
            "queue_p95_seconds": [{"value": [0, "0.025"]}],
        },
    }
    report = successful_report(scenario, payload, artifact)
    measurements = report["measurements"]
    profile = report["profile_key"]
    assert report["state"] == "succeeded"
    assert measurements["queue_p95_ms"] == 25.0  # type: ignore[index]
    assert measurements["slo_attainment_percent"] == 98.0  # type: ignore[index]
    assert profile["model_uri"] == "hf://Qwen/Qwen3-8B"  # type: ignore[index]
    assert profile["scenario_digest"] == "c" * 64  # type: ignore[index]
    assert report["scenario_configuration_digest"] == "b" * 64


def test_successful_report_requires_control_plane_selection_digest(
    tmp_path: Path,
) -> None:
    scenario = load_scenario(
        Path(__file__).parents[2] / "scenarios" / "qwen3-8b-vllm-1gpu.yaml"
    )
    payload: dict[str, object] = {
        "aggregate": {},
        "cost": {},
        "provenance": {
            "container_image_digest": "sha256:" + "a" * 64,
            "driver_cuda_fingerprint": "driver-cuda",
            "benchmark_tool": "guidellm-0.7.0",
            "telemetry_evidence": {"valid": True},
        },
        "scenario_configuration_digest": "b" * 64,
    }
    with pytest.raises(ValueError, match="authoritative selection-scenario digest"):
        successful_report(scenario, payload, tmp_path / "result.json")
