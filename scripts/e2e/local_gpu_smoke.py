"""Bounded real-GPU Kubernetes checks; never retain request/response content."""

from __future__ import annotations

import base64
import concurrent.futures
import datetime as dt
import io
import ipaddress
import json
import math
import os
import re
import shutil
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from pathlib import Path
from typing import BinaryIO

MODEL = "hf://Qwen/Qwen3-0.6B"
REVISION = "c1899de289a04d12100db370d81485cdf75e47ca"
GPU = "RTX_4070"
SUPPORTED_TENSOR_PARALLELISMS = (1, 2, 4)
STREAM_TIMEOUT_SECONDS = 180
MAX_RESPONSE_BYTES = 4 * 1024 * 1024
MAX_SSE_EVENT_BYTES = 64 * 1024
LABEL_REVISION = "inferscale.io/revision"
EXPECTED_FAILURES = (
    RuntimeError,
    ValueError,
    LookupError,
    TypeError,
    OSError,
    subprocess.SubprocessError,
    StopIteration,
)


def check(condition: object, message: str) -> None:
    if not condition:
        raise RuntimeError(message)


def flag(name: str, default: bool) -> bool:
    value = os.environ.get(name, str(default).lower())
    check(value in {"true", "false"}, f"{name} must be true or false")
    return value == "true"


def bounded_int(name: str, default: int, minimum: int, maximum: int) -> int:
    value = os.environ.get(name, str(default))
    check(re.fullmatch(r"[0-9]+", value) is not None, f"{name} must be an integer")
    parsed = int(value)
    check(
        minimum <= parsed <= maximum,
        f"{name} must be between {minimum} and {maximum}",
    )
    return parsed


def gpu_model_matches(expected: str, observed: str) -> bool:
    """Match a configured SKU to nvidia-smi's vendor-qualified model name."""

    normalize = lambda value: re.sub(r"[^a-z0-9]", "", value.lower())
    expected_value = normalize(expected)
    observed_value = normalize(observed)
    return bool(expected_value) and expected_value in observed_value


PROMETHEUS_SAMPLE = re.compile(
    r"^(?P<name>[a-zA-Z_:][a-zA-Z0-9_:]*)(?:\{[^}\n]*\})?\s+"
    r"(?P<value>[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?)$"
)


def parse_prometheus_samples(body: str) -> dict[str, float]:
    """Sum unlabelled and labelled samples without retaining arbitrary labels."""

    values: dict[str, float] = {}
    for line in body.splitlines():
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        match = PROMETHEUS_SAMPLE.fullmatch(stripped)
        if match is None:
            # Prometheus permits NaN and +/-Inf sample values. They are not
            # useful evidence for a functional smoke check and must not be
            # silently ignored if a runtime emits them.
            sample_value = stripped.rsplit(None, 1)[-1].lower()
            check(
                sample_value not in {"nan", "+nan", "-nan", "inf", "+inf", "-inf"},
                "runtime metrics are not finite",
            )
            continue
        value = float(match.group("value"))
        check(
            math.isfinite(value),
            "runtime metrics are not finite",
        )
        name = match.group("name")
        values[name] = values.get(name, 0.0) + value
    return values


def prefix_cache_metrics(body: str) -> dict[str, float]:
    """Validate the vLLM prefix-cache counters after repeated inference."""

    values = parse_prometheus_samples(body)

    def first(*names: str) -> tuple[str, float] | None:
        for name in names:
            if name in values:
                return name, values[name]
        return None

    queries = first("vllm:prefix_cache_queries", "vllm:prefix_cache_queries_total")
    hits = first("vllm:prefix_cache_hits", "vllm:prefix_cache_hits_total")
    kv_usage = first("vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc")
    check(queries is not None, "vLLM prefix-cache query metric is missing")
    check(hits is not None, "vLLM prefix-cache hit metric is missing")
    check(kv_usage is not None, "vLLM KV-cache usage metric is missing")
    check(queries[1] > 0, "repeated inference produced no prefix-cache queries")
    check(hits[1] > 0, "repeated inference produced no prefix-cache hits")
    check(0 <= kv_usage[1] <= 1, "vLLM KV-cache usage is outside the 0..1 range")
    return {
        "queries": queries[1],
        "hits": hits[1],
        "hit_ratio": hits[1] / queries[1],
        "kv_cache_usage": kv_usage[1],
    }


def validate_origin(value: str) -> str:
    parsed = urllib.parse.urlsplit(value)
    try:
        local = (
            parsed.hostname == "localhost"
            or ipaddress.ip_address(parsed.hostname or "").is_loopback
        )
        valid_port = parsed.port is None or 0 < parsed.port < 65536
    except ValueError:
        local, valid_port = False, False
    check(
        value == value.strip()
        and local
        and valid_port
        and parsed.scheme in {"http", "https"}
        and parsed.username is None
        and parsed.password is None
        and parsed.path in {"", "/"}
        and not parsed.query
        and not parsed.fragment,
        "base_url must be a loopback HTTP(S) origin; port-forward the local Gateway",
    )
    return value.rstrip("/")


