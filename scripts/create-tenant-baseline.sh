#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 3 ]]; then
  echo "usage: $0 <tenant-namespace> <tenant-slug> <gpu-quota>" >&2
  exit 64
fi
namespace="$1"
tenant="$2"
gpu_quota="$3"
dns_label='^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'
[[ "${namespace}" =~ ${dns_label} && ${#namespace} -le 63 ]] || { echo "invalid namespace" >&2; exit 64; }
[[ "${tenant}" =~ ${dns_label} && ${#tenant} -le 63 ]] || { echo "invalid tenant slug" >&2; exit 64; }
[[ "${gpu_quota}" =~ ^[1-9][0-9]*$ && "${gpu_quota}" -le 64 ]] || { echo "GPU quota must be 1-64" >&2; exit 64; }
gpu_resource_quota="$((gpu_quota * 2))"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
rendered="$(mktemp)"
trap 'rm -f -- "$rendered"' EXIT
sed \
  -e "s/TENANT_NAMESPACE/${namespace}/g" \
  -e "s/TENANT_SLUG/${tenant}/g" \
  -e "s/GPU_RESOURCE_QUOTA/${gpu_resource_quota}/g" \
  -e "s/GPU_QUOTA/${gpu_quota}/g" \
  "${repo_root}/deploy/templates/tenant-baseline.yaml" >"${rendered}"
kubectl apply --server-side --field-manager=inferscale-tenant-bootstrap -f "${rendered}"
