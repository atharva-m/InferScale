#!/usr/bin/env bash
set -euo pipefail

script_root="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_root}/.." && pwd)"
source "${script_root}/lib/common.sh"
require_command kubectl
require_command curl
require_command sha256sum
require_command awk

versions_lock="${INFERSCALE_VERSIONS_LOCK:-${repo_root}/versions.lock.yaml}"
[[ -f "${versions_lock}" ]] || die "versions lock not found: ${versions_lock}"

lock_value() {
  local component="$1"
  local key="$2"
  awk -v component="${component}" -v key="${key}" '
    $0 == "  " component ":" { in_component = 1; next }
    in_component && /^  [^ ]/ { exit }
    in_component {
      prefix = "    " key ":"
      if (index($0, prefix) == 1) {
        value = substr($0, length(prefix) + 1)
        sub(/^[[:space:]]+/, "", value)
        sub(/[[:space:]]+#.*$/, "", value)
        if (value ~ /^".*"$/ || value ~ /^'\''.*'\''$/) {
          value = substr(value, 2, length(value) - 2)
        }
        print value
        found = 1
        exit
      }
    }
    END { if (!found) exit 1 }
  ' "${versions_lock}"
}

required_lock_value() {
  local component="$1"
  local key="$2"
  local value
  value="$(lock_value "${component}" "${key}")" || die "versions lock omits components.${component}.${key}"
  [[ -n "${value}" ]] || die "versions lock has an empty components.${component}.${key}"
  printf '%s\n' "${value}"
}

require_checksum() {
  local label="$1"
  local checksum="$2"
  [[ "${checksum}" =~ ^[0-9a-f]{64}$ ]] || die "${label} must be a lowercase 64-hex SHA-256 checksum"
}

require_https_url() {
  local label="$1"
  local url="$2"
  [[ "${url}" =~ ^https://[^[:space:]@]+/[^[:space:]]+$ ]] || die "${label} must be a credential-free HTTPS URL"
}

locked_image() {
  local component="$1"
  local image_key="$2"
  local digest_key="$3"
  local image digest
  image="$(required_lock_value "${component}" "${image_key}")"
  digest="$(required_lock_value "${component}" "${digest_key}")"
  [[ "${image}" =~ ^[^[:space:]@]+$ ]] || die "components.${component}.${image_key} must be an image repository without a digest"
  [[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || die "components.${component}.${digest_key} must be an immutable sha256 digest"
  printf '%s@%s\n' "${image}" "${digest}"
}

download_and_verify() {
  local label="$1"
  local url="$2"
  local checksum="$3"
  local destination="$4"
  require_https_url "${label} URL" "${url}"
  require_checksum "${label} checksum" "${checksum}"
  curl --fail --show-error --silent --location --proto '=https' --tlsv1.2 "${url}" -o "${destination}"
  printf '%s  %s\n' "${checksum}" "${destination}" | sha256sum --check --status \
    || die "${label} checksum mismatch"
}

replace_manifest_image() {
  local label="$1"
  local manifest="$2"
  local source_image="$3"
  local locked_image_ref="$4"
  local expected_count="$5"
  local rewritten="${manifest}.rewritten"

  [[ "${source_image}" =~ ^[^[:space:]@]+:[^[:space:]@]+$ ]] \
    || die "${label} source image must identify the exact tagged image in the checksummed manifest"
  [[ "${locked_image_ref}" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]] \
    || die "${label} replacement image must use an immutable sha256 digest"

  if ! awk -v source_image="${source_image}" -v locked_image_ref="${locked_image_ref}" -v expected_count="${expected_count}" '
    {
      value = $0
      sub(/^[[:space:]]*image:[[:space:]]*/, "", value)
      if (value == source_image) {
        match($0, /^[[:space:]]*/)
        print substr($0, RSTART, RLENGTH) "image: " locked_image_ref
        replacements++
        next
      }
      print
    }
    END { if (replacements != expected_count) exit 42 }
  ' "${manifest}" >"${rewritten}"; then
    rm -f -- "${rewritten}"
    die "${label} expected ${expected_count} image occurrence(s) in the verified manifest"
  fi
  mv -- "${rewritten}" "${manifest}"
}

verify_manifest_images() {
  local label="$1"
  local manifest="$2"
  awk -v label="${label}" '
    /^[[:space:]]*image:/ {
      value = $0
      sub(/^[[:space:]]*image:[[:space:]]*/, "", value)
      if (value == "") next
      if (value !~ /^[^[:space:]@]+@sha256:[0-9a-f]{64}$/) {
        print "error: " label " contains a mutable workload image after locking: " value > "/dev/stderr"
        invalid = 1
      }
    }
    END { if (invalid) exit 1 }
  ' "${manifest}" || die "${label} image inventory is not immutable"
}

configure_gateway_namespace() {
  local manifest="$1"
  local rewritten="${manifest}.rewritten"

  # This is a counted transform of the checksummed v1.8 manifest, not a
  # cluster-side patch: the controller must start in the intended mode. Limit
  # the match to the embedded EnvoyGateway configuration, excluding CRD schemas.
  if ! awk '
    /^---[[:space:]]*$/ { in_config = 0; in_provider = 0; in_kubernetes = 0 }
    /^  envoy-gateway.yaml: \|$/ { in_config = 1; configs++ }
    in_config && /^    provider:$/ { in_provider = 1 }
    in_provider && /^      kubernetes:$/ {
      print
      print "        deploy:"
      print "          type: GatewayNamespace"
      in_kubernetes = 1
      replacements++
      next
    }
    in_kubernetes && /^        deploy:/ { unexpected_deploy = 1 }
    { print }
    END { if (configs != 1 || replacements != 1 || unexpected_deploy) exit 42 }
  ' "${manifest}" >"${rewritten}"; then
    rm -f -- "${rewritten}"
    die "Envoy Gateway expected exactly one default Kubernetes provider in the verified manifest"
  fi
  mv -- "${rewritten}" "${manifest}"
}

gateway_api_url="$(required_lock_value gatewayAPI manifestURL)"
gateway_api_sha256="$(required_lock_value gatewayAPI manifestSHA256)"
envoy_gateway_url="$(required_lock_value envoyGateway manifestURL)"
envoy_gateway_sha256="$(required_lock_value envoyGateway manifestSHA256)"
envoy_gateway_source_image="$(required_lock_value envoyGateway manifestControllerImage)"
envoy_gateway_image="$(locked_image envoyGateway image imageDigest)"
envoy_rate_limit_source_image="$(required_lock_value envoyGateway manifestRateLimitImage)"
envoy_rate_limit_image="$(locked_image envoyGateway rateLimitImage rateLimitImageDigest)"
inference_extension_url="$(required_lock_value gatewayAPIInferenceExtension manifestURL)"
inference_extension_sha256="$(required_lock_value gatewayAPIInferenceExtension manifestSHA256)"
llmd_objective_url="$(required_lock_value llmd objectiveCRDURL)"
llmd_objective_sha256="$(required_lock_value llmd objectiveCRDSHA256)"
keda_url="$(required_lock_value keda manifestURL)"
keda_sha256="$(required_lock_value keda manifestSHA256)"
keda_operator_source_image="$(required_lock_value keda manifestOperatorImage)"
keda_operator_image="$(locked_image keda image imageDigest)"
keda_metrics_source_image="$(required_lock_value keda manifestMetricsServerImage)"
keda_metrics_image="$(locked_image keda metricsServerImage metricsServerImageDigest)"
service_monitor_url="$(required_lock_value prometheusOperator serviceMonitorCRDURL)"
service_monitor_sha256="$(required_lock_value prometheusOperator serviceMonitorCRDSHA256)"
pod_monitor_url="$(required_lock_value prometheusOperator podMonitorCRDURL)"
pod_monitor_sha256="$(required_lock_value prometheusOperator podMonitorCRDSHA256)"

dependency_dir="$(mktemp -d)"
trap 'rm -rf -- "${dependency_dir}"' EXIT
gateway_api_file="${dependency_dir}/gateway-api.yaml"
envoy_gateway_file="${dependency_dir}/envoy-gateway.yaml"
inference_extension_file="${dependency_dir}/inference-extension.yaml"
llmd_objective_file="${dependency_dir}/llmd-objective.yaml"
keda_file="${dependency_dir}/keda.yaml"
service_monitor_file="${dependency_dir}/service-monitor.yaml"
pod_monitor_file="${dependency_dir}/pod-monitor.yaml"
gateway_namespace_file="${dependency_dir}/gateway-namespace.yaml"

# Resolve and verify the complete dependency set before mutating the cluster.
download_and_verify "Gateway API" "${gateway_api_url}" "${gateway_api_sha256}" "${gateway_api_file}"
download_and_verify "Envoy Gateway" "${envoy_gateway_url}" "${envoy_gateway_sha256}" "${envoy_gateway_file}"
download_and_verify "Gateway API Inference Extension" "${inference_extension_url}" "${inference_extension_sha256}" "${inference_extension_file}"
download_and_verify "llm-d InferenceObjective CRD" "${llmd_objective_url}" "${llmd_objective_sha256}" "${llmd_objective_file}"
download_and_verify "KEDA" "${keda_url}" "${keda_sha256}" "${keda_file}"
download_and_verify "Prometheus Operator ServiceMonitor CRD" "${service_monitor_url}" "${service_monitor_sha256}" "${service_monitor_file}"
download_and_verify "Prometheus Operator PodMonitor CRD" "${pod_monitor_url}" "${pod_monitor_sha256}" "${pod_monitor_file}"

# Rewrite every workload-bearing upstream manifest before its first apply. The
# expected counts bind these transforms to the checksummed upstream structure.
replace_manifest_image "Envoy Gateway controller, certgen, and shutdown manager" "${envoy_gateway_file}" \
  "${envoy_gateway_source_image}" "${envoy_gateway_image}" 3
replace_manifest_image "Envoy Gateway rate-limit service" "${envoy_gateway_file}" \
  "${envoy_rate_limit_source_image}" "${envoy_rate_limit_image}" 1
replace_manifest_image "KEDA operator" "${keda_file}" \
  "${keda_operator_source_image}" "${keda_operator_image}" 1
replace_manifest_image "KEDA metrics API server" "${keda_file}" \
  "${keda_metrics_source_image}" "${keda_metrics_image}" 1
configure_gateway_namespace "${envoy_gateway_file}"

# Infrastructure writes stay scoped to the one namespace hosting our Gateway.
# GatewayNamespace mode also needs cluster-wide infrastructure informer reads
# and TokenReviews for proxy JWT authentication.
cat "${repo_root}/deploy/base/gateway/namespace.yaml" >"${gateway_namespace_file}"
printf '\n---\n' >>"${gateway_namespace_file}"
cat "${repo_root}/deploy/base/gateway/controller-rbac.yaml" >>"${gateway_namespace_file}"

for entry in \
  "Gateway API|${gateway_api_file}" \
  "Envoy Gateway namespace access|${gateway_namespace_file}" \
  "Envoy Gateway|${envoy_gateway_file}" \
  "Gateway API Inference Extension|${inference_extension_file}" \
  "llm-d InferenceObjective CRD|${llmd_objective_file}" \
  "KEDA|${keda_file}" \
  "Prometheus Operator ServiceMonitor CRD|${service_monitor_file}" \
  "Prometheus Operator PodMonitor CRD|${pod_monitor_file}"; do
  verify_manifest_images "${entry%%|*}" "${entry#*|}"
done

echo "Installing checksum-verified, digest-pinned platform dependencies from ${versions_lock}"
for entry in \
  "Gateway API|${gateway_api_file}" \
  "Envoy Gateway namespace access|${gateway_namespace_file}" \
  "Envoy Gateway|${envoy_gateway_file}" \
  "Gateway API Inference Extension|${inference_extension_file}" \
  "llm-d InferenceObjective CRD|${llmd_objective_file}" \
  "KEDA|${keda_file}" \
  "Prometheus Operator ServiceMonitor CRD|${service_monitor_file}" \
  "Prometheus Operator PodMonitor CRD|${pod_monitor_file}"; do
  echo "Installing ${entry%%|*}"
  kubectl apply --server-side --field-manager=inferscale-dependencies -f "${entry#*|}"
done

kubectl wait --for=condition=Available -n envoy-gateway-system deployment/envoy-gateway --timeout=300s
kubectl wait --for=condition=Available -n keda deployment/keda-operator --timeout=300s
kubectl wait --for=condition=Available -n keda deployment/keda-metrics-apiserver --timeout=300s
