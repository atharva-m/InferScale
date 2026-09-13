from __future__ import annotations

import json
import math
import ssl
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from collections.abc import Iterable
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import asdict, dataclass

from .scenario import Scenario


@dataclass
class RequestResult:
    index: int
    ok: bool
    status_code: int | None
    ttft_ms: float | None
    tpot_ms: float | None
    e2e_ms: float
    prompt_tokens: int
    output_tokens: int
    output_tokens_per_s: float | None
    error_code: str | None = None
    error: str | None = None

    def to_dict(self) -> dict[str, object]:
        return asdict(self)


@dataclass(frozen=True)
class MeasurementInterval:
    start: float
    end: float

    def __post_init__(self) -> None:
        if (
            not math.isfinite(self.start)
            or not math.isfinite(self.end)
            or self.start <= 0
            or self.end <= self.start
        ):
            raise ValueError(
                "measurement interval must have finite, ordered timestamps"
            )


@dataclass(frozen=True)
class MeasuredRun:
    requests: list[RequestResult]
    wall_time_s: float
    interval: MeasurementInterval


def parse_sse_lines(lines: Iterable[bytes]) -> Iterable[dict[str, object]]:
    data_lines: list[str] = []
    for raw_line in lines:
        line = raw_line.decode("utf-8", errors="strict").rstrip("\r\n")
        if not line:
            if data_lines:
                payload = "\n".join(data_lines)
                data_lines.clear()
                if payload == "[DONE]":
                    return
                parsed = json.loads(payload)
                if isinstance(parsed, dict):
                    yield parsed
            continue
        if line.startswith(":"):
            continue
        if line.startswith("data:"):
            data_lines.append(line[5:].lstrip())
    if data_lines:
        payload = "\n".join(data_lines)
        if payload != "[DONE]":
            parsed = json.loads(payload)
            if isinstance(parsed, dict):
                yield parsed


def request_payload(
    scenario: Scenario, messages: list[dict[str, str]]
) -> dict[str, object]:
    return {
        "model": scenario.target.deployment,
        "messages": messages,
        "max_tokens": scenario.workload.output_tokens,
        "temperature": 0,
        "stream": True,
        "stream_options": {"include_usage": True},
    }


def request_endpoint(scenario: Scenario, deployment_id: str | None) -> str:
    if scenario.target.api_mode == "openai":
        return f"{scenario.target.base_url}/v1/chat/completions"
    if not deployment_id:
        raise ValueError("InferScale deployment UUID is required")
    try:
        parsed_id = uuid.UUID(deployment_id)
    except ValueError as exc:
        raise ValueError("InferScale deployment ID must be a UUID") from exc
    if parsed_id.version != 7:
        raise ValueError("InferScale deployment ID must be a UUIDv7")
    return (
        f"{scenario.target.base_url}/v1/deployments/"
        f"{urllib.parse.quote(deployment_id, safe='')}/chat/completions"
    )


def _request_once(
    index: int,
    scenario: Scenario,
    messages: list[dict[str, str]],
    api_key: str | None,
    deployment_id: str | None,
) -> RequestResult:
    endpoint = request_endpoint(scenario, deployment_id)
    payload = request_payload(scenario, messages)
    headers = {"Content-Type": "application/json", "Accept": "text/event-stream"}
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"
    request = urllib.request.Request(
        endpoint,
        data=json.dumps(payload).encode("utf-8"),
        headers=headers,
        method="POST",
    )
    ssl_context = None
    if endpoint.startswith("https://") and not scenario.target.verify_tls:
        ssl_context = ssl._create_unverified_context()
    started = time.perf_counter()
    first_token_at: float | None = None
    last_token_at: float | None = None
    chunks = 0
    prompt_tokens = 0
    output_tokens = 0
    status_code: int | None = None
    try:
        with urllib.request.urlopen(
            request, timeout=scenario.target.request_timeout_s, context=ssl_context
        ) as response:
            status_code = response.status
            for event in parse_sse_lines(response):
                usage = event.get("usage")
                if isinstance(usage, dict):
                    prompt_tokens = int(usage.get("prompt_tokens", prompt_tokens) or 0)
                    output_tokens = int(
                        usage.get("completion_tokens", output_tokens) or 0
                    )
                choices = event.get("choices")
                if not isinstance(choices, list):
                    continue
                for choice in choices:
                    delta = choice.get("delta") if isinstance(choice, dict) else None
                    content = delta.get("content") if isinstance(delta, dict) else None
                    if isinstance(content, str) and content:
                        now = time.perf_counter()
                        first_token_at = first_token_at or now
                        last_token_at = now
                        chunks += 1
        finished = time.perf_counter()
        if first_token_at is None:
            raise ValueError("stream completed without a content token")
        if output_tokens <= 0:
            output_tokens = chunks
        generated_s = max((last_token_at or finished) - first_token_at, 0.0)
        tpot_ms = None
        if output_tokens > 1 and generated_s > 0:
            tpot_ms = generated_s * 1000 / (output_tokens - 1)
        output_rate = output_tokens / generated_s if generated_s > 0 else None
        return RequestResult(
            index=index,
            ok=True,
            status_code=status_code,
            ttft_ms=(first_token_at - started) * 1000,
            tpot_ms=tpot_ms,
            e2e_ms=(finished - started) * 1000,
            prompt_tokens=prompt_tokens,
            output_tokens=output_tokens,
            output_tokens_per_s=output_rate,
        )
    except urllib.error.HTTPError as exc:
        finished = time.perf_counter()
        return RequestResult(
            index,
            False,
            exc.code,
            None,
            None,
            (finished - started) * 1000,
            0,
            0,
            None,
            "http_error",
            "upstream rejected the benchmark request; response body omitted",
        )
    except Exception as exc:  # noqa: BLE001 - one failed sample must not abort a reproducible run
        finished = time.perf_counter()
        return RequestResult(
            index,
            False,
            status_code,
            None,
            None,
            (finished - started) * 1000,
            0,
            0,
            None,
            type(exc).__name__,
            str(exc),
        )


def run_requests(
    scenario: Scenario,
    messages: list[list[dict[str, str]]],
    api_key: str | None,
    deployment_id: str | None = None,
) -> MeasuredRun:
    started_at = time.time()
    started = time.perf_counter()
    results: list[RequestResult] = []
    with ThreadPoolExecutor(max_workers=scenario.workload.concurrency) as executor:
        futures = {
            executor.submit(
                _request_once, index, scenario, request_messages, api_key, deployment_id
            ): index
            for index, request_messages in enumerate(messages)
        }
        for future in as_completed(futures):
            results.append(future.result())
    elapsed = time.perf_counter() - started
    return MeasuredRun(
        sorted(results, key=lambda item: item.index),
        elapsed,
        MeasurementInterval(started_at, started_at + elapsed),
    )
