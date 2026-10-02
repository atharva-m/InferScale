from __future__ import annotations

import json
import math
import os
import stat
import subprocess
import tempfile
import urllib.parse
import uuid
from pathlib import Path
from typing import Any

import yaml

from .client import MeasuredRun, MeasurementInterval, RequestResult
from .scenario import GUIDELLM_TOOL, Scenario


class GuideLLMError(ValueError):
    pass


def _backend_target(scenario: Scenario, deployment_id: str) -> tuple[str, str]:
    if scenario.target.api_mode == "inferscale":
        try:
            parsed_id = uuid.UUID(deployment_id)
        except ValueError as exc:
            raise GuideLLMError("InferScale deployment ID must be a UUID") from exc
        if parsed_id.version != 7:
            raise GuideLLMError("InferScale deployment ID must be a UUIDv7")
        route = (
            f"/v1/deployments/{urllib.parse.quote(deployment_id, safe='')}"
            "/chat/completions"
        )
        return scenario.target.base_url, route
    return scenario.target.base_url, "/v1/chat/completions"


def build_config(
    scenario: Scenario,
    api_key: str | None,
    deployment_id: str,
    output_path: Path,
) -> dict[str, object]:
    """Build the pinned GuideLLM 0.7 run contract.

    The caller writes this object to a private file. In particular, the bearer
    token must never be converted to an inline CLI argument.
    """

    if scenario.measurement_tool != GUIDELLM_TOOL:
        raise GuideLLMError("GuideLLM received a scenario for another tool")
    if scenario.workload.dataset != "synthetic":
        raise GuideLLMError("GuideLLM requires the exact synthetic_text workload")
    target, request_route = _backend_target(scenario, deployment_id)
    backend: dict[str, object] = {
        "kind": "openai_http",
        "target": target,
        "model": scenario.target.deployment,
        "request_format": "/v1/chat/completions",
        "api_routes": {"/v1/chat/completions": request_route.lstrip("/")},
        "validate_backend": scenario.target.api_mode != "inferscale",
        "stream": True,
        "verify": scenario.target.verify_tls,
        "timeout": scenario.target.request_timeout_s,
        "extras": {
            "body": {
                "temperature": 0,
                "stream_options": {"include_usage": True},
            }
        },
    }
    if api_key:
        backend["api_key"] = api_key
    workload = scenario.workload
    spec = {
        "backend": backend,
        "data": [
            {
                "kind": "synthetic_text",
                "prompt_tokens": workload.input_tokens,
                # GuideLLM 0.7 accepts only positive stdev values or None.
                # Equal bounds make the sampler deterministic without stdev.
                "prompt_tokens_min": workload.input_tokens,
                "prompt_tokens_max": workload.input_tokens,
                "output_tokens": workload.output_tokens,
                "output_tokens_min": workload.output_tokens,
                "output_tokens_max": workload.output_tokens,
            }
        ],
        "tokenizer": {
            "kind": "huggingface_auto",
            "model": scenario.deployment.model,
            "load_kwargs": {
                "revision": scenario.deployment.model_revision,
                "trust_remote_code": False,
            },
        },
        "data_loader": {"kind": "pytorch", "samples": workload.requests},
        "profile": {"kind": "concurrent"},
        "constraints": [
            {"kind": "max_requests", "count": workload.requests},
            {"kind": "max_errors", "count": 1},
        ],
        "seed": {"kind": "static", "value": workload.seed},
        "outputs": [{"kind": "json", "path": str(output_path)}],
    }
    # GuideLLM 0.7 scenario files wrap arguments in spec and apply each
    # benchmark override before validating its concurrent profile.
    return {"spec": spec, "benchmarks": [{"profile.streams": [workload.concurrency]}]}


