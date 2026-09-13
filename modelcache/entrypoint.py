#!/usr/bin/env python3
from __future__ import annotations

import argparse
import fcntl
import hashlib
import json
import os
import re
import shutil
import sys
import time
import uuid
from collections.abc import Iterator
from contextlib import contextmanager
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path

IMMUTABLE_REVISION = re.compile(r"^[0-9a-fA-F]{40}$")
HF_REPOSITORY = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$")


class CacheError(RuntimeError):
    pass


@dataclass(frozen=True)
class CacheSpec:
    uri: str
    repository: str
    revision: str

    @classmethod
    def parse(cls, uri: str, revision: str) -> CacheSpec:
        if not uri.startswith("hf://"):
            raise CacheError("v1 model cache accepts only hf:// URIs")
        repository = uri.removeprefix("hf://")
        if not HF_REPOSITORY.fullmatch(repository):
            raise CacheError("model URI must be hf://<owner>/<repository>")
        if not IMMUTABLE_REVISION.fullmatch(revision):
            raise CacheError("revision must be a 40-character immutable commit SHA")
        return cls(uri=uri, repository=repository, revision=revision.lower())

    @property
    def key(self) -> str:
        canonical = self.uri.encode("utf-8") + b"\0" + self.revision.encode("ascii")
        return "sha256-" + hashlib.sha256(canonical).hexdigest()


def _assert_child(root: Path, child: Path) -> None:
    if root.resolve() not in child.resolve().parents:
        raise CacheError(f"cache path escapes root: {child}")


@contextmanager
def cache_lock(lock_path: Path, timeout_s: float) -> Iterator[None]:
    lock_path.parent.mkdir(parents=True, exist_ok=True)
    with lock_path.open("a+", encoding="utf-8") as handle:
        deadline = time.monotonic() + timeout_s
        while True:
            try:
                fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    raise CacheError(
                        f"timed out waiting for cache lock {lock_path.name}"
                    )
                time.sleep(0.25)
        handle.seek(0)
        handle.truncate()
        handle.write(
            json.dumps(
                {
                    "pid": os.getpid(),
                    "acquired_utc": datetime.now(timezone.utc).isoformat(),
                }
            )
        )
        handle.flush()
        os.fsync(handle.fileno())
        try:
            yield
        finally:
            fcntl.flock(handle.fileno(), fcntl.LOCK_UN)


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def build_manifest(directory: Path, spec: CacheSpec) -> dict[str, object]:
    entries: list[dict[str, object]] = []
    tree_digest = hashlib.sha256()
    for path in sorted(directory.rglob("*")):
        if path.name.startswith(".complete"):
            continue
        if path.is_symlink():
            raise CacheError(
                f"model snapshot contains a symbolic link: {path.relative_to(directory)}"
            )
        if not path.is_file():
            continue
        relative = path.relative_to(directory).as_posix()
        file_digest = _sha256(path)
        size = path.stat().st_size
        entries.append({"path": relative, "size": size, "sha256": file_digest})
        tree_digest.update(
            relative.encode("utf-8") + b"\0" + file_digest.encode("ascii") + b"\0"
        )
    if not entries:
        raise CacheError("downloaded snapshot contains no files")
    return {
        "schema_version": 1,
        "cache_key": spec.key,
        "model_uri": spec.uri,
        "model_revision": spec.revision,
        "tree_sha256": tree_digest.hexdigest(),
        "files": entries,
        "completed_utc": datetime.now(timezone.utc).isoformat(),
    }


def write_completion(directory: Path, manifest: dict[str, object]) -> None:
    temporary = directory / ".complete.tmp"
    final = directory / ".complete"
    with temporary.open("w", encoding="utf-8") as handle:
        json.dump(manifest, handle, indent=2, sort_keys=True)
        handle.write("\n")
        handle.flush()
        os.fsync(handle.fileno())
    temporary.replace(final)
    directory_fd = os.open(directory, os.O_RDONLY)
    try:
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)


