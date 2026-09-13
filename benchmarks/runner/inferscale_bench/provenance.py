from __future__ import annotations

import hashlib
import os
import platform
import subprocess
from datetime import datetime, timezone
from pathlib import Path


def _command(args: list[str], cwd: Path | None = None) -> str | None:
    try:
        result = subprocess.run(
            args, cwd=cwd, check=True, capture_output=True, text=True, timeout=10
        )
        return result.stdout.strip() or None
    except (OSError, subprocess.SubprocessError):
        return None


def collect(
    repo_root: Path, metadata: dict[str, str], benchmark_tool: str
) -> dict[str, object]:
    repository_commit = _command(["git", "rev-parse", "HEAD"], cwd=repo_root)
    image_commit = os.getenv("INFERSCALE_RUNNER_GIT_COMMIT")
    git_commit = repository_commit or (
        image_commit if image_commit != "unknown" else None
    )
    repository_status = (
        _command(["git", "status", "--porcelain"], cwd=repo_root)
        if repository_commit
        else None
    )
    nvidia_query = _command(
        [
            "nvidia-smi",
            "--query-gpu=name,driver_version,memory.total",
            "--format=csv,noheader",
        ]
    )
    driver_version = metadata.get("driver_version")
    if not driver_version and nvidia_query:
        first_gpu = nvidia_query.splitlines()[0].split(",")
        if len(first_gpu) >= 2:
            driver_version = first_gpu[1].strip()
    cuda_version = metadata.get("cuda_version") or os.getenv("CUDA_VERSION")
    compatibility = (
        f"driver={driver_version}|cuda={cuda_version}"
        if driver_version and cuda_version
        else None
    )
    return {
        "timestamp_utc": datetime.now(timezone.utc).isoformat(),
        "git_commit": git_commit,
        "git_dirty": bool(repository_status) if repository_commit else None,
        "hostname": platform.node(),
        "kernel": platform.release(),
        "platform": platform.platform(),
        "python": platform.python_version(),
        "nvidia_gpus": nvidia_query.splitlines() if nvidia_query else [],
        "nvidia_driver_version": driver_version,
        "cuda_version": cuda_version,
        "driver_cuda_fingerprint": hashlib.sha256(compatibility.encode()).hexdigest()
        if compatibility
        else None,
        "container_image_digest": metadata.get("container_image_digest"),
        "runtime_image_digest": metadata.get("runtime_image_digest")
        or metadata.get("container_image_digest"),
        "runtime_version": metadata.get("runtime_version"),
        "runner_image_digest": metadata.get("runner_image_digest"),
        "benchmark_tool": benchmark_tool,
        "gpu_hourly_price": float(metadata["gpu_hourly_price"])
        if metadata.get("gpu_hourly_price")
        else None,
        "provider": metadata.get("provider"),
    }
