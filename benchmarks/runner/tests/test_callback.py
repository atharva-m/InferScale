import io
import json
import urllib.error
import urllib.request
from email.message import Message
from typing import cast

import pytest
from inferscale_bench.callback import CallbackError, bounded_error, post_result


class Response:
    status = 200

    def __enter__(self) -> "Response":  # noqa: PYI034
        return self

    def __exit__(self, *_: object) -> None:
        return None


def test_post_result_uses_bearer_and_never_places_token_in_json(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured: dict[str, object] = {}

    def urlopen(request: object, timeout: float) -> Response:
        captured["request"] = request
        captured["timeout"] = timeout
        return Response()

    monkeypatch.setattr("inferscale_bench.callback.urllib.request.urlopen", urlopen)
    post_result(
        "http://api.example/internal/v1/benchmarks/run-1/result",
        "run-1",
        {"state": "failed", "error": "worker failed"},
        token="signed-secret",
    )
    request = cast(urllib.request.Request, captured["request"])
    assert request.get_header("Authorization") == "Bearer signed-secret"
    assert request.get_header("X-inferscale-benchmark-run") == "run-1"
    assert isinstance(request.data, bytes)
    assert b"signed-secret" not in request.data
    assert json.loads(request.data) == {"state": "failed", "error": "worker failed"}


def test_http_callback_error_does_not_include_response_body(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    def urlopen(*_: object, **__: object) -> Response:
        raise urllib.error.HTTPError(
            "http://api.example/callback",
            400,
            "bad request",
            Message(),
            io.BytesIO(b"sensitive server detail"),
        )

    monkeypatch.setattr("inferscale_bench.callback.urllib.request.urlopen", urlopen)
    with pytest.raises(CallbackError, match="HTTP 400") as caught:
        post_result(
            "http://api.example/callback", "run-1", {"state": "failed"}, token="token"
        )
    assert "sensitive" not in str(caught.value)


def test_bounded_error_redacts_credentials_and_newlines() -> None:
    error = bounded_error(
        "first\nBearer abc.def\nsecret-value " + "x" * 2000, ["secret-value"]
    )
    assert "abc.def" not in error
    assert "secret-value" not in error
    assert "\n" not in error
    assert len(error) == 1024


def test_callback_retries_a_transport_failure(monkeypatch: pytest.MonkeyPatch) -> None:
    calls = 0

    def urlopen(*_: object, **__: object) -> Response:
        nonlocal calls
        calls += 1
        if calls == 1:
            raise urllib.error.URLError(ConnectionError("temporarily unavailable"))
        return Response()

    monkeypatch.setattr("inferscale_bench.callback.urllib.request.urlopen", urlopen)
    monkeypatch.setattr("inferscale_bench.callback.time.sleep", lambda _: None)
    post_result(
        "http://api.example/callback",
        "run-1",
        {"state": "failed", "error": "worker failed"},
        token="token",
    )
    assert calls == 2
