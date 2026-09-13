#!/usr/bin/env bash
set -euo pipefail

if (( EUID != 0 )); then
  echo "run as root (or with sudo)" >&2
  exit 77
fi
command -v nvidia-smi >/dev/null 2>&1 || { echo "nvidia-smi is required before Kubernetes bootstrap" >&2; exit 1; }

gpu_sku="${INFERSCALE_GPU_SKU:-}"
[[ "${gpu_sku}" =~ ^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?$ ]] || {
  echo "INFERSCALE_GPU_SKU is required and must be a Kubernetes label value (for example RTX_5090)" >&2
  exit 64
}
gpu_count="$(nvidia-smi --query-gpu=index --format=csv,noheader | awk 'NF {count++} END {print count+0}')"
(( gpu_count > 0 )) || { echo "no NVIDIA GPUs were discovered" >&2; exit 1; }

cache_root="${INFERSCALE_MODEL_CACHE_ROOT:-/var/lib/inferscale/models}"
engine_root="${INFERSCALE_ENGINE_CACHE_ROOT:-/var/lib/inferscale/engines}"
install -d -o 65532 -g 65532 -m 2770 "${cache_root}" "${engine_root}"

if ! command -v k3s >/dev/null 2>&1; then
  k3s_version="${K3S_VERSION:-}"
  k3s_installer_sha256="${K3S_INSTALLER_SHA256:-}"
  [[ -n "${k3s_version}" ]] || { echo "K3S_VERSION is required when k3s is not installed" >&2; exit 64; }
  [[ "${k3s_installer_sha256}" =~ ^[0-9a-f]{64}$ ]] || { echo "K3S_INSTALLER_SHA256 must be a lowercase 64-hex digest" >&2; exit 64; }
  command -v curl >/dev/null 2>&1 || { echo "curl is required to install k3s" >&2; exit 1; }
  command -v sha256sum >/dev/null 2>&1 || { echo "sha256sum is required to verify the k3s installer" >&2; exit 1; }
  installer="$(mktemp)"
  trap 'rm -f -- "$installer"' EXIT
  curl --fail --show-error --location --proto '=https' --tlsv1.2 https://get.k3s.io --output "${installer}"
  printf '%s  %s\n' "${k3s_installer_sha256}" "${installer}" | sha256sum --check --status || {
    echo "k3s installer checksum mismatch" >&2
    exit 65
  }
  INSTALL_K3S_VERSION="${k3s_version}" sh "${installer}" \
    --write-kubeconfig-mode 0640 \
    --disable traefik \
    --node-label "inferscale.io/gpu-node=true"
fi

systemctl enable --now k3s
for _ in $(seq 1 60); do
  if k3s kubectl get nodes >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
k3s kubectl label nodes --all --overwrite \
  "inferscale.io/gpu-node=true" \
  "inferscale.io/gpu-sku=${gpu_sku}" \
  "inferscale.io/gpu-count=${gpu_count}"
k3s kubectl get nodes
nvidia-smi --query-gpu=index,name,memory.total,driver_version --format=csv
echo "Host bootstrap complete. Install device-plugin/DCGM components after setting explicit chart versions."
