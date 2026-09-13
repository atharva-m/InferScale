#!/usr/bin/env bash
set -euo pipefail

container_name="${INFERSCALE_BASELINE_CONTAINER:-inferscale-vllm-baseline}"
if docker container inspect "${container_name}" >/dev/null 2>&1; then
  docker stop --time 30 "${container_name}" >/dev/null
  echo "Stopped ${container_name}; the Hugging Face cache volume was retained."
else
  echo "Baseline container ${container_name} does not exist."
fi