def verify_entry(
    directory: Path, spec: CacheSpec, full: bool = False
) -> dict[str, object]:
    marker = directory / ".complete"
    if not marker.is_file():
        raise CacheError(f"cache entry {spec.key} is incomplete")
    try:
        manifest = json.loads(marker.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise CacheError(f"invalid completion marker: {exc}") from exc
    if not isinstance(manifest, dict):
        raise CacheError("invalid completion marker: root must be a JSON object")
    expected = {
        "cache_key": spec.key,
        "model_uri": spec.uri,
        "model_revision": spec.revision,
    }
    for field, value in expected.items():
        if manifest.get(field) != value:
            raise CacheError(f"completion marker {field} mismatch")
    if full:
        actual = build_manifest(directory, spec)
        if actual["tree_sha256"] != manifest.get("tree_sha256"):
            raise CacheError("cache checksum verification failed")
    return manifest


def result_payload(
    target: Path,
    spec: CacheSpec,
    cache_hit: bool,
    download_duration_s: float,
    manifest: dict[str, object],
    quarantined_path: Path | None = None,
) -> dict[str, object]:
    """Return a bounded result suitable for Kubernetes' termination message."""
    files = manifest.get("files")
    payload: dict[str, object] = {
        "schema_version": 1,
        "path": str(target),
        "cache_key": spec.key,
        "model_revision": spec.revision,
        "cache_hit": cache_hit,
        "download_duration_s": round(download_duration_s, 6),
        "tree_sha256": manifest.get("tree_sha256"),
        "file_count": len(files) if isinstance(files, list) else 0,
    }
    if quarantined_path is not None:
        payload["quarantined_path"] = str(quarantined_path)
    return payload


def _fsync_directory(directory: Path) -> None:
    directory_fd = os.open(directory, os.O_RDONLY)
    try:
        os.fsync(directory_fd)
    finally:
        os.close(directory_fd)


def quarantine_entry(
    cache_root: Path, target: Path, spec: CacheSpec, reason: str
) -> Path:
    """Atomically retain an invalid target and a bounded diagnostic sidecar."""
    quarantine_root = cache_root / ".quarantine"
    _assert_child(cache_root, quarantine_root)
    quarantine_root.mkdir(mode=0o750, parents=True, exist_ok=True)
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S.%fZ")
    destination = quarantine_root / f"{spec.key}-{timestamp}-{uuid.uuid4().hex}"
    _assert_child(quarantine_root, destination)
    target.replace(destination)

    metadata = {
        "schema_version": 1,
        "cache_key": spec.key,
        "model_revision": spec.revision,
        "original_path": str(target),
        "quarantined_path": str(destination),
        "quarantined_utc": datetime.now(timezone.utc).isoformat(),
        "reason": reason[:1024],
    }
    metadata_path = quarantine_root / f"{destination.name}.json"
    temporary = quarantine_root / f".{destination.name}.{uuid.uuid4().hex}.tmp"
    with temporary.open("w", encoding="utf-8") as handle:
        json.dump(metadata, handle, indent=2, sort_keys=True)
        handle.write("\n")
        handle.flush()
        os.fsync(handle.fileno())
    temporary.replace(metadata_path)
    _fsync_directory(quarantine_root)
    _fsync_directory(cache_root)
    return destination


def prune_quarantine(cache_root: Path, max_entries: int) -> list[Path]:
    """Remove oldest evidence only when an operator explicitly sets a limit."""
    if max_entries < 0:
        raise CacheError("quarantine max entries cannot be negative")
    quarantine_root = cache_root / ".quarantine"
    if max_entries == 0 or not quarantine_root.is_dir():
        return []
    candidates = [
        path
        for path in quarantine_root.iterdir()
        if path.name.startswith("sha256-") and not path.name.endswith(".json")
    ]
    candidates.sort(key=lambda path: path.name.split("-", 2)[-1], reverse=True)
    removed: list[Path] = []
    for stale in candidates[max_entries:]:
        if stale.is_symlink() or stale.is_file():
            stale.unlink()
        elif stale.is_dir():
            shutil.rmtree(stale)
        metadata = quarantine_root / f"{stale.name}.json"
        metadata.unlink(missing_ok=True)
        removed.append(stale)
    if removed:
        _fsync_directory(quarantine_root)
    return removed


@dataclass(frozen=True)
class DownloadResult:
    path: Path
    cache_hit: bool
    download_duration_s: float
    quarantined_path: Path | None
    manifest: dict[str, object]


def download(
    spec: CacheSpec,
    cache_root: Path,
    token: str | None,
    lock_timeout_s: float,
    *,
    full_verification: bool = False,
    quarantine_max_entries: int = 0,
) -> DownloadResult:
    cache_root = cache_root.resolve()
    cache_root.mkdir(parents=True, exist_ok=True)
    target = cache_root / spec.key
    lock_path = cache_root / ".locks" / f"{spec.key}.lock"
    _assert_child(cache_root, target)
    _assert_child(cache_root, lock_path)
    with cache_lock(lock_path, lock_timeout_s):
        quarantined_path: Path | None = None
        if target.exists() or target.is_symlink():
            try:
                manifest = verify_entry(target, spec, full=full_verification)
            except (CacheError, OSError) as exc:
                quarantined_path = quarantine_entry(
                    cache_root, target, spec, f"{type(exc).__name__}: {exc}"
                )
                prune_quarantine(cache_root, quarantine_max_entries)
            else:
                return DownloadResult(target, True, 0.0, None, manifest)
        partial = cache_root / f".{spec.key}.partial-{uuid.uuid4().hex}"
        _assert_child(cache_root, partial)
        partial.mkdir(mode=0o750)
        try:
            from huggingface_hub import snapshot_download

            download_started = time.monotonic()
            snapshot_download(
                repo_id=spec.repository,
                repo_type="model",
                revision=spec.revision,
                token=token,
                local_dir=partial,
            )
            metadata_cache = partial / ".cache"
            if metadata_cache.exists():
                shutil.rmtree(metadata_cache)
            manifest = build_manifest(partial, spec)
            write_completion(partial, manifest)
            partial.replace(target)
            _fsync_directory(cache_root)
            return DownloadResult(
                target,
                False,
                time.monotonic() - download_started,
                quarantined_path,
                manifest,
            )
        except Exception:
            shutil.rmtree(partial, ignore_errors=True)
            raise


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(
        description="Populate or verify an immutable InferScale model cache entry"
    )
    result.add_argument("--uri", required=True)
    result.add_argument("--revision", required=True)
    result.add_argument(
        "--cache-root", default=os.getenv("INFERSCALE_MODEL_CACHE", "/cache")
    )
    result.add_argument("--lock-timeout-s", type=float, default=1800)
    result.add_argument(
        "--quarantine-max-entries",
        type=int,
        default=os.getenv("INFERSCALE_QUARANTINE_MAX_ENTRIES", "0"),
        help="retain at most this many quarantined entries; zero disables automatic deletion",
    )
    result.add_argument("--verify-only", action="store_true")
    result.add_argument("--full-verification", action="store_true")
    return result


def main(argv: list[str] | None = None) -> int:
    args = parser().parse_args(argv)
    try:
        spec = CacheSpec.parse(args.uri, args.revision)
        if args.lock_timeout_s <= 0:
            raise CacheError("lock timeout must be positive")
        if args.quarantine_max_entries < 0:
            raise CacheError("quarantine max entries cannot be negative")
        root = Path(args.cache_root)
        target = root / spec.key
        quarantined_path: Path | None = None
        if args.verify_only:
            manifest = verify_entry(target, spec, full=args.full_verification)
            cache_hit = True
            download_duration_s = 0.0
        else:
            downloaded = download(
                spec,
                root,
                os.getenv("HF_TOKEN"),
                args.lock_timeout_s,
                full_verification=args.full_verification,
                quarantine_max_entries=args.quarantine_max_entries,
            )
            target = downloaded.path
            cache_hit = downloaded.cache_hit
            download_duration_s = downloaded.download_duration_s
            quarantined_path = downloaded.quarantined_path
            manifest = downloaded.manifest
        payload = result_payload(
            target, spec, cache_hit, download_duration_s, manifest, quarantined_path
        )
        message = json.dumps(payload, sort_keys=True)
        print(message)
        termination_path = Path(
            os.getenv("TERMINATION_LOG_PATH", "/dev/termination-log")
        )
        try:
            termination_path.write_text(message + "\n", encoding="utf-8")
        except OSError:
            pass
        return 0
    except Exception as exc:  # noqa: BLE001 - CLI boundary returns one machine-readable error
        print(
            json.dumps({"error": type(exc).__name__, "message": str(exc)}),
            file=sys.stderr,
        )
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
