import hashlib
import json
from pathlib import Path

import pytest

from modelcache import entrypoint
from modelcache.entrypoint import (
    CacheError,
    CacheSpec,
    build_manifest,
    download,
    prune_quarantine,
    quarantine_entry,
    result_payload,
    verify_entry,
    write_completion,
)


def test_cache_key_is_stable_and_revision_addressed() -> None:
    first = CacheSpec.parse("hf://Qwen/Qwen3-8B", "a" * 40)
    same = CacheSpec.parse("hf://Qwen/Qwen3-8B", "A" * 40)
    different = CacheSpec.parse("hf://Qwen/Qwen3-8B", "b" * 40)
    assert first.key == same.key
    assert first.key != different.key
    assert first.key.startswith("sha256-")
    expected = hashlib.sha256(b"hf://Qwen/Qwen3-8B\0" + b"a" * 40).hexdigest()
    assert first.key == f"sha256-{expected}"


@pytest.mark.parametrize(
    ("uri", "revision"),
    [
        ("https://example.com/model", "a" * 40),
        ("hf://../../etc/passwd", "a" * 40),
        ("hf://Qwen/Qwen3-8B", "main"),
    ],
)
def test_spec_rejects_mutable_or_unsafe_input(uri: str, revision: str) -> None:
    with pytest.raises(CacheError):
        CacheSpec.parse(uri, revision)


def test_manifest_detects_corruption(tmp_path: Path) -> None:
    spec = CacheSpec.parse("hf://Qwen/Qwen3-8B", "a" * 40)
    target = tmp_path / spec.key
    target.mkdir()
    weights = target / "model.safetensors"
    weights.write_bytes(b"weights")
    manifest = build_manifest(target, spec)
    write_completion(target, manifest)
    assert (
        verify_entry(target, spec, full=True)["tree_sha256"] == manifest["tree_sha256"]
    )
    weights.write_bytes(b"corrupt")
    with pytest.raises(CacheError, match="checksum"):
        verify_entry(target, spec, full=True)


def test_completion_marker_is_json(tmp_path: Path) -> None:
    spec = CacheSpec.parse("hf://Qwen/Qwen3-8B", "a" * 40)
    target = tmp_path / spec.key
    target.mkdir()
    (target / "config.json").write_text("{}", encoding="utf-8")
    write_completion(target, build_manifest(target, spec))
    marker = json.loads((target / ".complete").read_text(encoding="utf-8"))
    assert marker["model_revision"] == "a" * 40


def test_result_payload_is_bounded_summary(tmp_path: Path) -> None:
    spec = CacheSpec.parse("hf://Qwen/Qwen3-8B", "a" * 40)
    target = tmp_path / spec.key
    target.mkdir()
    (target / "config.json").write_text("{}", encoding="utf-8")
    manifest = build_manifest(target, spec)
    payload = result_payload(target, spec, False, 1.23456789, manifest)
    assert payload["cache_hit"] is False
    assert payload["download_duration_s"] == 1.234568
    assert payload["file_count"] == 1
    assert payload["tree_sha256"] == manifest["tree_sha256"]
    assert "files" not in payload
    assert len(json.dumps(payload)) < 4096


def test_download_quarantines_incomplete_target(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    spec = CacheSpec.parse("hf://Qwen/Qwen3-8B", "a" * 40)
    target = tmp_path / spec.key
    target.mkdir()
    (target / "partial.safetensors").write_bytes(b"incomplete")

    def fake_snapshot_download(**kwargs: object) -> None:
        local_dir = Path(str(kwargs["local_dir"]))
        (local_dir / "model.safetensors").write_bytes(b"complete")

    monkeypatch.setattr("huggingface_hub.snapshot_download", fake_snapshot_download)
    result = download(spec, tmp_path, None, 1)
    downloaded, cache_hit, quarantined = (
        result.path,
        result.cache_hit,
        result.quarantined_path,
    )

    assert cache_hit is False
    assert downloaded == target
    assert (downloaded / ".complete").is_file()
    assert quarantined is not None
    assert quarantined.parent == tmp_path / ".quarantine"
    assert (quarantined / "partial.safetensors").read_bytes() == b"incomplete"
    assert (quarantined.parent / f"{quarantined.name}.json").is_file()


def test_full_verification_quarantines_corrupt_complete_target(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    spec = CacheSpec.parse("hf://Qwen/Qwen3-8B", "a" * 40)
    target = tmp_path / spec.key
    target.mkdir()
    weights = target / "model.safetensors"
    weights.write_bytes(b"original")
    write_completion(target, build_manifest(target, spec))
    weights.write_bytes(b"corrupt")

    def fake_snapshot_download(**kwargs: object) -> None:
        local_dir = Path(str(kwargs["local_dir"]))
        (local_dir / "model.safetensors").write_bytes(b"replacement")

    monkeypatch.setattr("huggingface_hub.snapshot_download", fake_snapshot_download)
    result = download(spec, tmp_path, None, 1, full_verification=True)
    cache_hit, quarantined = result.cache_hit, result.quarantined_path

    assert cache_hit is False
    assert quarantined is not None
    assert (quarantined / "model.safetensors").read_bytes() == b"corrupt"
    assert (target / "model.safetensors").read_bytes() == b"replacement"


@pytest.mark.parametrize("cache_exists", [False, True])
def test_cli_hashes_weights_once_per_prefetch(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, cache_exists: bool
) -> None:
    spec = CacheSpec.parse("hf://Qwen/Qwen3-8B", "a" * 40)
    target = tmp_path / spec.key
    if cache_exists:
        target.mkdir()
        (target / "model.safetensors").write_bytes(b"weights")
        write_completion(target, build_manifest(target, spec))

    def fake_snapshot_download(**kwargs: object) -> None:
        assert not cache_exists, "a verified cache hit must not download again"
        (Path(str(kwargs["local_dir"])) / "model.safetensors").write_bytes(b"weights")

    monkeypatch.setattr("huggingface_hub.snapshot_download", fake_snapshot_download)
    monkeypatch.setenv("TERMINATION_LOG_PATH", str(tmp_path / "termination.json"))
    hashed: list[Path] = []
    real_hash = entrypoint._sha256

    def count_hash(path: Path) -> str:
        hashed.append(path)
        return real_hash(path)

    monkeypatch.setattr(entrypoint, "_sha256", count_hash)
    assert (
        entrypoint.main(
            [
                "--uri",
                spec.uri,
                "--revision",
                spec.revision,
                "--cache-root",
                str(tmp_path),
                "--full-verification",
            ]
        )
        == 0
    )
    assert len(hashed) == 1
    result = json.loads((tmp_path / "termination.json").read_text())
    assert result["cache_hit"] is cache_exists
    assert result["tree_sha256"] == verify_entry(target, spec)["tree_sha256"]


def test_quarantine_pruning_is_explicit_and_bounded(tmp_path: Path) -> None:
    spec = CacheSpec.parse("hf://Qwen/Qwen3-8B", "a" * 40)
    quarantined: list[Path] = []
    for index in range(3):
        target = tmp_path / spec.key
        target.write_text(str(index), encoding="utf-8")
        entry = quarantine_entry(tmp_path, target, spec, "test evidence")
        quarantined.append(entry)

    assert prune_quarantine(tmp_path, 0) == []
    assert all(path.exists() for path in quarantined)
    removed = prune_quarantine(tmp_path, 2)
    assert len(removed) == 1
    removed_entry = removed[0]
    assert not removed_entry.exists()
    assert not (removed_entry.parent / f"{removed_entry.name}.json").exists()
    assert sum(path.exists() for path in quarantined) == 2
