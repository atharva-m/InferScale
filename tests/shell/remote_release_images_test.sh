#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
render_dir="$(mktemp -d)"
trap 'rm -rf -- "${render_dir}"' EXIT

digest="$(printf 'a%.0s' {1..64})"
export API_IMAGE="registry.example/inferscale/api@sha256:${digest}"
export CONTROLLER_IMAGE="registry.example/inferscale/controller@sha256:${digest}"
export ADMISSION_IMAGE="registry.example/inferscale/admission@sha256:${digest}"
export MIGRATE_IMAGE="registry.example/inferscale/migrate@sha256:${digest}"
export PUBLIC_BASE_URL="https://inference.example.com"
export INFERSCALE_BENCHMARK_PROVIDER="vast"
export INFERSCALE_BENCHMARK_INFERENCE_BASE_URL="https://benchmark.inference.example.com"
export INFERSCALE_BENCHMARK_PROMETHEUS_URL="http://prometheus.inferscale-monitoring.svc.cluster.local:9090"
export INFERSCALE_BENCHMARK_DRIVER_VERSION="580.82.07"
export INFERSCALE_BENCHMARK_CUDA_VERSION="13.0"

"${repo_root}/scripts/render-remote-release.sh" remote >"${render_dir}/release.yaml"
for image in "${API_IMAGE}" "${CONTROLLER_IMAGE}" "${ADMISSION_IMAGE}" "${MIGRATE_IMAGE}"; do
  grep -Fq "image: ${image}" "${render_dir}/release.yaml"
done
grep -Fq "INFERSCALE_PUBLIC_BASE_URL: ${PUBLIC_BASE_URL}" "${render_dir}/release.yaml"
python3 - "${render_dir}/release.yaml" <<'PY'
import pathlib
import sys

import yaml

documents = [value for value in yaml.safe_load_all(pathlib.Path(sys.argv[1]).read_text()) if value]
config = next(
    value
    for value in documents
    if value.get("kind") == "ConfigMap" and value.get("metadata", {}).get("name") == "inferscale-config"
)
expected_authority = {
    "INFERSCALE_BENCHMARK_PROVIDER": "vast",
    "INFERSCALE_BENCHMARK_INFERENCE_BASE_URL": "https://benchmark.inference.example.com",
    "INFERSCALE_BENCHMARK_PROMETHEUS_URL": "http://prometheus.inferscale-monitoring.svc.cluster.local:9090",
    "INFERSCALE_BENCHMARK_DRIVER_VERSION": "580.82.07",
    "INFERSCALE_BENCHMARK_CUDA_VERSION": "13.0",
}
assert all(config["data"].get(name) == value for name, value in expected_authority.items())
api = next(
    value
    for value in documents
    if value.get("kind") == "Deployment" and value.get("metadata", {}).get("name") == "inferscale-api"
)
assert {source["configMapRef"]["name"] for source in api["spec"]["template"]["spec"]["containers"][0]["envFrom"]} == {
    "inferscale-config"
}
PY
if grep -Eq 'ghcr.io/inferscale/(api|controller|admission|migrate):0\.1\.0-dev' "${render_dir}/release.yaml"; then
  echo "release render retained a development control-plane image" >&2
  exit 1
fi

export API_IMAGE="registry.example/inferscale/api:mutable"
if "${repo_root}/scripts/render-remote-release.sh" remote >"${render_dir}/invalid.yaml" 2>"${render_dir}/invalid.err"; then
  echo "mutable release image was accepted" >&2
  exit 1
fi
grep -Fq 'API_IMAGE must be a registry image pinned with @sha256' "${render_dir}/invalid.err"

export API_IMAGE="registry.example/inferscale/api@sha256:${digest}"
for invalid_origin in \
  "https://user:secret@inference.example.com/private" \
  "https://inference.example.com?" \
  "https://inference.example.com#" \
  "https://:443" \
  "https://inference.example.com:0" \
  "https://inference.example.com:65536" \
  " https://inference.example.com" \
  'https://inference.example.com\evil'; do
  export PUBLIC_BASE_URL="${invalid_origin}"
  if "${repo_root}/scripts/render-remote-release.sh" remote >"${render_dir}/invalid-origin.yaml" 2>"${render_dir}/invalid-origin.err"; then
    echo "unsafe public release origin was accepted: ${invalid_origin}" >&2
    exit 1
  fi
  grep -Fq 'PUBLIC_BASE_URL must be an HTTPS origin' "${render_dir}/invalid-origin.err"
done

export PUBLIC_BASE_URL="https://inference.example.com"
unset INFERSCALE_BENCHMARK_PROVIDER
if "${repo_root}/scripts/render-remote-release.sh" remote >"${render_dir}/missing-authority.yaml" 2>"${render_dir}/missing-authority.err"; then
  echo "release render accepted missing benchmark provider authority" >&2
  exit 1
fi
grep -Fq 'INFERSCALE_BENCHMARK_PROVIDER must be a lowercase provider identifier' "${render_dir}/missing-authority.err"

export INFERSCALE_BENCHMARK_PROVIDER="vast"
export INFERSCALE_BENCHMARK_INFERENCE_BASE_URL="http://benchmark.inference.example.com"
if "${repo_root}/scripts/render-remote-release.sh" remote >"${render_dir}/unsafe-inference.yaml" 2>"${render_dir}/unsafe-inference.err"; then
  echo "release render accepted plaintext benchmark inference authority" >&2
  exit 1
fi
grep -Fq 'INFERSCALE_BENCHMARK_INFERENCE_BASE_URL must be an HTTPS origin' "${render_dir}/unsafe-inference.err"

export INFERSCALE_BENCHMARK_INFERENCE_BASE_URL="https://benchmark.inference.example.com"
export INFERSCALE_BENCHMARK_PROMETHEUS_URL="http://user:secret@prometheus:9090"
if "${repo_root}/scripts/render-remote-release.sh" remote >"${render_dir}/unsafe-prometheus.yaml" 2>"${render_dir}/unsafe-prometheus.err"; then
  echo "release render accepted credentialed benchmark Prometheus authority" >&2
  exit 1
fi
grep -Fq 'INFERSCALE_BENCHMARK_PROMETHEUS_URL must be an HTTP(S) origin' "${render_dir}/unsafe-prometheus.err"

export INFERSCALE_BENCHMARK_PROMETHEUS_URL="http://prometheus.inferscale-monitoring.svc.cluster.local:9090"
export INFERSCALE_BENCHMARK_DRIVER_VERSION="set-at-run-time"
if "${repo_root}/scripts/render-remote-release.sh" remote >"${render_dir}/invalid-driver.yaml" 2>"${render_dir}/invalid-driver.err"; then
  echo "release render accepted placeholder benchmark driver authority" >&2
  exit 1
fi
grep -Fq 'INFERSCALE_BENCHMARK_DRIVER_VERSION must be an explicit NVIDIA driver version' "${render_dir}/invalid-driver.err"

export INFERSCALE_BENCHMARK_DRIVER_VERSION="580.82.07"
export INFERSCALE_BENCHMARK_CUDA_VERSION="set-at-run-time"
if "${repo_root}/scripts/render-remote-release.sh" remote >"${render_dir}/invalid-cuda.yaml" 2>"${render_dir}/invalid-cuda.err"; then
  echo "release render accepted placeholder benchmark CUDA authority" >&2
  exit 1
fi
grep -Fq 'INFERSCALE_BENCHMARK_CUDA_VERSION must be an explicit CUDA version' "${render_dir}/invalid-cuda.err"
