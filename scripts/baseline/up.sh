#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${script_dir}/../lib/common.sh"
require_command docker

container_name="${INFERSCALE_BASELINE_CONTAINER:-inferscale-vllm-baseline}"
image="${INFERSCALE_BASELINE_IMAGE:-vllm/vllm-openai:v0.23.0@sha256:6d8429e38e3747723ca07ee1b17972e09bb9c51c4032b266f24fb1cc3b22ed8f}"
model="${INFERSCALE_LOCAL_MODEL:-Qwen/Qwen3-0.6B}"
revision="${INFERSCALE_LOCAL_MODEL_REVISION:-}"
if [[ -z "${revision}" && "${model}" == "Qwen/Qwen3-0.6B" ]]; then
  revision=c1899de289a04d12100db370d81485cdf75e47ca
fi
[[ "${revision}" =~ ^[0-9a-fA-F]{40}$ ]] || die "INFERSCALE_LOCAL_MODEL_REVISION must be an immutable 40-character commit SHA"

if docker container inspect "${container_name}" >/dev/null 2>&1; then
  docker start "${container_name}" >/dev/null
else
  token_args=()
  if [[ -n "${HF_TOKEN:-}" ]]; then
    token_args=(--env HF_TOKEN)
  fi
  docker run --detach \
    --name "${container_name}" \
    --gpus all \
    --publish 8000:8000 \
    --volume inferscale-huggingface-cache:/root/.cache/huggingface \
    "${token_args[@]}" \
    "${image}" \
    --model "${model}" \
    --revision "${revision}" \
    --served-model-name qwen-local \
    --max-model-len 2048 \
    --gpu-memory-utilization 0.85 \
    --enable-prefix-caching
fi

deadline=$((SECONDS + 600))
until curl --fail --silent http://127.0.0.1:8000/health >/dev/null; do
  if (( SECONDS >= deadline )); then
    docker logs --tail 100 "${container_name}" >&2
    die "vLLM baseline did not become ready within 10 minutes"
  fi
  sleep 2
done
echo "Direct vLLM baseline is ready at http://127.0.0.1:8000"