def validate_chat(payload: dict, model: str) -> None:
    check(payload.get("model") == model, "JSON response model mismatch")
    choices = payload.get("choices", [])
    check(bool(choices), "JSON response has no choices")
    message = choices[0].get("message", {})
    # Qwen3 may emit reasoning before its final answer under this short budget.
    check(
        bool(
            message.get("content")
            or message.get("reasoning_content")
            or message.get("reasoning")
        ),
        "JSON response contains no generated text",
    )


def stream_lines(response: BinaryIO, deadline: float):
    """Read available chunks without buffering an entire response or huge line."""
    pending = bytearray()
    total = 0
    while True:
        remaining = deadline - time.monotonic()
        check(remaining > 0, "SSE exceeded its total time limit")
        # urllib's HTTPResponse exposes its socket through the buffered reader.
        # Refresh the timeout for every single-read read1 call, so a slow stream
        # cannot restart the full deadline with each arriving chunk.
        connection = getattr(
            getattr(getattr(response, "fp", None), "raw", None), "_sock", None
        )
        if connection is not None:
            connection.settimeout(remaining)
        chunk = response.read1(MAX_SSE_EVENT_BYTES)
        check(time.monotonic() <= deadline, "SSE exceeded its total time limit")
        if not chunk:
            if pending:
                yield bytes(pending)
            return
        total += len(chunk)
        check(total <= MAX_RESPONSE_BYTES, "SSE exceeded its total byte limit")
        pending.extend(chunk)
        while b"\n" in pending:
            line, _, rest = pending.partition(b"\n")
            check(len(line) <= MAX_SSE_EVENT_BYTES, "SSE line exceeded its byte limit")
            pending = bytearray(rest)
            yield bytes(line).rstrip(b"\r")
        check(len(pending) <= MAX_SSE_EVENT_BYTES, "SSE line exceeded its byte limit")


def validate_stream(response: BinaryIO | bytes, model: str, *, deadline=None) -> dict:
    if isinstance(response, bytes):
        response = io.BytesIO(response)
    if deadline is None:
        deadline = time.monotonic() + STREAM_TIMEOUT_SECONDS
    done, events, generated_events = False, 0, 0
    usage = None
    data_lines = []
    event_bytes = 0

    def consume_event(data):
        nonlocal done, events, generated_events, usage
        check(not done, "SSE data follows the terminal marker")
        if data == "[DONE]":
            check(generated_events > 0, "SSE terminal marker precedes generated text")
            check(
                usage is not None, "SSE terminal marker precedes positive token usage"
            )
            done = True
            return
        event = json.loads(data)
        check(
            isinstance(event, dict) and not event.get("error"),
            "SSE returned an invalid event or error",
        )
        choices = event.get("choices", [])
        check(isinstance(choices, list), "SSE choices must be an array")
        event_usage = event.get("usage")
        # A final usage-only frame may omit model; content frames must identify
        # the deployment, and every supplied model must still match it.
        check(
            event.get("model") == model
            or (
                event.get("model") is None
                and not choices
                and isinstance(event_usage, dict)
            ),
            "SSE response model mismatch",
        )
        if event_usage is not None:
            check(isinstance(event_usage, dict), "SSE usage must be an object")
            counts = {
                name: event_usage.get(name)
                for name in ("prompt_tokens", "completion_tokens", "total_tokens")
            }
            check(
                all(type(value) is int and value > 0 for value in counts.values()),
                "SSE token usage must contain positive integer counts",
            )
            check(
                counts["total_tokens"]
                == counts["prompt_tokens"] + counts["completion_tokens"],
                "SSE total token usage is inconsistent",
            )
            usage = counts
        generated = False
        for choice in choices:
            check(isinstance(choice, dict), "SSE choice must be an object")
            delta = choice.get("delta", {})
            check(isinstance(delta, dict), "SSE delta must be an object")
            generated = generated or any(
                isinstance(delta.get(name), str) and delta[name]
                for name in ("content", "reasoning_content", "reasoning")
            )
        generated_events += int(generated)
        events += 1

    for raw_line in stream_lines(response, deadline):
        line = raw_line.decode("utf-8")
        if line.startswith("data:"):
            check(not done, "SSE data follows the terminal marker")
            event_bytes += len(raw_line)
            check(
                event_bytes <= MAX_SSE_EVENT_BYTES, "SSE event exceeded its byte limit"
            )
            data_lines.append(line[5:].lstrip(" "))
        elif not line and data_lines:
            consume_event("\n".join(data_lines))
            data_lines.clear()
            event_bytes = 0
    if data_lines:
        consume_event("\n".join(data_lines))
    check(
        done and generated_events > 0 and usage is not None,
        "SSE lacks generated text, positive usage or terminal marker",
    )
    return {"events": events, "generated_events": generated_events, "usage": usage}


