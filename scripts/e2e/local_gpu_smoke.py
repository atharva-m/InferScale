"""Bounded real-GPU Kubernetes checks; never retain request/response content."""

from __future__ import annotations

import base64
import datetime as dt
import io
import ipaddress
import json
import os
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
        self.restart = flag("INFERSCALE_GPU_CONTROLLER_RESTART", True)
        self.candidates = flag("INFERSCALE_GPU_CANDIDATES", True)
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
            "gpu_type": GPU,
            "checks": [],
            "excluded": [
                "performance claims",
                "multi-GPU scaling",
                "healthy dual-revision canary",
                "full Gateway conformance",
            ],
        }

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

    def request(self, method: str, path: str, payload=None, headers=None, stream=False):
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
                request, timeout=STREAM_TIMEOUT_SECONDS if stream else 60
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

    def chat(self, stream=False):
        payload = {
            "model": self.name,
            "messages": [
                {
                    "role": "user",
                    "content": "Say hello in one short sentence. /no_think",
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
        )
        if stream:
            return result
        validate_chat(result, self.name)
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
            and int(gpu_nodes[0]["status"]["allocatable"]["nvidia.com/gpu"]) == 1,
            "smoke requires exactly one allocatable exclusive GPU",
        )
        check(
            gpu_nodes[0]["metadata"].get("labels", {}).get("inferscale.io/gpu-sku")
            == GPU,
            "GPU node lacks the RTX_4070 SKU label",
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
                "gpu": {"type": GPU, "count": 1},
                "tensor_parallelism": 1,
                "max_model_len": 2048,
                "prefix_caching": False,
                "min_replicas": 1,
                "max_replicas": 1,
                "admission": {
                    "maxConcurrentRequests": 2,
                    "maxQueuedRequests": 4,
                    "priorityClass": "standard",
                },
                "routing": {"policy": "load-aware"},
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
        check(len(workers) == 1, "expected one stable GPU worker")
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
            str(runtime["resources"]["limits"].get("nvidia.com/gpu")) == "1",
            "worker lacks one exclusive GPU limit",
        )
        environment = {item["name"]: item.get("value") for item in runtime["env"]}
        check(
            environment.get("PREFIX_CACHING") == "false",
            "worker did not receive prefix caching false",
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
        check(
            "--no-enable-prefix-caching" in process
            and "--enable-prefix-caching" not in process,
            "vLLM process did not explicitly disable prefix caching",
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
        check(
            "4070" in device and len(device.splitlines()) == 1,
            "runtime does not see exactly one RTX 4070",
        )
        self.summary["gpu_observation"] = device
        self.passed(
            "real prefetch and exclusive RTX 4070 runtime become Ready with prefix caching disabled"
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
