#!/usr/bin/env bash
set -euo pipefail

if [[ "$(id -u)" != "0" ]]; then
  echo "run as root (or with sudo)" >&2
  exit 77
fi
for required in nvidia-smi nvidia-container-runtime systemctl curl sha256sum python3; do
  command -v "${required}" >/dev/null 2>&1 || { echo "${required} is required before Kubernetes bootstrap" >&2; exit 1; }
done
systemctl show --property=Version --value >/dev/null || {
  echo "A full Linux host/VM with a running systemd is required; a rented Docker container is not sufficient" >&2
  exit 1
}

gpu_sku="${INFERSCALE_GPU_SKU:-}"
[[ "${gpu_sku}" =~ ^[A-Za-z0-9]([A-Za-z0-9_.-]*[A-Za-z0-9])?$ && ${#gpu_sku} -le 63 ]] || {
  echo "INFERSCALE_GPU_SKU is required and must be a Kubernetes label value (for example RTX_5090)" >&2
  exit 64
}
gpu_count="$(nvidia-smi --query-gpu=index --format=csv,noheader | awk 'NF {count++} END {print count+0}')"
(( gpu_count > 0 )) || { echo "no NVIDIA GPUs were discovered" >&2; exit 1; }
expected_gpu_count="${INFERSCALE_EXPECTED_GPU_COUNT:-${gpu_count}}"
[[ "${expected_gpu_count}" =~ ^(1|2|4|8)$ ]] || { echo "INFERSCALE_EXPECTED_GPU_COUNT must be 1, 2, 4, or 8 (host total, not tensor parallelism)" >&2; exit 64; }
[[ "${gpu_count}" == "${expected_gpu_count}" ]] || { echo "host has ${gpu_count} GPUs; expected ${expected_gpu_count}" >&2; exit 1; }
node_name="${INFERSCALE_NODE_NAME:-$(hostname -s)}"
[[ "${node_name}" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ && ${#node_name} -le 253 ]] || { echo "INFERSCALE_NODE_NAME must be a Kubernetes node name" >&2; exit 64; }
[[ -z "${K3S_URL:-}" ]] || { echo "This bootstrap creates one server; joining a multi-node inference cluster is outside the v1 cache contract" >&2; exit 64; }

cache_root="${INFERSCALE_MODEL_CACHE_ROOT:-/var/lib/inferscale/models}"
engine_root="${INFERSCALE_ENGINE_CACHE_ROOT:-/var/lib/inferscale/engines}"
for cache_path in "${cache_root}" "${engine_root}"; do
  [[ "${cache_path}" =~ ^/[A-Za-z0-9._/-]+$ && "${cache_path}" != "/" && "${cache_path}" != *..* ]] || { echo "cache roots must be non-root absolute paths without '..'" >&2; exit 64; }
done
install -d -o 65532 -g 65532 -m 2770 "${cache_root}" "${engine_root}"

if ! command -v k3s >/dev/null 2>&1; then
  k3s_version="${K3S_VERSION:-}"
  k3s_installer_sha256="${K3S_INSTALLER_SHA256:-}"
  [[ "${k3s_version}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+\+k3s[0-9]+$ ]] || { echo "K3S_VERSION must be an immutable k3s release" >&2; exit 64; }
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
  tls_args=()
  if [[ -n "${INFERSCALE_K3S_TLS_SAN:-}" ]]; then
    tls_args+=(--tls-san "${INFERSCALE_K3S_TLS_SAN}")
  fi
  INSTALL_K3S_VERSION="${k3s_version}" sh "${installer}" server \
    --write-kubeconfig-mode 0640 \
    --disable traefik \
    --default-runtime nvidia \
    --node-name "${node_name}" \
    --node-label "inferscale.io/gpu-node=true" \
    "${tls_args[@]}"
fi

systemctl enable --now k3s
registered=false
for _ in $(seq 1 60); do
  if k3s kubectl get node "${node_name}" >/dev/null 2>&1; then
    registered=true
    break
  fi
  sleep 2
done
[[ "${registered}" == true ]] || { echo "Kubernetes node ${node_name} did not register within 120 seconds" >&2; exit 1; }
# Existing installations must already have the NVIDIA runtime selected. Do not
# silently restart/reconfigure an occupied node to correct a runtime mismatch.
k3s crictl info | python3 -c 'import json, sys; info = json.load(sys.stdin); runtime = info.get("config", {}).get("containerd", {}).get("defaultRuntimeName"); sys.exit(0 if runtime == "nvidia" else "K3s default runtime must be nvidia; configure default-runtime: nvidia in /etc/rancher/k3s/config.yaml and restart k3s during maintenance")'
k3s kubectl label node "${node_name}" --overwrite \
  "inferscale.io/gpu-node=true" \
  "inferscale.io/gpu-sku=${gpu_sku}" \
  "inferscale.io/gpu-count=${gpu_count}"
k3s kubectl wait --for=condition=Ready "node/${node_name}" --timeout=120s
k3s kubectl get node "${node_name}" -o wide
nvidia-smi --query-gpu=index,name,memory.total,driver_version --format=csv
echo "Host bootstrap complete. Install device-plugin/DCGM components after setting explicit chart versions."
