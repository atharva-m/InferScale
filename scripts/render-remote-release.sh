#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
overlay_name="${1:-}"
case "${overlay_name}" in
  remote|vast-1x5090|vast-2x5090|vast-4x5090) ;;
  *)
    echo "usage: $0 <remote|vast-1x5090|vast-2x5090|vast-4x5090>" >&2
    exit 64
    ;;
esac

command -v kubectl >/dev/null 2>&1 || { echo "kubectl is required" >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 1; }

for variable in API_IMAGE CONTROLLER_IMAGE ADMISSION_IMAGE MIGRATE_IMAGE; do
  value="${!variable:-}"
  if [[ ! "${value}" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]]; then
    echo "${variable} must be a registry image pinned with @sha256:<64 lowercase hex>" >&2
    exit 64
  fi
done
if ! python3 - \
  "${PUBLIC_BASE_URL:-}" \
  "${INFERSCALE_BENCHMARK_PROVIDER:-}" \
  "${INFERSCALE_BENCHMARK_INFERENCE_BASE_URL:-}" \
  "${INFERSCALE_BENCHMARK_PROMETHEUS_URL:-}" \
  "${INFERSCALE_BENCHMARK_DRIVER_VERSION:-}" \
  "${INFERSCALE_BENCHMARK_CUDA_VERSION:-}" <<'PY'
import re
import sys
import urllib.parse


def valid_origin(raw: str, schemes: set[str]) -> bool:
    try:
        value = urllib.parse.urlsplit(raw)
        valid_port = value.port is None or 1 <= value.port <= 65535
    except ValueError:
        return False
    return (
        raw == raw.strip()
        and not any(character.isspace() or ord(character) < 32 for character in raw)
        and "?" not in raw
        and "#" not in raw
        and "\\" not in value.netloc
        and "%" not in value.netloc
        and value.scheme in schemes
        and value.hostname is not None
        and value.username is None
        and value.password is None
        and not value.netloc.endswith(":")
        and value.path in {"", "/"}
        and not value.query
        and not value.fragment
        and valid_port
    )


public_origin, provider, inference_origin, prometheus_origin, driver_version, cuda_version = sys.argv[1:]
if not valid_origin(public_origin, {"https"}):
    print("PUBLIC_BASE_URL must be an HTTPS origin without a path, credentials, query, or fragment", file=sys.stderr)
    raise SystemExit(1)
if not re.fullmatch(r"[a-z0-9][a-z0-9._-]{0,63}", provider) or provider == "set-at-run-time":
    print("INFERSCALE_BENCHMARK_PROVIDER must be a lowercase provider identifier", file=sys.stderr)
    raise SystemExit(1)
if not valid_origin(inference_origin, {"https"}):
    print("INFERSCALE_BENCHMARK_INFERENCE_BASE_URL must be an HTTPS origin without credentials, query, or fragment", file=sys.stderr)
    raise SystemExit(1)
if not valid_origin(prometheus_origin, {"http", "https"}):
    print("INFERSCALE_BENCHMARK_PROMETHEUS_URL must be an HTTP(S) origin without credentials, query, or fragment", file=sys.stderr)
    raise SystemExit(1)
if not re.fullmatch(r"[0-9]{3,4}\.[0-9]{1,3}(?:\.[0-9]{1,3})?", driver_version):
    print("INFERSCALE_BENCHMARK_DRIVER_VERSION must be an explicit NVIDIA driver version", file=sys.stderr)
    raise SystemExit(1)
if not re.fullmatch(r"[0-9]{1,2}\.[0-9]{1,2}(?:\.[0-9]+)?", cuda_version):
    print("INFERSCALE_BENCHMARK_CUDA_VERSION must be an explicit CUDA version", file=sys.stderr)
    raise SystemExit(1)
PY
then
  exit 64
fi

render_dir="$(mktemp -d)"
trap 'rm -rf -- "${render_dir}"' EXIT
kubectl kustomize "${repo_root}/deploy/overlays/${overlay_name}" >"${render_dir}/template.yaml"

