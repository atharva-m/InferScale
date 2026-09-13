from pathlib import Path

import pytest
from inferscale_bench.scenario import (
    Scenario,
    ScenarioError,
    load_scenario,
)


def test_load_checked_in_smoke_scenario() -> None:
    path = Path(__file__).parents[2] / "scenarios" / "local-smoke.yaml"
    scenario = load_scenario(path)
    assert scenario.name == "qwen-small-vllm-local-smoke"
    assert scenario.deployment.gpu_count == scenario.deployment.tensor_parallelism
    assert Path(scenario.workload.dataset).is_file()
    assert len(scenario.selection_digest()) == 64
    assert scenario.deployment.quantization == "none"


def test_backend_comparison_scenarios_share_selection_digest() -> None:
    scenarios = Path(__file__).parents[2] / "scenarios"
    vllm = load_scenario(scenarios / "backend-comparison-vllm.yaml")
    trtllm = load_scenario(scenarios / "backend-comparison-trtllm.yaml")
    assert vllm.deployment.backend != trtllm.deployment.backend
    assert vllm.selection_digest() == trtllm.selection_digest()


def test_guidellm_selection_digest_matches_control_plane_contract() -> None:
    scenario = Scenario.from_mapping(
        {
            "schema_version": 1,
            "name": "selection-contract",
            "measurement_tool": "guidellm-0.7.0",
            "publishable": True,
            "target": {"base_url": "https://gateway.example", "deployment": "qwen"},
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
                "max_model_len": 8192,
            },
            "workload": {
                "input_tokens": 128,
                "output_tokens": 32,
                "concurrency": 1,
                "requests": 4,
                "dataset": "synthetic",
                "seed": 7,
            },
            "cache_state": "process-warm",
            "routing": {"policy": "round-robin"},
            "experiment": {},
        }
    )
    assert (
        scenario.selection_digest()
        == "b2baed04002d2407d56064ccbe098bc7466ed93043544640939ad84e151f93b4"
    )


def test_rejects_total_gpu_count_that_differs_from_tp() -> None:
    raw = {
        "schema_version": 1,
        "name": "invalid",
        "measurement_tool": "inferscale-native-http-v1",
        "publishable": False,
        "target": {"base_url": "http://localhost:8000", "deployment": "model"},
        "deployment": {
            "model": "Qwen/Qwen3-8B",
            "model_revision": "a" * 40,
            "backend": "vllm",
            "backend_version": "test",
            "precision": "bf16",
            "gpu_type": "RTX_5090",
            "gpu_count": 2,
            "tensor_parallelism": 1,
            "max_model_len": 8192,
        },
        "workload": {
            "input_tokens": 128,
            "output_tokens": 32,
            "concurrency": 1,
            "requests": 1,
            "dataset": "unused.jsonl",
        },
        "cache_state": "process-warm",
        "routing": {"policy": "round-robin"},
    }
    with pytest.raises(ScenarioError, match="gpu_count"):
        Scenario.from_mapping(raw)
