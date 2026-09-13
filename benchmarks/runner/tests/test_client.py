import json
from pathlib import Path

from inferscale_bench.client import parse_sse_lines, request_endpoint, request_payload
from inferscale_bench.scenario import load_scenario


def test_parse_sse_lines_handles_comments_usage_and_done() -> None:
    events = [
        b": keepalive\n",
        b"data: "
        + json.dumps({"choices": [{"delta": {"content": "hello"}}]}).encode()
        + b"\n",
        b"\n",
        b"data: "
        + json.dumps({"usage": {"prompt_tokens": 3, "completion_tokens": 1}}).encode()
        + b"\n",
        b"\n",
        b"data: [DONE]\n",
        b"\n",
    ]
    parsed = list(parse_sse_lines(events))
    assert parsed[0] == {"choices": [{"delta": {"content": "hello"}}]}
    assert parsed[1] == {"usage": {"prompt_tokens": 3, "completion_tokens": 1}}


def test_request_payload_always_names_the_deployment_model() -> None:
    scenarios = Path(__file__).parents[2] / "scenarios"
    messages = [{"role": "user", "content": "hello"}]
    for filename in ("local-smoke.yaml", "qwen3-8b-vllm-1gpu.yaml"):
        scenario = load_scenario(scenarios / filename)
        assert (
            request_payload(scenario, messages)["model"] == scenario.target.deployment
        )


def test_inferscale_path_uses_public_uuid_not_model_name() -> None:
    scenario = load_scenario(
        Path(__file__).parents[2] / "scenarios" / "qwen3-8b-vllm-1gpu.yaml"
    )
    deployment_id = "018f6d72-6d22-7b31-a2ac-6a90739016e5"
    endpoint = request_endpoint(scenario, deployment_id)
    assert endpoint.endswith(f"/v1/deployments/{deployment_id}/chat/completions")
    assert scenario.target.deployment not in endpoint
