#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bash -n "${repo_root}/scripts/e2e/live-local-gpu.sh"
test -x "${repo_root}/scripts/e2e/live-local-gpu.sh"
python3 - "${repo_root}/scripts/e2e" <<'PY'
import json
import os
import sys
import tempfile
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, sys.argv[1])
from local_gpu_smoke import (
    MAX_SSE_EVENT_BYTES,
    Smoke,
    bounded_int,
    gpu_model_matches,
    parse_prometheus_samples,
    prefix_cache_metrics,
    validate_chat,
    validate_origin,
    validate_stream,
)


def rejects(function, value):
    try:
        function(value)
    except (RuntimeError, ValueError):
        return
    raise AssertionError("invalid input was accepted")


assert validate_origin("http://127.0.0.1:8080/") == "http://127.0.0.1:8080"
assert validate_origin("https://[::1]:8443") == "https://[::1]:8443"
for origin in (
    "https://remote.example", "http://127.0.0.1/path", "http://token@localhost",
    "http://@localhost", "http://localhost?key=value", " http://localhost",
):
    rejects(validate_origin, origin)

event = {"model": "test-model", "choices": [{"delta": {"content": "hello"}}]}
usage = {"prompt_tokens": 8, "completion_tokens": 2, "total_tokens": 10}
usage_event = {"model": "test-model", "choices": [], "usage": usage}


def encode_stream(*events):
    return b"".join(("data: " + json.dumps(event) + "\r\n\r\n").encode() for event in events) + b"data: [DONE]\r\n\r\n"


raw = encode_stream(event, usage_event)
expected = {"events": 2, "generated_events": 1, "usage": usage}
assert validate_stream(raw, "test-model") == expected
# The final usage-only chunk may omit its model, unlike generated chunks.
assert validate_stream(encode_stream(event, {"choices": [], "usage": usage}), "test-model") == expected
assert validate_stream(encode_stream(event, {"model": None, "choices": [], "usage": usage}), "test-model") == expected


class FragmentedStream:
    def __init__(self, value):
        self.value = value
        self.reads = 0

    def read1(self, limit):
        assert 0 < limit <= MAX_SSE_EVENT_BYTES
        self.reads += 1
        chunk, self.value = self.value[:7], self.value[7:]
        return chunk

    def read(self, *args):
        raise AssertionError("SSE must not read the entire response")


fragmented = FragmentedStream(raw)
assert validate_stream(fragmented, "test-model") == expected
assert fragmented.reads > 10
rejects(lambda value: validate_stream(value, "test-model"), raw.replace(b"data: [DONE]", b""))
rejects(lambda value: validate_stream(value, "wrong-model"), raw)
rejects(lambda value: validate_stream(value, "test-model"), b"data: [DONE]\n\n")
rejects(lambda value: validate_stream(value, "test-model"), raw + raw)
rejects(lambda value: validate_stream(value, "test-model"), raw + b"data: [DONE]\n\n")
rejects(lambda value: validate_stream(value, "test-model"), encode_stream(event))
for bad_usage in (
    {"prompt_tokens": 8, "completion_tokens": 0, "total_tokens": 8},
    {"prompt_tokens": 0, "completion_tokens": 2, "total_tokens": 2},
    {"prompt_tokens": 8, "completion_tokens": -1, "total_tokens": 7},
    {"prompt_tokens": 8, "completion_tokens": 2},
    {"prompt_tokens": 8, "completion_tokens": 2, "total_tokens": 99},
    {"prompt_tokens": True, "completion_tokens": 2, "total_tokens": 3},
):
    rejects(lambda value: validate_stream(value, "test-model"), encode_stream(event, {"choices": [], "usage": bad_usage}))
rejects(lambda value: validate_stream(value, "test-model"), b"data: " + b"x" * MAX_SSE_EVENT_BYTES)
with patch("local_gpu_smoke.time.monotonic", side_effect=[0, 2]):
    rejects(lambda value: validate_stream(value, "test-model", deadline=1), raw)
with patch("local_gpu_smoke.MAX_RESPONSE_BYTES", 8):
    rejects(lambda value: validate_stream(value, "test-model"), raw)
validate_chat({"model": "test-model", "choices": [{"message": {"content": "hello"}}]}, "test-model")
rejects(lambda value: validate_chat(value, "test-model"), {"model": "test-model", "choices": []})

