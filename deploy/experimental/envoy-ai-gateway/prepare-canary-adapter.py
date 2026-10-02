#!/usr/bin/env python3
"""Verify and patch the pinned Gateway sources without downloading or building."""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path
import shutil
import stat
import subprocess
import sys


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def tree_sha256(root: Path) -> str:
    """Content hash of source names, regular file bytes and symbolic link targets."""
    digest = hashlib.sha256()
    for path in sorted(root.rglob("*")):
        relative = path.relative_to(root)
        if ".git" in relative.parts:
            continue
        if path.is_symlink():
            value = b"L\0" + relative.as_posix().encode() + b"\0" + path.readlink().as_posix().encode()
        elif path.is_file():
            value = b"F\0" + relative.as_posix().encode() + b"\0" + bytes.fromhex(sha256(path))
        else:
            continue
        digest.update(value + b"\0")
    return digest.hexdigest()


def prepare(sources: dict[str, Path], output: Path, manifest_path: Path) -> None:
    manifest = json.loads(manifest_path.read_text())
    if output.exists():
        raise ValueError(f"output already exists: {output}; use a new directory")
    for filename, expected in manifest["build_files"].items():
        if sha256(manifest_path.parent / filename) != expected:
            raise ValueError(f"{filename}: build input checksum mismatch")
    # Validate every input before creating the output tree.
    for name, spec in manifest["sources"].items():
        source = sources[name].resolve(strict=True)
        if not source.is_dir():
            raise ValueError(f"{name}: source must be an extracted directory")
        if output.resolve().is_relative_to(source):
            raise ValueError(f"{name}: output must be outside the input source directory")
        if tree_sha256(source) != spec["upstream_tree_sha256"]:
            raise ValueError(f"{name}: source tree does not match the recorded pinned source")
        patch = manifest_path.parent / spec["patch_file"]
        if sha256(patch) != spec["patch_sha256"]:
            raise ValueError(f"{name}: patch checksum mismatch")
    output.mkdir(parents=True)
    for name, spec in manifest["sources"].items():
        destination = output / name
        shutil.copytree(sources[name], destination, symlinks=True, ignore=shutil.ignore_patterns(".git"))
        # Go's module cache is read-only. Only the copied tree becomes writable.
        for path in [destination, *destination.rglob("*")]:
            if not path.is_symlink():
                path.chmod(stat.S_IMODE(path.stat().st_mode) | stat.S_IWUSR)
        subprocess.run(
            ["patch", "--batch", "--forward", "--fuzz=0", "-p1", "-i", str((manifest_path.parent / spec["patch_file"]).resolve())],
            cwd=destination,
            check=True,
        )
        if tree_sha256(destination) != spec["patched_tree_sha256"]:
            raise ValueError(f"{name}: patched tree checksum mismatch")
        for module, expected in spec["module_files_unchanged"].items():
            if sha256(destination / module) != expected:
                raise ValueError(f"{name}: {module} changed")
        print(f"Verified patched {name}: {destination}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ai-source", required=True, type=Path, help="pristine v0.7.0 source archive extraction")
    parser.add_argument("--eg-source", required=True, type=Path, help="verified github.com/envoyproxy/gateway@v1.8.1 Go module directory")
    parser.add_argument("--output", required=True, type=Path, help="new directory for both patched build contexts")
    args = parser.parse_args()
    try:
        prepare(
            {"ai-gateway": args.ai_source, "envoy-gateway": args.eg_source},
            args.output,
            Path(__file__).resolve().with_name("weighted-canary.provenance.json"),
        )
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print(f"Adapter preparation failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
