#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"
command -v docker >/dev/null 2>&1 || { echo "docker is required" >&2; exit 1; }

python_image="${PYTHON_BASE_IMAGE:-}"
go_image="${GO_BASE_IMAGE:-}"
runtime_image="${DISTROLESS_BASE_IMAGE:-}"
source_commit="${INFERSCALE_GIT_COMMIT:-}"
if [[ -z "${source_commit}" ]]; then
  source_commit="$(git rev-parse HEAD 2>/dev/null || true)"
fi
[[ "${source_commit}" =~ ^[0-9a-f]{40}$ ]] || {
  echo "INFERSCALE_GIT_COMMIT must be a full source commit when HEAD is unavailable" >&2
  exit 64
}
vllm_base_image="${VLLM_BASE_IMAGE:-vllm/vllm-openai:v0.23.0@sha256:6d8429e38e3747723ca07ee1b17972e09bb9c51c4032b266f24fb1cc3b22ed8f}"
trtllm_base_image="${TRTLLM_BASE_IMAGE:-nvcr.io/nvidia/tensorrt-llm/release:1.0.0@sha256:e0d966e2daec1827046dd30c6143b39c947ef06ed3fb9ef9469c83a8643b29d7}"
for value in "${python_image}" "${go_image}" "${runtime_image}"; do
  [[ "${value}" =~ @sha256:[0-9a-f]{64}$ ]] || {
    echo "PYTHON_BASE_IMAGE, GO_BASE_IMAGE, and DISTROLESS_BASE_IMAGE must all be immutable digest references" >&2
    exit 64
  }
done
for value in "${vllm_base_image}" "${trtllm_base_image}"; do
  [[ "${value}" =~ @sha256:[0-9a-f]{64}$ ]] || {
    echo "VLLM_BASE_IMAGE and TRTLLM_BASE_IMAGE must be immutable digest references" >&2
    exit 64
  }
done

docker build --build-arg "PYTHON_IMAGE=${python_image}" -t "${MODEL_CACHE_IMAGE:?MODEL_CACHE_IMAGE is required}" modelcache
docker build \
  --build-arg "PYTHON_IMAGE=${python_image}" \
  --build-arg "INFERSCALE_GIT_COMMIT=${source_commit}" \
  -t "${BENCHMARK_IMAGE:?BENCHMARK_IMAGE is required}" \
  -f benchmarks/runner/Dockerfile .
docker build --build-arg "VLLM_IMAGE=${vllm_base_image}" -t "${VLLM_IMAGE:?VLLM_IMAGE is required}" runtime/vllm
docker build --build-arg "TRTLLM_IMAGE=${trtllm_base_image}" -t "${TRTLLM_IMAGE:?TRTLLM_IMAGE is required}" runtime/trtllm
for service in api controller admission migrate; do
  destination_variable="${service^^}_IMAGE"
  destination_variable="${destination_variable//-/_}"
  destination="${!destination_variable:-}"
  [[ -n "${destination}" ]] || { echo "${destination_variable} is required" >&2; exit 64; }
  docker build \
    --build-arg "GO_IMAGE=${go_image}" \
    --build-arg "RUNTIME_IMAGE=${runtime_image}" \
    --build-arg "COMMAND=${service}" \
    -t "${destination}" -f build/Dockerfile .
done
