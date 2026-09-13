#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${script_dir}/../lib/common.sh"
cluster_name="${INFERSCALE_K3D_CLUSTER:-inferscale-dev}"

require_command k3d
require_command kubectl
cluster_exists "${cluster_name}" || die "k3d cluster ${cluster_name} does not exist"
kubectl config use-context "k3d-${cluster_name}" >/dev/null

kubectl get nodes
kubectl get pods -n inferscale-system
kubectl get pods -n inferscale-monitoring
kubectl get gateway,httproute -A 2>/dev/null || true

failed=0
for check in \
  'inferscale-system deployment/valkey' \
  'inferscale-monitoring deployment/prometheus' \
  'inferscale-monitoring deployment/otel-collector' \
  'inferscale-monitoring deployment/tempo' \
  'inferscale-monitoring deployment/grafana'; do
  read -r namespace resource <<<"${check}"
  if ! kubectl -n "${namespace}" wait --for=condition=Available "${resource}" --timeout=5s >/dev/null; then
    echo "not ready: ${namespace}/${resource}" >&2
    failed=1
  fi
done

if kubectl -n inferscale-system get deployment inferscale-api >/dev/null 2>&1; then
  for resource in deployment/inferscale-api deployment/inferscale-controller deployment/inferscale-admission; do
    kubectl -n inferscale-system wait --for=condition=Available "${resource}" --timeout=5s >/dev/null || failed=1
  done
fi

if kubectl -n inferscale-system get configmap inferscale-config >/dev/null 2>&1; then
  fake_runtime="$(kubectl -n inferscale-system get configmap inferscale-config -o jsonpath='{.data.INFERSCALE_FAKE_RUNTIME}')"
  echo "Local fake runtime: ${fake_runtime:-false}"
  if [[ "${fake_runtime}" == "true" ]]; then
    kubectl get deployments -A -l app.kubernetes.io/component=model-server \
      -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,IMAGE:.spec.template.spec.containers[0].image'
  fi
fi

echo "Grafana:    kubectl -n inferscale-monitoring port-forward svc/grafana 3000:3000"
echo "Prometheus: kubectl -n inferscale-monitoring port-forward svc/prometheus 9090:9090"
echo "Tempo:      kubectl -n inferscale-monitoring port-forward svc/tempo 3200:3200"
exit "${failed}"