class Smoke:
    def __init__(self, work: Path):
        self.context = os.environ.get("INFERSCALE_GPU_KUBE_CONTEXT", "")
        check(self.context, "INFERSCALE_GPU_KUBE_CONTEXT is required")
        auth_path = Path(os.environ.get("INFERSCALE_GPU_AUTH_FILE", ""))
        check(
            auth_path.is_file(),
            "INFERSCALE_GPU_AUTH_FILE must name a private JSON file",
        )
        check(
            not stat.S_IMODE(auth_path.stat().st_mode) & 0o077,
            "auth file must have mode 0600 or stricter",
        )
        private_auth = work / "auth.json"
        shutil.copyfile(auth_path, private_auth)
        private_auth.chmod(0o600)
        auth = json.loads(private_auth.read_text())
        self.origin = validate_origin(auth["base_url"])
        self.token = auth["api_key"]
        check(
            isinstance(self.token, str)
            and self.token
            and not any(character.isspace() for character in self.token),
            "invalid API key",
        )
        self.timeout = int(os.environ.get("INFERSCALE_GPU_WAIT_SECONDS", "1200"))
        check(60 <= self.timeout <= 3600, "wait seconds must be between 60 and 3600")
        self.gpu_type = os.environ.get("INFERSCALE_GPU_TYPE", GPU)
        check(
            re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,63}", self.gpu_type) is not None,
            "INFERSCALE_GPU_TYPE must be a short GPU SKU",
        )
        self.gpu_count = bounded_int(
            "INFERSCALE_GPU_COUNT", 1, 1, max(SUPPORTED_TENSOR_PARALLELISMS)
        )
        check(
            self.gpu_count in SUPPORTED_TENSOR_PARALLELISMS,
            "INFERSCALE_GPU_COUNT must be 1, 2, or 4",
        )
        self.tensor_parallelism = bounded_int(
            "INFERSCALE_GPU_TENSOR_PARALLELISM",
            self.gpu_count,
            1,
            max(SUPPORTED_TENSOR_PARALLELISMS),
        )
        check(
            self.tensor_parallelism in SUPPORTED_TENSOR_PARALLELISMS,
            "INFERSCALE_GPU_TENSOR_PARALLELISM must be 1, 2, or 4",
        )
        check(
            self.tensor_parallelism == self.gpu_count,
            "GPU count must equal tensor parallelism in v1",
        )
        # The harness intentionally targets one physical node. This is the
        # node's total allocatable capacity, which may be larger than one
        # worker's TP degree when exercising replicas/autoscaling.
        self.node_gpu_count = bounded_int(
            "INFERSCALE_GPU_NODE_GPU_COUNT", self.gpu_count, 1, 8
        )
        check(
            self.node_gpu_count >= self.gpu_count,
            "INFERSCALE_GPU_NODE_GPU_COUNT must cover one worker",
        )
        self.prefix_caching = flag("INFERSCALE_GPU_PREFIX_CACHING", False)
        self.routing_policy = os.environ.get(
            "INFERSCALE_GPU_ROUTING_POLICY", "load-aware"
        )
        check(
            self.routing_policy in {"round-robin", "load-aware", "prefix-aware"},
            "INFERSCALE_GPU_ROUTING_POLICY must be round-robin, load-aware or prefix-aware",
        )
        check(
            self.routing_policy != "prefix-aware" or self.prefix_caching,
            "prefix-aware routing requires INFERSCALE_GPU_PREFIX_CACHING=true",
        )
        self.cache_metrics = flag("INFERSCALE_GPU_CACHE_METRICS", False)
        check(
            not self.cache_metrics or self.prefix_caching,
            "INFERSCALE_GPU_CACHE_METRICS requires prefix caching",
        )
        self.autoscaling = flag("INFERSCALE_GPU_AUTOSCALING", False)
        self.scale_to_zero = flag("INFERSCALE_GPU_SCALE_TO_ZERO", False)
        self.min_replicas = bounded_int(
            "INFERSCALE_GPU_MIN_REPLICAS", 0 if self.scale_to_zero else 1, 0, 8
        )
        check(
            (self.min_replicas == 0) == self.scale_to_zero,
            "MIN_REPLICAS=0 requires the scale-to-zero check and vice versa",
        )
        self.max_replicas = bounded_int(
            "INFERSCALE_GPU_MAX_REPLICAS", 1, self.min_replicas, 8
        )
        if self.autoscaling:
            check(
                self.max_replicas > self.min_replicas,
                "autoscaling requires INFERSCALE_GPU_MAX_REPLICAS greater than MIN_REPLICAS",
            )
            check(
                self.node_gpu_count >= self.gpu_count * self.max_replicas,
                "autoscaling requires enough GPUs for the configured maximum replicas",
            )
        self.restart = flag("INFERSCALE_GPU_CONTROLLER_RESTART", True)
        self.candidates = flag(
            "INFERSCALE_GPU_CANDIDATES", not (self.autoscaling or self.scale_to_zero)
        )
        check(
            not (self.autoscaling or self.scale_to_zero) or not self.candidates,
            "disable candidate checks when exercising autoscaling or scale-to-zero",
        )
        self.keep = flag("INFERSCALE_GPU_KEEP_DEPLOYMENT", False)
        self.kubectl = os.environ.get("KUBECTL", "kubectl")
        check(shutil.which(self.kubectl), "kubectl is required")
        self.run_id = f"local-gpu-{dt.datetime.now(dt.timezone.utc):%Y%m%dT%H%M%SZ}-{uuid.uuid4().hex[:8]}"
        self.name = self.run_id.lower()
        self.namespace = ""
        self.deployment_id = ""
        self.stage = "preflight"
        self.summary = {
            "schema_version": 1,
            "run_id": self.run_id,
            "publishable": False,
            "kind": "local-kubernetes-gpu-functional-smoke",
            "started_at": dt.datetime.now(dt.timezone.utc).isoformat(),
            "kube_context": self.context,
            "model": MODEL,
            "model_revision": REVISION,
            "gpu_type": self.gpu_type,
            "gpu_count": self.gpu_count,
            "tensor_parallelism": self.tensor_parallelism,
            "prefix_caching": self.prefix_caching,
            "routing_policy": self.routing_policy,
            "min_replicas": self.min_replicas,
            "max_replicas": self.max_replicas,
            "scale_to_zero": self.scale_to_zero,
            "checks": [],
            "excluded": [
                "performance claims",
                "healthy dual-revision canary",
                "full Gateway conformance",
            ],
        }
        if self.gpu_count == 1:
            self.summary["excluded"].append("multi-GPU scaling")
        if not self.cache_metrics:
            self.summary["excluded"].append("prefix-cache hit metrics")
        if not self.autoscaling:
            self.summary["excluded"].append("autoscaling burst behavior")

        # Ignore proxy environment variables for explicitly local requests and
        # reject redirects before a bearer credential can leave this origin.
        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, req, fp, code, msg, headers, newurl):
                return None

        self.http = urllib.request.build_opener(
            urllib.request.ProxyHandler({}), NoRedirect()
        )

    def kube(self, *args: str, timeout: int = 30) -> str:
        result = subprocess.run(
            [self.kubectl, "--context", self.context, "--request-timeout=25s", *args],
            capture_output=True,
            text=True,
            timeout=timeout,
            check=False,
        )
        check(result.returncode == 0, f"kubectl {args[0]} failed during {self.stage}")
        return result.stdout

    def objects(self, kind: str, selector: str = "") -> list[dict]:
        args = ["-n", self.namespace, "get", kind, "-o", "json"]
        if selector:
            args += ["-l", selector]
        return json.loads(self.kube(*args))["items"]

    def request(
        self,
        method: str,
        path: str,
        payload=None,
        headers=None,
        stream=False,
        timeout: int | None = None,
    ):
        request_headers = {
            "Authorization": f"Bearer {self.token}",
            "Content-Type": "application/json",
        }
        request_headers.update(headers or {})
        request = urllib.request.Request(
            self.origin + path,
            data=None if payload is None else json.dumps(payload).encode(),
            headers=request_headers,
            method=method,
        )
        try:
            started = time.monotonic()
            with self.http.open(
                request,
                timeout=timeout or (STREAM_TIMEOUT_SECONDS if stream else 60),
            ) as response:
                if stream:
                    check(
                        "text/event-stream" in response.headers.get("Content-Type", ""),
                        "missing SSE content type",
                    )
                    return validate_stream(
                        response, self.name, deadline=started + STREAM_TIMEOUT_SECONDS
                    )
                raw = response.read(MAX_RESPONSE_BYTES + 1)
                check(
                    len(raw) <= MAX_RESPONSE_BYTES, "response exceeded smoke-test bound"
                )
                return json.loads(raw) if raw else {}
        except urllib.error.HTTPError as error:
            # Do not persist bodies, headers, or urllib's potentially sensitive
            # exception rendering in diagnostics or result artifacts.
            raise RuntimeError(f"HTTP {error.code} during {self.stage}") from None
        except urllib.error.URLError:
            raise RuntimeError(
                f"local HTTP connection failed during {self.stage}"
            ) from None

    def wait(self, description: str, probe):
        self.stage = description
        print(f"Waiting for {description}...", flush=True)
        deadline = time.monotonic() + self.timeout
        while time.monotonic() < deadline:
            value = probe()
            if value:
                return value
            time.sleep(2)
        raise RuntimeError(f"timed out waiting for {description}")

    def passed(self, description: str):
        self.summary["checks"].append(description)
        print(f"PASS: {description}", flush=True)

    def cr(self):
        resources = self.objects("inferencedeployments")
        return next(
            (item for item in resources if item["metadata"]["name"] == self.name), {}
        )

    def api(self):
        return self.request("GET", f"/v1/deployments/{self.deployment_id}")

    def ready(self):
        resource = self.cr()
        status = resource.get("status", {})
        if status.get("phase") != "Ready" or status.get(
            "observedGeneration"
        ) != resource.get("metadata", {}).get("generation"):
            return False
        conditions = {
            item["type"]: item["status"] for item in status.get("conditions", [])
        }
        if not all(
            conditions.get(name) == "True"
            for name in ("RuntimeReady", "RouteReady", "ModelCached")
        ):
            return False
        check(
            status.get("cache", {}).get("weights") == "Warm",
            "real runtime did not use the model cache",
        )
        revision = status.get("revision", {})
        if not revision.get("stable") or revision.get("candidate"):
            return False
        public = self.api()
        if (
            public.get("status", {}).get("phase") != "Ready"
            or not public.get("stableRevisionId")
            or public.get("candidateRevisionId")
        ):
            return False
        return resource

    def chat(self, stream=False, prefix=False, timeout: int | None = None):
        content = "Say hello in one short sentence. /no_think"
        if prefix:
            # Cover several router index blocks (64 tokens each), as well as
            # runtime cache blocks. A tiny greeting only tests runtime reuse.
            content = "Reference material: " + "alpha beta gamma delta " * 64
            content += "\nSummarize this in one short sentence. /no_think"
        payload = {
            "model": self.name,
            "messages": [
                {
                    "role": "user",
                    "content": content,
                }
            ],
            "stream": stream,
            "max_tokens": 32,
            "temperature": 0,
        }
        if stream:
            payload["stream_options"] = {"include_usage": True}
        result = self.request(
            "POST",
            f"/v1/deployments/{self.deployment_id}/chat/completions",
            payload,
            stream=stream,
            timeout=timeout,
        )
        if stream:
            return result
        validate_chat(result, self.name)
        if prefix:
            check(
                result.get("usage", {}).get("prompt_tokens", 0) >= 128,
                "prefix workload is too short to cover router cache blocks",
            )
        return True

    def patch_context(self, length: int):
        current = self.api()
        self.request(
            "PATCH",
            f"/v1/deployments/{self.deployment_id}",
            {"runtime": {"maxModelLen": length}},
            {
                "Content-Type": "application/merge-patch+json",
                "If-Match": f'"{current["generation"]}"',
            },
        )

    def pending_candidate(self, stable: str, previous=""):
        resource = self.cr()
        status = resource.get("status", {})
        revision = status.get("revision", {})
        candidate = revision.get("candidate", "")
        if not candidate or candidate == previous:
            return False
        check(
            revision.get("stable") == stable,
            "pending candidate changed the stable revision",
        )
        pods = self.objects(
            "pods",
            f"{LABEL_REVISION}={candidate},app.kubernetes.io/component=model-server",
        )
        for pod in pods:
            for condition in pod.get("status", {}).get("conditions", []):
                if (
                    condition.get("type") == "PodScheduled"
                    and condition.get("status") == "False"
                    and condition.get("reason") == "Unschedulable"
                    and "Insufficient nvidia.com/gpu" in condition.get("message", "")
                ):
                    return candidate
        return False

    def retired(self, revision: str):
        selector = f"{LABEL_REVISION}={revision}"
        workloads = self.objects("deployments", selector)
        if any(
            item.get("spec", {}).get("replicas", 1) != 0
            or not item["metadata"]
            .get("annotations", {})
            .get("inferscale.io/retired-at")
            for item in workloads
        ):
            return False
        active_consumers = [
            pod
            for pod in self.objects("pods", selector)
            if pod["metadata"].get("labels", {}).get("app.kubernetes.io/component")
            in {"model-server", "endpoint-picker"}
        ]
        # Completed prerequisite Pods remain as rollback evidence; only the
        # worker and EPP must disappear after their Deployments reach zero.
        return not self.objects("scaledobjects", selector) and not active_consumers

    def runtime_metrics(self, worker: dict) -> str:
        """Read the runtime's bounded Prometheus exposition without pod logs."""

        body = self.kube(
            "-n",
            self.namespace,
            "exec",
            worker["metadata"]["name"],
            "-c",
            "runtime",
            "--",
            "python3",
            "-c",
            "import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:8000/metrics', timeout=10).read().decode())",
        )
        check(
            len(body.encode("utf-8")) <= MAX_RESPONSE_BYTES,
            "runtime metrics exceeded smoke-test bound",
        )
        return body

    def available_workers(self, revision: str) -> int:
        workers = self.objects(
            "deployments",
            f"{LABEL_REVISION}={revision},app.kubernetes.io/component=model-server",
        )
        return sum(
            int(worker.get("status", {}).get("availableReplicas", 0))
            for worker in workers
        )

    def exercise_autoscaling(self, revision: str) -> None:
        """Drive a short bounded burst and require a second worker to appear."""

        target = min(self.max_replicas, max(self.min_replicas + 1, 2))
        request_count = min(
            self.max_replicas * 4,
            max(self.max_replicas * 2, self.min_replicas * 2),
        )
        check(request_count > 0, "autoscaling burst must contain requests")
        self.stage = "autoscaling burst"
        print(
            f"Sending bounded autoscaling burst ({request_count} requests)...",
            flush=True,
        )
        with concurrent.futures.ThreadPoolExecutor(
            max_workers=min(request_count, 16)
        ) as executor:
            futures = [executor.submit(self.chat) for _ in range(request_count)]
            self.wait(
                f"autoscaling to at least {target} available workers",
                lambda: self.available_workers(revision) >= target,
            )
            for future in futures:
                future.result()
        self.summary["autoscaling_workers_observed"] = self.available_workers(revision)
        self.passed("bounded burst scales workers without inference errors")

    def exercise_scale_to_zero(self, revision: str) -> None:
        """Let KEDA idle the worker, then wake it with one real request."""

        selector = (
            f"{LABEL_REVISION}={revision},app.kubernetes.io/component=model-server"
        )
        initial = self.objects("pods", selector)
        initial_uids = {item["metadata"]["uid"] for item in initial}
        check(bool(initial_uids), "scale-to-zero requires an initially ready worker")

        def zero_minimum():
            scalers = self.objects("scaledobjects", f"{LABEL_REVISION}={revision}")
            return len(scalers) == 1 and scalers[0]["spec"].get("minReplicaCount") == 0

        # Initial promotion starts with min=1 to verify readiness. The next
        # reconcile restores the requested min=0; API Ready can precede it.
        self.wait("zero-minimum autoscaler after initial promotion", zero_minimum)

        def idle():
            workloads = self.objects("deployments", selector)
            return (
                len(workloads) == 1
                and workloads[0]["spec"].get("replicas") == 0
                and workloads[0].get("status", {}).get("replicas", 0) == 0
                and not self.objects("pods", selector)
            )

        started = time.monotonic()
        self.wait("KEDA idle scale-down to zero (normal cooldown)", idle)
        self.summary["scale_to_zero_idle_wait_seconds"] = round(
            time.monotonic() - started, 3
        )
        check(
            self.cr().get("status", {}).get("revision", {}).get("stable") == revision,
            "scale-down changed the stable revision",
        )
        self.passed("KEDA removes idle GPU workers without manual scaling")
        self.stage = "cold activation through Gateway/admission/EPP"
        started = time.monotonic()
        # The serving contract allows 900 seconds for a queued cold request.
        # No direct replica patch, metrics injection, or retry may wake it.
        self.chat(timeout=900)
        elapsed = time.monotonic() - started
        check(elapsed <= 900, "cold activation exceeded the serving deadline")
        self.wait("ready worker after cold activation", self.ready)
        awakened = self.objects("pods", selector)
        check(
            bool(awakened)
            and all(item["metadata"]["uid"] not in initial_uids for item in awakened),
            "cold activation did not create a new worker",
        )
        self.summary["scale_to_zero_activation_seconds"] = round(elapsed, 3)
        self.summary["scale_to_zero_cache_state"] = "cache-warm"
        self.passed(
            "one queued inference request activates a new GPU worker and completes"
        )

    def run(self):
        config = json.loads(
            self.kube(
                "-n",
                "inferscale-system",
                "get",
                "configmap",
                "inferscale-config",
                "-o",
                "json",
            )
        )["data"]
        check(
            config.get("INFERSCALE_FAKE_RUNTIME", "false") == "false",
            "selected cluster still enables the CPU fake runtime",
        )
        check(
            not self.scale_to_zero
            or config.get("INFERSCALE_FEATURE_SCALE_TO_ZERO") == "true",
            "scale-to-zero requires the cluster's scale-to-zero feature gate",
        )
        nodes = json.loads(self.kube("get", "nodes", "-o", "json"))["items"]
        gpu_nodes = [
            node
            for node in nodes
            if int(
                node.get("status", {}).get("allocatable", {}).get("nvidia.com/gpu", "0")
            )
            > 0
        ]
        check(
            len(gpu_nodes) == 1
            and int(gpu_nodes[0]["status"]["allocatable"]["nvidia.com/gpu"])
            == self.node_gpu_count,
            "smoke requires one physical GPU node with the configured allocatable GPU count",
        )
        check(
            gpu_nodes[0]["metadata"].get("labels", {}).get("inferscale.io/gpu-sku")
            == self.gpu_type,
            f"GPU node lacks the {self.gpu_type} SKU label",
        )
        expected_image = base64.b64decode(
            self.kube(
                "-n",
                "inferscale-system",
                "get",
                "secret",
                "inferscale-storage",
                "-o",
                "jsonpath={.data.runtime_vllm_image}",
            ).strip()
        ).decode()
        check(
            expected_image and "fake-runtime" not in expected_image,
            "configure a real vLLM wrapper image",
        )
        self.summary["runtime_image"] = expected_image
        self.passed("real GPU cluster prerequisites")

        self.stage = "deployment creation"
        created = self.request(
            "POST",
            "/v1/deployments",
            {
                "name": self.name,
                "model": {"uri": MODEL, "revision": REVISION},
                "backend": "vllm",
                "precision": "bf16",
                "quantization": "none",
                "gpu": {"type": self.gpu_type, "count": self.gpu_count},
                "tensor_parallelism": self.tensor_parallelism,
                "max_model_len": 2048,
                "prefix_caching": self.prefix_caching,
                "min_replicas": self.min_replicas,
                "max_replicas": self.max_replicas,
                "admission": {
                    "maxConcurrentRequests": 2,
                    "maxQueuedRequests": 4,
                    "priorityClass": "standard",
                },
                "routing": {"policy": self.routing_policy},
                "rollout": {"strategy": "progressive", "shadowPercent": 10},
                "observability": {"tracing": True},
            },
            {"Idempotency-Key": self.run_id},
        )["deployment"]
        self.deployment_id, self.namespace = created["id"], created["namespace"]
        self.summary.update(
            deployment_id=self.deployment_id,
            namespace=self.namespace,
            deployment_name=self.name,
        )
        resource = self.wait(
            "real runtime, cache, Gateway and API readiness", self.ready
        )
        stable = resource["status"]["revision"]["stable"]
        self.summary["stable_revision"] = stable
        workers = self.objects(
            "pods",
            f"{LABEL_REVISION}={stable},app.kubernetes.io/component=model-server",
        )
        check(len(workers) == 1, "expected one stable GPU worker Deployment")
        worker = workers[0]
        runtime = next(
            container
            for container in worker["spec"]["containers"]
            if container["name"] == "runtime"
        )
        check(
            runtime["image"] == expected_image,
            "worker image differs from configured real runtime",
        )
        check(
            str(runtime["resources"]["limits"].get("nvidia.com/gpu"))
            == str(self.gpu_count),
            "worker lacks the configured exclusive GPU limit",
        )
        environment = {item["name"]: item.get("value") for item in runtime["env"]}
        check(
            environment.get("PREFIX_CACHING") == str(self.prefix_caching).lower()
            and environment.get("TENSOR_PARALLELISM") == str(self.tensor_parallelism),
            "worker did not receive the requested cache or tensor-parallel settings",
        )
        process = json.loads(
            self.kube(
                "-n",
                self.namespace,
                "exec",
                worker["metadata"]["name"],
                "-c",
                "runtime",
                "--",
                "python3",
                "-c",
                "import json; from pathlib import Path; print(json.dumps(Path('/proc/1/cmdline').read_bytes().decode().split(chr(0))))",
            )
        )
        expected_prefix_flag = (
            "--enable-prefix-caching"
            if self.prefix_caching
            else "--no-enable-prefix-caching"
        )
        opposite_prefix_flag = (
            "--no-enable-prefix-caching"
            if self.prefix_caching
            else "--enable-prefix-caching"
        )
        check(
            expected_prefix_flag in process and opposite_prefix_flag not in process,
            "vLLM process did not explicitly receive the requested prefix-cache setting",
        )
        self.summary["runtime_image_id"] = next(
            entry["imageID"]
            for entry in worker["status"]["containerStatuses"]
            if entry["name"] == "runtime"
        )
        jobs = self.objects(
            "jobs",
            f"{LABEL_REVISION}={stable},app.kubernetes.io/component=model-prefetch",
        )
        check(
            any(job.get("status", {}).get("succeeded", 0) > 0 for job in jobs),
            "missing completed real prefetch job",
        )
        device = self.kube(
            "-n",
            self.namespace,
            "exec",
            worker["metadata"]["name"],
            "-c",
            "runtime",
            "--",
            "nvidia-smi",
            "--query-gpu=name,driver_version,memory.total",
            "--format=csv,noheader,nounits",
        ).strip()
        device_lines = [line for line in device.splitlines() if line.strip()]
        check(
            len(device_lines) == self.gpu_count
            and all(gpu_model_matches(self.gpu_type, line) for line in device_lines),
            "runtime does not see the configured number and type of GPUs",
        )
        self.summary["gpu_observation"] = device
        self.passed(
            f"real prefetch and exclusive {self.gpu_type} runtime become Ready with TP={self.tensor_parallelism} and prefix caching {'enabled' if self.prefix_caching else 'disabled'}"
        )
        self.stage = "JSON inference"
        self.chat()
        self.passed("authenticated JSON inference through Gateway/admission/EPP")
        self.stage = "streaming inference"
        stream = self.chat(stream=True)
        self.summary["stream_events"] = stream["events"]
        self.summary["stream_generated_events"] = stream["generated_events"]
        self.summary["stream_usage"] = stream["usage"]
        self.passed(
            "authenticated incremental streaming inference with positive usage and terminal SSE marker"
        )

        if self.prefix_caching:
            self.stage = "prefix cache reuse"
            # Identical prompts make the second request eligible for a local
            # prefix hit. The counter check is opt-in because vLLM metric
            # names and scrape availability are runtime-image contracts.
            self.chat(prefix=True)
            before = (
                prefix_cache_metrics(self.runtime_metrics(worker))
                if self.cache_metrics
                else None
            )
            self.chat(prefix=True)
            if self.cache_metrics:
                after = prefix_cache_metrics(self.runtime_metrics(worker))
                check(
                    after["hits"] > before["hits"],
                    "repeated prefix produced no new cache hits",
                )
                check(
                    after["queries"] > before["queries"],
                    "prefix query counter did not advance",
                )
                self.summary["prefix_cache_metrics"] = after
                self.summary["prefix_cache_hit_delta"] = after["hits"] - before["hits"]
                self.passed("repeated inference produces positive prefix-cache metrics")
            else:
                self.passed("prefix caching remains enabled for repeated inference")

        if self.scale_to_zero:
            self.exercise_scale_to_zero(stable)

        if self.autoscaling:
            self.exercise_autoscaling(stable)

        if self.restart:
            self.stage = "controller restart"
            self.kube(
                "-n",
                "inferscale-system",
                "rollout",
                "restart",
                "deployment/inferscale-controller",
            )
            self.kube(
                "-n",
                "inferscale-system",
                "rollout",
                "status",
                "deployment/inferscale-controller",
                f"--timeout={self.timeout}s",
                timeout=self.timeout + 10,
            )
            observed = self.wait("readiness after controller restart", self.ready)
            check(
                observed["status"]["revision"]["stable"] == stable,
                "restart changed the stable revision",
            )
            self.chat()
            self.passed("controller restart preserves stable revision and inference")
        if self.candidates:
            self.stage = "first candidate update"
            self.patch_context(1536)
            first = self.wait(
                "candidate pending on the occupied GPU",
                lambda: self.pending_candidate(stable),
            )
            self.chat()
            self.passed("pending GPU candidate preserves stable inference")
            self.stage = "superseding candidate update"
            self.patch_context(1024)
            second = self.wait(
                "replacement candidate pending",
                lambda: self.pending_candidate(stable, first),
            )
            self.wait("superseded candidate retirement", lambda: self.retired(first))
            self.chat()
            self.passed("superseded candidate releases worker, EPP and autoscaler")
            self.stage = "operator abort"
            self.kube(
                "-n",
                self.namespace,
                "annotate",
                "inferencedeployment",
                self.name,
                "inferscale.io/rollout-control=abort",
                "--overwrite",
            )
            self.wait("aborted candidate retirement", lambda: self.retired(second))
            observed = self.cr()["status"]
            check(
                observed["revision"]["stable"] == stable
                and observed["rollout"]["stage"] == "Failed",
                "abort did not retain stable with a durable failed rollout",
            )
            self.chat()
            self.passed("pending-candidate abort retains stable inference")

    def finish(self, success: bool):
        self.summary["status"] = "passed" if success else "failed"
        self.summary["last_stage"] = self.stage
        if self.deployment_id and not self.keep:
            try:
                self.stage = "test deployment cleanup"
                self.request("DELETE", f"/v1/deployments/{self.deployment_id}")
                self.wait("test deployment deletion", lambda: not self.cr())
                self.summary["cleanup"] = "deleted"
            except EXPECTED_FAILURES:
                self.summary["cleanup"] = "failed; test deployment retained"
                self.summary["status"] = "failed"
        elif self.deployment_id:
            self.summary["cleanup"] = "retained by configuration"
        self.summary["finished_at"] = dt.datetime.now(dt.timezone.utc).isoformat()
        root = Path(__file__).resolve().parents[2]
        output_dir = Path(
            os.environ.get(
                "INFERSCALE_GPU_RESULTS_DIR", str(root / "benchmarks/results")
            )
        )
        output_dir.mkdir(parents=True, exist_ok=True)
        output = output_dir / f"{self.run_id}.json"
        temporary = output.with_suffix(".tmp")
        temporary.write_text(json.dumps(self.summary, indent=2) + "\n")
        temporary.replace(output)
        print(f"Non-publishable result: {output}", flush=True)
        return self.summary["status"] == "passed"


def main() -> int:
    os.umask(0o077)
    with tempfile.TemporaryDirectory(prefix="inferscale-gpu-smoke-") as directory:
        smoke = Smoke(Path(directory))
        success = False
        try:
            smoke.run()
            success = True
        except EXPECTED_FAILURES as error:
            # Only our controlled RuntimeError messages are safe to print.
            message = (
                str(error) if type(error) is RuntimeError else type(error).__name__
            )
            print(f"FAIL during {smoke.stage}: {message}", file=sys.stderr)
            smoke.summary["failure"] = message
        finally:
            success = smoke.finish(success)
        return 0 if success else 1


if __name__ == "__main__":
    try:
        sys.exit(main())
    except EXPECTED_FAILURES as error:
        print(
            str(error)
            if type(error) is RuntimeError
            else f"GPU smoke setup failed: {type(error).__name__}",
            file=sys.stderr,
        )
        sys.exit(1)
