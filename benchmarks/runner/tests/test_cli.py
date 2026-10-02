import hashlib
import json
from pathlib import Path

import pytest
from inferscale_bench import cli
from inferscale_bench.client import MeasuredRun, MeasurementInterval, RequestResult


def scheduled_scenario(tmp_path: Path) -> Path:
    dataset = tmp_path / "requests.jsonl"
    dataset.write_text(
        json.dumps({"messages": [{"role": "user", "content": "hello"}]}) + "\n",
        encoding="utf-8",
    )
    configuration = {
        "schema_version": 1,
        "name": "scheduled-test",
        "measurement_tool": "guidellm-0.7.0",
        "publishable": True,
        "target": {
            "base_url": "https://gateway.example",
            "deployment": "qwen-chat",
            "api_mode": "inferscale",
            "api_key_env": "INFERSCALE_API_KEY",
        },
        "deployment": {
            "model": "Qwen/Qwen3-8B",
            "model_revision": "a" * 40,
            "backend": "vllm",
            "backend_version": "0.23.0",
            "precision": "bf16",
            "quantization": "none",
            "gpu_type": "RTX_5090",
            "gpu_count": 1,
            "tensor_parallelism": 1,
            "prefix_cache": True,
            "max_model_len": 8192,
        },
        "workload": {
            "input_tokens": 8,
            "output_tokens": 4,
            "concurrency": 1,
            "requests": 1,
            "dataset": "synthetic",
        },
        "cache_state": "process-warm",
        "routing": {"policy": "load-aware"},
        "metadata": {
            "provider": "test-host",
            "gpu_hourly_price": "1.00",
            "container_image_digest": "sha256:" + "b" * 64,
            "driver_version": "580.10",
            "cuda_version": "13.0",
        },
    }
    path = tmp_path / "scenario.json"
    path.write_text(json.dumps(configuration, separators=(",", ":")), encoding="utf-8")
    return path


def test_scheduled_run_uses_run_token_for_inference_and_callback(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    scenario_path = scheduled_scenario(tmp_path)
    captured: dict[str, object] = {}

    interval = MeasurementInterval(1000, 1001)

    def run_guidellm(*args: object) -> MeasuredRun:
        captured["api_key"] = args[1]
        return MeasuredRun(
            [RequestResult(0, True, 200, 10.0, 2.0, 20.0, 8, 4, 200.0)],
            1.0,
            interval,
        )

    def scrape_prometheus(*args: object, **_kwargs: object) -> dict[str, object]:
        captured["interval"] = args[1]
        return {}

    def post_result(callback: str, run_id: str, report: dict[str, object]) -> None:
        captured["callback"] = callback
        captured["run_id"] = run_id
        captured["report"] = report

    monkeypatch.setattr(cli, "run_guidellm", run_guidellm)
    monkeypatch.setattr(cli, "scrape_prometheus", scrape_prometheus)
    monkeypatch.setattr(
        cli,
        "validate_publishable_telemetry",
        lambda *_args, **_kwargs: {
            "valid": True,
            "observed_gpus": [
                {
                    "uuid": "GPU-1",
                    "model": "NVIDIA GeForce RTX 5090",
                    "pod": "qwen-chat-deadbeef-vllm-0",
                    "namespace": "tenant-test",
                }
            ],
        },
    )
    monkeypatch.setattr(
        cli,
        "collect",
        lambda *_: {
            "gpu_hourly_price": 1.0,
            "container_image_digest": "sha256:" + "b" * 64,
            "driver_cuda_fingerprint": "driver-cuda",
            "benchmark_tool": "guidellm-0.7.0",
        },
    )
    monkeypatch.setattr(cli, "post_result", post_result)
    monkeypatch.delenv("INFERSCALE_API_KEY", raising=False)
    monkeypatch.setenv("INFERSCALE_BENCHMARK_TOKEN", "run-scoped-token")
    code = cli.main(
        [
            "run",
            "--scenario",
            str(scenario_path),
            "--results-dir",
            str(tmp_path / "results"),
            "--deployment-id",
            "018f6d72-6d22-7b31-a2ac-6a90739016e5",
            "--run-id",
            "run-1",
            "--callback",
            "http://api.example/internal/v1/benchmarks/run-1/result",
            "--runtime-version",
            "0.23.0",
            "--runner-image-digest",
            "sha256:" + "c" * 64,
            "--configuration-digest",
            hashlib.sha256(scenario_path.read_bytes()).hexdigest(),
            "--selection-scenario-digest",
            "d" * 64,
            "--prometheus-url",
            "http://prometheus.example:9090",
            "--deployment-namespace",
            "tenant-test",
            "--runtime-pod-prefix",
            "qwen-chat-deadbeef-vllm",
            "--epp-service",
            "qwen-chat-deadbeef-epp",
        ]
    )
    assert code == 0
    assert captured["interval"] == interval
    artifact = next((tmp_path / "results").glob("*.json"))
    assert json.loads(artifact.read_text())["measurement_interval"] == {
        "start_unix_s": 1000,
        "end_unix_s": 1001,
    }
    assert captured["api_key"] == "run-scoped-token"
    assert captured["run_id"] == "run-1"
    assert captured["report"]["state"] == "succeeded"  # type: ignore[index]
    assert captured["report"]["profile_key"]["scenario_digest"] == "d" * 64  # type: ignore[index]
    assert captured["report"]["provenance"]["nvidia_gpus"] == [  # type: ignore[index]
        "NVIDIA GeForce RTX 5090"
    ]


def test_scheduled_validation_failure_posts_redacted_terminal_result(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    captured: dict[str, object] = {}

    def post_result(callback: str, run_id: str, report: dict[str, object]) -> None:
        captured.update({"callback": callback, "run_id": run_id, "report": report})

    monkeypatch.setattr(cli, "post_result", post_result)
    monkeypatch.setenv("INFERSCALE_BENCHMARK_TOKEN", "secret-token")
    code = cli.main(
        [
            "run",
            "--scenario",
            str(tmp_path / "missing.json"),
            "--run-id",
            "run-2",
            "--callback",
            "http://api.example/callback",
        ]
    )
    assert code == 1
    assert captured["report"]["state"] == "failed"  # type: ignore[index]
    assert "secret-token" not in captured["report"]["error"]  # type: ignore[index]
