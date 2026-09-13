#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repository_root}"

generated=(
  api/openapi/control.yaml
  api/openapi/inference.yaml
  api/platform/v1alpha1/zz_generated.deepcopy.go
  api/crds/platform.inferscale.io_inferencedeployments.yaml
  deploy/base/inferscale/crd/platform.inferscale.io_inferencedeployments.yaml
  deploy/base/inferscale/generated/role.yaml
  deploy/base/monitoring/prometheus.yml
  deploy/base/monitoring/alerts.yml
  deploy/base/monitoring/otel-collector.yaml
  deploy/base/monitoring/tempo.yaml
)
for dashboard in observability/dashboards/*.json; do
  generated+=("deploy/base/monitoring/dashboards/$(basename "${dashboard}")")
done
backup_dir="$(mktemp -d)"
restore_generated() {
  for path in "${generated[@]}"; do
    if [[ -f "${backup_dir}/${path}" ]]; then
      cp "${backup_dir}/${path}" "${path}"
    fi
  done
  rm -rf -- "${backup_dir}"
}
trap restore_generated EXIT

for path in "${generated[@]}"; do
  [[ -f "${path}" ]] || { echo "missing generated artifact: ${path}" >&2; exit 1; }
  mkdir -p "${backup_dir}/$(dirname "${path}")"
  cp "${path}" "${backup_dir}/${path}"
done
make generate >/dev/null
stale=()
for path in "${generated[@]}"; do
  if ! cmp -s "${backup_dir}/${path}" "${path}"; then
    stale+=("${path}")
  fi
done
if ((${#stale[@]} > 0)); then
  echo "generated files are stale; run make generate" >&2
  for path in "${stale[@]}"; do
    echo "  ${path}" >&2
    diff -u "${backup_dir}/${path}" "${path}" || true
  done
  exit 1
fi
