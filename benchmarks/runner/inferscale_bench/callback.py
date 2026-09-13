from __future__ import annotations

import json
import os
import re
import time
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Mapping, Sequence

BENCHMARK_TOKEN_ENV = "INFERSCALE_BENCHMARK_TOKEN"
MAX_REPORTED_ERROR_CHARS = 1024
_BEARER_PATTERN = re.compile(r"(?i)bearer\s+[a-z0-9._~+/=-]+")


class CallbackError(RuntimeError):
    """A callback failed without exposing its response body or credential."""


def bounded_error(error: BaseException | str, secrets: Sequence[str] = ()) -> str:
    """Return a single-line, size-bounded error safe for persistence.

    Benchmark failures are retained in PostgreSQL. Strip known credentials and
    bearer-shaped values before producing that durable error text.
    """

    message = str(error)
    for secret in secrets:
        if secret:
            message = message.replace(secret, "[REDACTED]")
    message = _BEARER_PATTERN.sub("Bearer [REDACTED]", message)
    message = " ".join(message.split())
    if not message:
        message = (
            type(error).__name__
            if isinstance(error, BaseException)
            else "benchmark failed"
        )
    if len(message) > MAX_REPORTED_ERROR_CHARS:
        message = message[: MAX_REPORTED_ERROR_CHARS - 3] + "..."
    return message


def post_result(
    callback_url: str,
    run_id: str,
    report: Mapping[str, object],
    *,
    token: str | None = None,
    timeout_s: float = 15.0,
    attempts: int = 3,
) -> None:
    """Post one authenticated terminal result to the management API."""

    parsed = urllib.parse.urlsplit(callback_url)
    if parsed.scheme not in {"http", "https"} or not parsed.netloc or parsed.username:
        raise CallbackError(
            "benchmark callback must be an absolute HTTP(S) URL without user info"
        )
    if not run_id.strip():
        raise CallbackError("benchmark run ID is required")
    if attempts <= 0:
        raise CallbackError("benchmark callback attempts must be positive")
    credential = token if token is not None else os.getenv(BENCHMARK_TOKEN_ENV, "")
    if not credential:
        raise CallbackError(f"{BENCHMARK_TOKEN_ENV} is unset")
    body = json.dumps(dict(report), allow_nan=False, separators=(",", ":")).encode(
        "utf-8"
    )
    request = urllib.request.Request(
        callback_url,
        data=body,
        headers={
            "Authorization": f"Bearer {credential}",
            "Content-Type": "application/json",
            "User-Agent": "inferscale-bench/0.1",
            "X-Inferscale-Benchmark-Run": run_id,
        },
        method="POST",
    )
    failure: CallbackError | None = None
    for attempt in range(attempts):
        try:
            with urllib.request.urlopen(request, timeout=timeout_s) as response:
                if 200 <= response.status < 300:
                    return
                failure = CallbackError(
                    f"benchmark callback returned HTTP {response.status}"
                )
                retryable = response.status == 429 or response.status >= 500
        except urllib.error.HTTPError as exc:
            # Never copy the API response body into logs: it may contain
            # details supplied by another component or future extension.
            failure = CallbackError(f"benchmark callback returned HTTP {exc.code}")
            retryable = exc.code == 429 or exc.code >= 500
        except urllib.error.URLError as exc:
            failure = CallbackError(
                f"benchmark callback transport failed: {type(exc.reason).__name__}"
            )
            retryable = True
        if not retryable or attempt == attempts - 1:
            assert failure is not None
            raise failure
        time.sleep(0.25 * (attempt + 1))
    raise CallbackError("benchmark callback failed")
