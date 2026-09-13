import hashlib
from pathlib import Path

import pytest
from inferscale_bench import provenance


def test_driver_cuda_fingerprint_uses_explicit_target_versions(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(provenance, "_command", lambda *_args, **_kwargs: None)
    result = provenance.collect(
        tmp_path,
        {
            "driver_version": "580.10",
            "cuda_version": "13.0",
        },
        "guidellm-0.7.0",
    )
    expected = hashlib.sha256(b"driver=580.10|cuda=13.0").hexdigest()
    assert result["driver_cuda_fingerprint"] == expected
    assert result["nvidia_driver_version"] == "580.10"
    assert result["cuda_version"] == "13.0"


def test_missing_driver_cuda_evidence_does_not_create_fingerprint(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(provenance, "_command", lambda *_args, **_kwargs: None)
    monkeypatch.delenv("CUDA_VERSION", raising=False)
    result = provenance.collect(tmp_path, {}, "inferscale-native-http-v1")
    assert result["driver_cuda_fingerprint"] is None
