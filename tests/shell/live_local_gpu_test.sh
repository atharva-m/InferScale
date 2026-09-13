#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bash -n "${repo_root}/scripts/e2e/live-local-gpu.sh"
test -x "${repo_root}/scripts/e2e/live-local-gpu.sh"
python3 - "${repo_root}/scripts/e2e" <<'PY'
import json
import sys
from unittest.mock import patch

sys.path.insert(0, sys.argv[1])
from local_gpu_smoke import MAX_SSE_EVENT_BYTES, Smoke, validate_chat, validate_origin, validate_stream


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
