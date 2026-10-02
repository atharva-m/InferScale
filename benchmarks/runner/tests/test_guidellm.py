import importlib
import json
import os
import shutil
import stat
from itertools import islice
from pathlib import Path
from types import SimpleNamespace
from typing import cast

import pytest
import yaml
from inferscale_bench import guidellm
from inferscale_bench.scenario import Scenario


def scenario() -> Scenario:
    return Scenario.from_mapping(
        {
            "schema_version": 1,
            "name": "guide-test",
            "measurement_tool": "guidellm-0.7.0",
            "publishable": True,
            "target": {
                "base_url": "https://gateway.example",
                "deployment": "qwen-chat",
                "api_mode": "inferscale",
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
                "requests": 2,
                "dataset": "synthetic",
                "seed": 7,
            },
            "cache_state": "process-warm",
            "routing": {"policy": "load-aware"},
        }
    )


def fixture() -> Path:
    return Path(__file__).parent / "fixtures" / "guidellm-0.7.0-report.json"


def test_parses_pinned_fixture_and_strips_request_content() -> None:
    measured = guidellm.parse_report(fixture(), scenario())
    results, duration = measured.requests, measured.wall_time_s
    assert duration == 1.5
    assert len(results) == 2
    assert results[0].prompt_tokens == 8
    assert results[1].tpot_ms == 6.0
    assert "request_id" not in results[0].to_dict()
    assert measured.interval.start == 1780000000.0
    assert measured.interval.end == 1780000001.5


@pytest.mark.parametrize("end_time", [1780000000.0, 1780000002.0, float("nan")])
def test_rejects_invalid_measurement_interval(tmp_path: Path, end_time: float) -> None:
    raw = json.loads(fixture().read_text(encoding="utf-8"))
    raw["benchmarks"][0]["end_time"] = end_time
    path = tmp_path / "bad-interval.json"
    path.write_text(json.dumps(raw), encoding="utf-8")
    with pytest.raises(guidellm.GuideLLMError):
        guidellm.parse_report(path, scenario())


def test_rejects_token_contract_mismatch(tmp_path: Path) -> None:
    raw = json.loads(fixture().read_text(encoding="utf-8"))
    raw["benchmarks"][0]["requests"]["successful"][0]["prompt_tokens"] = 9
    path = tmp_path / "mismatch.json"
    path.write_text(json.dumps(raw), encoding="utf-8")
    with pytest.raises(guidellm.GuideLLMError, match="prompt tokens"):
        guidellm.parse_report(path, scenario())


def test_run_keeps_secret_out_of_argv_and_environment(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    secret = "run-secret-never-in-argv"
    observed: dict[str, object] = {}

    def fake_run(command: list[str], **kwargs: object) -> SimpleNamespace:
        observed["command"] = command
        observed["env"] = kwargs["env"]
        config_path = Path(command[-1])
        assert stat.S_IMODE(config_path.stat().st_mode) == 0o600
        config = yaml.safe_load(config_path.read_text(encoding="utf-8"))
        observed["config"] = config
        spec = config["spec"]
        assert spec["backend"]["request_format"] == "/v1/chat/completions"
        assert spec["backend"]["api_routes"]["/v1/chat/completions"] == (
            "v1/deployments/018f6d72-6d22-7b31-a2ac-6a90739016e5/chat/completions"
        )
        assert config["benchmarks"] == [{"profile.streams": [1]}]
        assert spec["tokenizer"]["model"] == "Qwen/Qwen3-8B"
        assert spec["tokenizer"]["load_kwargs"]["revision"] == "a" * 40
        output = Path(spec["outputs"][0]["path"])
        shutil.copyfile(fixture(), output)
        return SimpleNamespace(returncode=0, stdout="", stderr="")

    monkeypatch.setenv("INFERSCALE_API_KEY", secret)
    monkeypatch.setenv("INFERSCALE_BENCHMARK_TOKEN", secret)
    monkeypatch.setattr(guidellm.subprocess, "run", fake_run)
    measured = guidellm.run(scenario(), secret, "018f6d72-6d22-7b31-a2ac-6a90739016e5")
    assert len(measured.requests) == 2
    assert secret not in " ".join(observed["command"])  # type: ignore[arg-type]
    child_environment = cast(dict[str, str], observed["env"])
    assert secret not in child_environment.values()
    assert observed["config"]["spec"]["backend"]["api_key"] == secret  # type: ignore[index]
    assert os.environ["INFERSCALE_API_KEY"] == secret


def test_config_validates_with_installed_guidellm(tmp_path: Path) -> None:
    # CI and the runner image install the pinned package; lightweight local
    # environments can still run the remaining tests without its ML stack.
    pinned = pytest.importorskip("guidellm.benchmark")

    config = guidellm.build_config(
        scenario(),
        "schema-test-key",
        "018f6d72-6d22-7b31-a2ac-6a90739016e5",
        tmp_path / "report.json",
    )
    path = tmp_path / "scenario.yaml"
    path.write_text(yaml.safe_dump(config), encoding="utf-8")
    loaded = pinned.BenchmarkScenario.create(scenario=path)
    benchmark = loaded.get_benchmarks()[0]
    assert benchmark.profile.streams == [1]
    assert benchmark.backend.target == "https://gateway.example"
    assert benchmark.tokenizer.load_kwargs["revision"] == "a" * 40

    random_utils = importlib.import_module("guidellm.utils.random")

    data = benchmark.data[0]
    for prefix, count in (("prompt", 8), ("output", 4)):
        sampler = random_utils.IntegerRangeSampler(
            average=getattr(data, f"{prefix}_tokens"),
            variance=getattr(data, f"{prefix}_tokens_stdev"),
            min_value=getattr(data, f"{prefix}_tokens_min"),
            max_value=getattr(data, f"{prefix}_tokens_max"),
            random_seed=7,
        )
        assert list(islice(sampler, 100)) == [count] * 100