def _mapping(value: object, name: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise GuideLLMError(f"GuideLLM output field {name} must be an object")
    return value


def _list(value: object, name: str) -> list[Any]:
    if not isinstance(value, list):
        raise GuideLLMError(f"GuideLLM output field {name} must be an array")
    return value


def _number(value: object, name: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise GuideLLMError(f"GuideLLM output field {name} must be numeric")
    result = float(value)
    if not math.isfinite(result) or result < 0:
        raise GuideLLMError(
            f"GuideLLM output field {name} must be finite and non-negative"
        )
    return result


def _integer(value: object, name: str) -> int:
    number = _number(value, name)
    if not number.is_integer():
        raise GuideLLMError(f"GuideLLM output field {name} must be an integer")
    return int(number)


def _request_result(raw: object, index: int, ok: bool) -> RequestResult:
    request = _mapping(raw, f"requests[{index}]")
    e2e_ms = _number(request.get("request_latency"), "request_latency") * 1000
    if not ok:
        return RequestResult(
            index=index,
            ok=False,
            status_code=None,
            ttft_ms=None,
            tpot_ms=None,
            e2e_ms=e2e_ms,
            prompt_tokens=0,
            output_tokens=0,
            output_tokens_per_s=None,
            error_code="guidellm_request_failed",
            error="GuideLLM recorded an incomplete or errored request",
        )
    prompt_tokens = _integer(request.get("prompt_tokens"), "prompt_tokens")
    output_tokens = _integer(request.get("output_tokens"), "output_tokens")
    ttft_ms = _number(request.get("time_to_first_token_ms"), "time_to_first_token_ms")
    tpot_ms = _number(
        request.get("time_per_output_token_ms"), "time_per_output_token_ms"
    )
    output_rate = _number(
        request.get("output_tokens_per_second"), "output_tokens_per_second"
    )
    return RequestResult(
        index=index,
        ok=True,
        status_code=200,
        ttft_ms=ttft_ms,
        tpot_ms=tpot_ms,
        e2e_ms=e2e_ms,
        prompt_tokens=prompt_tokens,
        output_tokens=output_tokens,
        output_tokens_per_s=output_rate,
    )


def parse_report(path: Path, scenario: Scenario) -> MeasuredRun:
    """Strictly parse the GuideLLM 0.7 report schema into native result rows."""

    try:
        raw: object = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise GuideLLMError(f"cannot read GuideLLM JSON output: {exc}") from exc
    report = _mapping(raw, "root")
    metadata = _mapping(report.get("metadata"), "metadata")
    if metadata.get("version") != 2:
        raise GuideLLMError("GuideLLM report schema version must be 2")
    if metadata.get("guidellm_version") != "0.7.0":
        raise GuideLLMError("GuideLLM report was not produced by version 0.7.0")
    benchmarks = _list(report.get("benchmarks"), "benchmarks")
    if len(benchmarks) != 1:
        raise GuideLLMError("the fixed-concurrency contract requires one benchmark")
    benchmark = _mapping(benchmarks[0], "benchmarks[0]")
    if benchmark.get("type") != "generative_benchmark":
        raise GuideLLMError("GuideLLM benchmark type must be generative_benchmark")
    duration = _number(benchmark.get("duration"), "duration")
    if duration <= 0:
        raise GuideLLMError("GuideLLM benchmark duration must be positive")
    # GuideLLM 0.7 exposes these as the scheduler's measured interval, excluding
    # tool startup, dataset preparation, warmup, cooldown, and report rendering.
    started_at = _number(benchmark.get("start_time"), "start_time")
    ended_at = _number(benchmark.get("end_time"), "end_time")
    if (
        started_at <= 0
        or ended_at <= started_at
        or not math.isclose(
            ended_at - started_at, duration, rel_tol=1e-6, abs_tol=0.001
        )
    ):
        raise GuideLLMError("GuideLLM measurement interval does not match duration")
    interval = MeasurementInterval(started_at, ended_at)
    metrics = _mapping(benchmark.get("metrics"), "metrics")
    totals = _mapping(metrics.get("request_totals"), "metrics.request_totals")
    successful_count = _integer(totals.get("successful"), "successful requests")
    incomplete_count = _integer(totals.get("incomplete"), "incomplete requests")
    errored_count = _integer(totals.get("errored"), "errored requests")
    total_count = _integer(totals.get("total"), "total requests")
    if total_count != scenario.workload.requests:
        raise GuideLLMError(
            "GuideLLM request total does not match the scenario contract"
        )
    if successful_count + incomplete_count + errored_count != total_count:
        raise GuideLLMError("GuideLLM request status totals are inconsistent")
    requests = _mapping(benchmark.get("requests"), "requests")
    successful = _list(requests.get("successful"), "requests.successful")
    incomplete = _list(requests.get("incomplete"), "requests.incomplete")
    errored = _list(requests.get("errored"), "requests.errored")
    if (len(successful), len(incomplete), len(errored)) != (
        successful_count,
        incomplete_count,
        errored_count,
    ):
        raise GuideLLMError(
            "GuideLLM must retain every request row for contract validation"
        )
    results: list[RequestResult] = []
    for raw_request in successful:
        result = _request_result(raw_request, len(results), True)
        if result.prompt_tokens != scenario.workload.input_tokens:
            raise GuideLLMError(
                "observed prompt tokens do not match the exact synthetic contract"
            )
        if result.output_tokens != scenario.workload.output_tokens:
            raise GuideLLMError(
                "observed output tokens do not match the exact synthetic contract"
            )
        results.append(result)
    for raw_request in [*incomplete, *errored]:
        results.append(_request_result(raw_request, len(results), False))
    return MeasuredRun(results, duration, interval)


def _safe_environment(api_key: str | None) -> dict[str, str]:
    environment = {
        key: value
        for key, value in os.environ.items()
        if not key.endswith("_API_KEY") and key != "INFERSCALE_BENCHMARK_TOKEN"
    }
    if api_key:
        # The key exists only in the mode-0600 config, never argv or child env.
        environment.pop("OPENAI_API_KEY", None)
    return environment


def run(
    scenario: Scenario,
    api_key: str | None,
    deployment_id: str,
) -> MeasuredRun:
    """Run GuideLLM without exposing credentials in argv or retained artifacts."""

    with tempfile.TemporaryDirectory(prefix="inferscale-guidellm-") as directory:
        private_dir = Path(directory)
        private_dir.chmod(stat.S_IRWXU)
        config_path = private_dir / "scenario.yaml"
        output_path = private_dir / "report.json"
        config = build_config(scenario, api_key, deployment_id, output_path)
        descriptor = os.open(
            config_path,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL,
            stat.S_IRUSR | stat.S_IWUSR,
        )
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            yaml.safe_dump(config, handle, sort_keys=True)
        if stat.S_IMODE(config_path.stat().st_mode) != 0o600:
            raise GuideLLMError("GuideLLM credential config is not mode 0600")
        command = ["guidellm", "run", "--disable-console", "--config", str(config_path)]
        try:
            completed = subprocess.run(
                command,
                check=False,
                capture_output=True,
                text=True,
                env=_safe_environment(api_key),
                timeout=max(
                    120.0,
                    scenario.target.request_timeout_s
                    * max(
                        2, scenario.workload.requests / scenario.workload.concurrency
                    ),
                ),
            )
        except (OSError, subprocess.SubprocessError) as exc:
            raise GuideLLMError(
                f"GuideLLM execution failed: {type(exc).__name__}"
            ) from exc
        if completed.returncode != 0:
            raise GuideLLMError(
                f"GuideLLM exited with code {completed.returncode}; "
                "diagnostics withheld to prevent prompt or completion disclosure"
            )
        return parse_report(output_path, scenario)
