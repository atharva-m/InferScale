#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${script_dir}/../lib/common.sh"
cluster_name="${INFERSCALE_K3D_CLUSTER:-inferscale-dev}"
require_command k3d

if cluster_exists "${cluster_name}"; then
  k3d cluster stop "${cluster_name}"
else
  echo "k3d cluster ${cluster_name} does not exist"
fi