API_IMAGE="${API_IMAGE}" \
CONTROLLER_IMAGE="${CONTROLLER_IMAGE}" \
ADMISSION_IMAGE="${ADMISSION_IMAGE}" \
MIGRATE_IMAGE="${MIGRATE_IMAGE}" \
PUBLIC_BASE_URL="${PUBLIC_BASE_URL}" \
INFERSCALE_BENCHMARK_PROVIDER="${INFERSCALE_BENCHMARK_PROVIDER}" \
INFERSCALE_BENCHMARK_INFERENCE_BASE_URL="${INFERSCALE_BENCHMARK_INFERENCE_BASE_URL}" \
INFERSCALE_BENCHMARK_PROMETHEUS_URL="${INFERSCALE_BENCHMARK_PROMETHEUS_URL}" \
INFERSCALE_BENCHMARK_DRIVER_VERSION="${INFERSCALE_BENCHMARK_DRIVER_VERSION}" \
INFERSCALE_BENCHMARK_CUDA_VERSION="${INFERSCALE_BENCHMARK_CUDA_VERSION}" \
python3 - "${render_dir}/template.yaml" <<'PY'
import os
import pathlib
import re
import sys

import yaml


source = pathlib.Path(sys.argv[1])
documents = [document for document in yaml.safe_load_all(source.read_text()) if document]
replacements = {
    "api": (["ghcr.io/inferscale/api", "registry.invalid/inferscale/api"], os.environ["API_IMAGE"]),
    "controller": (
        ["ghcr.io/inferscale/controller", "registry.invalid/inferscale/controller"],
        os.environ["CONTROLLER_IMAGE"],
    ),
    "admission": (
        ["ghcr.io/inferscale/admission", "registry.invalid/inferscale/admission"],
        os.environ["ADMISSION_IMAGE"],
    ),
    "migrate": (
        ["ghcr.io/inferscale/migrate", "registry.invalid/inferscale/migrate"],
        os.environ["MIGRATE_IMAGE"],
    ),
}
seen = {name: 0 for name in replacements}
configured_public_origin = 0
benchmark_authority = {
    name: os.environ[name]
    for name in (
        "INFERSCALE_BENCHMARK_PROVIDER",
        "INFERSCALE_BENCHMARK_INFERENCE_BASE_URL",
        "INFERSCALE_BENCHMARK_PROMETHEUS_URL",
        "INFERSCALE_BENCHMARK_DRIVER_VERSION",
        "INFERSCALE_BENCHMARK_CUDA_VERSION",
    )
}

for document in documents:
    if document.get("kind") == "ConfigMap" and document.get("metadata", {}).get("name") == "inferscale-config":
        document.setdefault("data", {})["INFERSCALE_PUBLIC_BASE_URL"] = os.environ["PUBLIC_BASE_URL"]
        document["data"].update(benchmark_authority)
        configured_public_origin += 1
    if document.get("kind") not in {"Deployment", "StatefulSet", "Job"}:
        continue
    pod = document.get("spec", {}).get("template", {}).get("spec", {})
    for container in pod.get("initContainers", []) + pod.get("containers", []):
        image = str(container.get("image", ""))
        for logical_name, (bases, replacement) in replacements.items():
            if any(image == base or image.startswith(base + ":") or image.startswith(base + "@") for base in bases):
                container["image"] = replacement
                seen[logical_name] += 1
                break

missing = sorted(name for name, count in seen.items() if count == 0)
if missing:
    raise SystemExit(f"release render did not find expected workload images: {', '.join(missing)}")
if configured_public_origin != 1:
    raise SystemExit(f"release render found {configured_public_origin} inferscale-config ConfigMaps, want exactly one")

for document in documents:
    if document.get("kind") not in {"Deployment", "StatefulSet", "Job"}:
        continue
    pod = document.get("spec", {}).get("template", {}).get("spec", {})
    for container in pod.get("initContainers", []) + pod.get("containers", []):
        image = str(container.get("image", ""))
        if image.startswith("ghcr.io/inferscale/") and re.search(r":0\.1\.0-dev(?:$|@)", image):
            raise SystemExit(f"remote release still contains development image {image}")

yaml.safe_dump_all(documents, sys.stdout, sort_keys=False, explicit_start=True)
PY