# Prefix-cache metrics are read from vLLM's Prometheus exposition. Support
# both the current names and older client-library `_total` spellings, sum
# labelled series without retaining labels, and fail closed on missing or
# non-positive evidence.
metric_body = """
# HELP vllm:prefix_cache_queries Prefix cache queries.
# TYPE vllm:prefix_cache_queries counter
vllm:prefix_cache_queries{model_name="test"} 12
vllm:prefix_cache_queries{model_name="other"} 3
vllm:prefix_cache_hits{model_name="test"} 5
vllm:prefix_cache_hits{model_name="other"} 2
vllm:kv_cache_usage_perc{model_name="test"} 0.25
"""
assert parse_prometheus_samples(metric_body)["vllm:prefix_cache_queries"] == 15
assert prefix_cache_metrics(metric_body) == {
    "queries": 15,
    "hits": 7,
    "hit_ratio": 7 / 15,
    "kv_cache_usage": 0.25,
}
total_metric_body = """
vllm:prefix_cache_queries_total 4
vllm:prefix_cache_hits_total 1
vllm:gpu_cache_usage_perc 0.5
"""
assert prefix_cache_metrics(total_metric_body)["hit_ratio"] == 0.25
rejects(
    lambda value: prefix_cache_metrics(value),
    metric_body.replace(" 5\n", " 0\n").replace(" 2\n", " 0\n"),
)
rejects(lambda value: prefix_cache_metrics(value), metric_body.replace("0.25", "1.5"))
rejects(lambda value: parse_prometheus_samples(value), "vllm:prefix_cache_hits NaN")
assert gpu_model_matches("RTX_5090", "NVIDIA GeForce RTX 5090, 596.36, 32768")
assert gpu_model_matches("RTX_4070", "NVIDIA GeForce RTX 4070 Laptop GPU, 596.36, 8188")
assert not gpu_model_matches("RTX_5090", "NVIDIA GeForce RTX 4090, 596.36, 24576")
with patch.dict(os.environ, {"INFERSCALE_GPU_TEST_INTEGER": "2"}):
    assert bounded_int("INFERSCALE_GPU_TEST_INTEGER", 99, 1, 4) == 2
with patch.dict(os.environ, {"INFERSCALE_GPU_TEST_INTEGER": "0"}):
    rejects(lambda _value: bounded_int("INFERSCALE_GPU_TEST_INTEGER", 2, 1, 4), None)

# Zero-worker activation is possible on one GPU, but must be explicit and
# cannot silently turn the ordinary readiness/candidate test into an idle run.
with tempfile.TemporaryDirectory() as temporary:
    root = Path(temporary)
    auth = root / "source.json"
    auth.write_text(json.dumps({"base_url": "http://127.0.0.1", "api_key": "fixture"}))
    auth.chmod(0o600)
    environment = {
        "INFERSCALE_GPU_KUBE_CONTEXT": "fixture",
        "INFERSCALE_GPU_AUTH_FILE": str(auth),
        "INFERSCALE_GPU_SCALE_TO_ZERO": "true",
    }
    with patch.dict(os.environ, environment, clear=True), patch("local_gpu_smoke.shutil.which", return_value="kubectl"):
        cold = Smoke(root)
        assert cold.min_replicas == 0 and cold.max_replicas == 1
        assert not cold.candidates and cold.scale_to_zero
        with patch.dict(os.environ, {"INFERSCALE_GPU_MIN_REPLICAS": "1"}):
            rejects(lambda _: Smoke(root), None)
        with patch.dict(os.environ, {"INFERSCALE_GPU_CANDIDATES": "true"}):
            rejects(lambda _: Smoke(root), None)
        with patch.dict(os.environ, {"INFERSCALE_GPU_SCALE_TO_ZERO": "false", "INFERSCALE_GPU_MIN_REPLICAS": "0"}):
            rejects(lambda _: Smoke(root), None)

# Reject a false cold-start pass when an original Pod is still present.
def cold_activation(new_uid):
    cold = Smoke.__new__(Smoke)
    cold.summary = {"checks": []}
    phase = ["initial"]
    calls = []

    def objects(kind, selector):
        if kind == "scaledobjects":
            return [{"spec": {"minReplicaCount": 0}}]
        if kind == "deployments":
            return [{"spec": {"replicas": 0}, "status": {"replicas": 0}}]
        if phase[0] == "idle":
            return []
        return [{"metadata": {"uid": "old" if phase[0] == "initial" else new_uid}}]

    def wait(description, probe):
        if "idle scale-down" in description:
            phase[0] = "idle"
        assert probe()

    def chat(**kwargs):
        calls.append(kwargs)
        phase[0] = "awakened"

    cold.objects, cold.wait, cold.chat = objects, wait, chat
    cold.cr = lambda: {"status": {"revision": {"stable": "stable"}}}
    cold.ready = lambda: True
    cold.exercise_scale_to_zero("stable")
    assert calls == [{"timeout": 900}]
    assert cold.summary["scale_to_zero_cache_state"] == "cache-warm"

cold_activation("new")
rejects(cold_activation, "old")

# A failed candidate retains completed prerequisite Pods for inspection. They
# must not prevent us from recognizing retirement of its GPU worker and EPP.
smoke = Smoke.__new__(Smoke)
inventory = {
    "deployments": [{"metadata": {"annotations": {"inferscale.io/retired-at": "now"}}, "spec": {"replicas": 0}}],
    "scaledobjects": [],
    "pods": [{"metadata": {"labels": {"app.kubernetes.io/component": "model-prefetch"}}}],
}
smoke.objects = lambda kind, selector: inventory[kind]
assert smoke.retired("candidate")
inventory["pods"].append({"metadata": {"labels": {"app.kubernetes.io/component": "model-server"}}})
assert not smoke.retired("candidate")
print("local GPU harness offline checks passed")
PY
