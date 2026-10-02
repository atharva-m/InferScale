#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ "$#" -ne 1 && "$#" -ne 3 ]] || [[ ! -d "$1" ]]; then
  echo "usage: $0 <reviewed-candidate-bundle-directory> [--context <kubectl-context>]" >&2
  exit 64
fi
kubectl_args=()
if [[ "$#" -eq 3 ]]; then
  if [[ "$2" != --context || -z "$3" ]]; then
    echo 'expected --context <kubectl-context> after the bundle directory' >&2
    exit 64
  fi
  kubectl_args=(--context "$3")
fi
command -v kubectl >/dev/null 2>&1 || { echo 'kubectl is required' >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo 'python3 is required' >&2; exit 1; }

# Use a private snapshot so editing/re-rendering the review directory during an
# installation cannot change the already-verified apply input.
umask 077
snapshot="$(mktemp -d)"
trap 'rm -rf -- "${snapshot}"' EXIT
for filename in candidate-bundle.json 00-namespaces.yaml 10-crds.yaml 20-support.yaml 30-addon.yaml 40-controllers.yaml; do
  cp -- "$1/${filename}" "${snapshot}/${filename}"
done
python3 "${repo_root}/scripts/render-candidate-dependencies.py" verify "${snapshot}"

apply_stage() {
  kubectl "${kubectl_args[@]}" apply --server-side --field-manager=inferscale-dependencies -f "${snapshot}/$1"
}

echo 'Installing opt-in qualification dependencies; production conformance remains pending.'
apply_stage 00-namespaces.yaml
apply_stage 10-crds.yaml
# The addon decides whether to start its InferencePool controller at startup.
# Every CRD must be established before any controller is allowed to start.
kubectl "${kubectl_args[@]}" wait --for=condition=Established --timeout=300s -f "${snapshot}/10-crds.yaml"
apply_stage 20-support.yaml
apply_stage 30-addon.yaml
kubectl "${kubectl_args[@]}" rollout status -n envoy-ai-gateway-system deployment/ai-gateway-controller --timeout=300s
apply_stage 40-controllers.yaml
kubectl "${kubectl_args[@]}" rollout status -n envoy-gateway-system deployment/envoy-gateway --timeout=300s
kubectl "${kubectl_args[@]}" rollout status -n keda deployment/keda-operator --timeout=300s
kubectl "${kubectl_args[@]}" rollout status -n keda deployment/keda-metrics-apiserver --timeout=300s
echo 'Candidate controllers rolled out. Exact-image live Gateway conformance is still required.'
