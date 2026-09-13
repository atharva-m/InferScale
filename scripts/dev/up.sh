#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${script_dir}/../lib/common.sh"
repo_root="$(repository_root)"
cluster_name="${INFERSCALE_K3D_CLUSTER:-inferscale-dev}"
k3s_image="${INFERSCALE_K3S_IMAGE:-rancher/k3s:v1.36.2-k3s1}"
skip_benchmark_image="${INFERSCALE_DEV_SKIP_BENCHMARK_IMAGE:-false}"

case "${skip_benchmark_image}" in
  true|false) ;;
  *) die "INFERSCALE_DEV_SKIP_BENCHMARK_IMAGE must be true or false" ;;
esac

require_command docker
require_command k3d
require_command kubectl

if cluster_exists "${cluster_name}"; then
  k3d cluster start "${cluster_name}" >/dev/null 2>&1 || true
else
  k3d cluster create "${cluster_name}" \
    --image "${k3s_image}" \
    --agents 1 \
    --servers 1 \
    --k3s-arg '--disable=traefik@server:*' \
    --port '8080:80@loadbalancer' \
    --port '8443:443@loadbalancer' \
    --wait
fi

kubectl config use-context "k3d-${cluster_name}" >/dev/null
"${repo_root}/scripts/install-platform-dependencies.sh"

kubectl apply --server-side --field-manager=inferscale-bootstrap -f "${repo_root}/deploy/base/inferscale/namespace.yaml"
kubectl apply --server-side --field-manager=inferscale-bootstrap -f "${repo_root}/deploy/overlays/local-wsl/storage-secret.yaml"
kubectl apply --server-side --field-manager=inferscale-bootstrap -k "${repo_root}/deploy/base/storage"
kubectl apply --server-side --field-manager=inferscale-bootstrap -k "${repo_root}/deploy/base/monitoring"
kubectl -n inferscale-system rollout status statefulset/postgres --timeout=300s

services=(api controller admission migrate)
platform_ready=true
for service in "${services[@]}"; do
  if [[ ! -f "${repo_root}/cmd/${service}/main.go" ]]; then
    platform_ready=false
  fi
done

if [[ "${platform_ready}" == "true" ]]; then
  benchmark_image="ghcr.io/inferscale/benchmark:0.1.0-dev"
  if [[ "${skip_benchmark_image}" == "false" ]]; then
    source_commit="$(git -C "${repo_root}" rev-parse HEAD 2>/dev/null || true)"
    source_commit="${source_commit:-unknown}"
    docker build \
      --build-arg "INFERSCALE_GIT_COMMIT=${source_commit}" \
      -t "${benchmark_image}" \
      -f "${repo_root}/benchmarks/runner/Dockerfile" \
      "${repo_root}"
    k3d image import -c "${cluster_name}" "${benchmark_image}"
  else
    echo "Skipping the unused local benchmark-runner build for the live control-plane E2E."
  fi

  # The local-wsl overlay deliberately substitutes a deterministic CPU-only
  # OpenAI server for vLLM. It exercises API→outbox→controller→Gateway/llm-d
  # without claiming GPU or model-runtime coverage.
  fake_runtime_image="ghcr.io/inferscale/fake-runtime:0.1.0-dev"
  docker build --build-arg "COMMAND=fakeruntime" -t "${fake_runtime_image}" -f "${repo_root}/build/Dockerfile" "${repo_root}"
  k3d image import -c "${cluster_name}" "${fake_runtime_image}"
  for service in "${services[@]}"; do
    image="ghcr.io/inferscale/${service}:0.1.0-dev"
    docker build --build-arg "COMMAND=${service}" -t "${image}" -f "${repo_root}/build/Dockerfile" "${repo_root}"
    k3d image import -c "${cluster_name}" "${image}"
  done
  crd_file="${repo_root}/api/crds/platform.inferscale.io_inferencedeployments.yaml"
  if [[ -f "${crd_file}" ]]; then
    kubectl apply --server-side --field-manager=inferscale-bootstrap -f "${crd_file}"
  fi
  kubectl -n inferscale-system delete job inferscale-migrate-000001 inferscale-migrate-000002 inferscale-migrate-000003 inferscale-migrate-000004 inferscale-migrate-000005 --ignore-not-found --wait=true
  kubectl apply --server-side --field-manager=inferscale-bootstrap -k "${repo_root}/deploy/overlays/local-wsl"
  kubectl -n inferscale-system wait --for=condition=complete job/inferscale-migrate-000005 --timeout=300s
  wait_for_deployment inferscale-system inferscale-api 300s
  wait_for_deployment inferscale-system inferscale-controller 300s
  wait_for_deployment inferscale-system inferscale-admission 300s
else
  echo "Control-plane commands are not all present; M0 storage and observability are running."
fi

wait_for_deployment inferscale-system valkey 180s
wait_for_deployment inferscale-monitoring prometheus 300s
wait_for_deployment inferscale-monitoring otel-collector 300s
wait_for_deployment inferscale-monitoring tempo 300s
wait_for_deployment inferscale-monitoring grafana 300s
echo "InferScale local cluster is ready; run make dev-status for endpoints."
