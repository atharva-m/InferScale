#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf -- "${test_root}"' EXIT
fixture_dir="${test_root}/fixtures"
fake_bin="${test_root}/bin"
applied_dir="${test_root}/applied"
mkdir -p "${fixture_dir}" "${fake_bin}" "${applied_dir}"

cat >"${fixture_dir}/gateway-api.yaml" <<'YAML'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
YAML
cat >"${fixture_dir}/envoy-gateway.yaml" <<'YAML'
apiVersion: v1
kind: ConfigMap
metadata:
  name: envoy-gateway-config
  namespace: envoy-gateway-system
data:
  envoy-gateway.yaml: |
    apiVersion: gateway.envoyproxy.io/v1alpha1
    kind: EnvoyGateway
    provider:
      kubernetes:
        shutdownManager:
          image: envoyproxy/gateway:v1.8.0
        rateLimitDeployment:
          container:
            image: docker.io/envoyproxy/ratelimit:ff287602
      type: Kubernetes
---
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: envoy-gateway
          image: envoyproxy/gateway:v1.8.0
---
kind: Job
spec:
  template:
    spec:
      containers:
        - name: envoy-gateway-certgen
          image: envoyproxy/gateway:v1.8.0
YAML
cat >"${fixture_dir}/inference-extension.yaml" <<'YAML'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
YAML
cat >"${fixture_dir}/llmd-objective.yaml" <<'YAML'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
YAML
cat >"${fixture_dir}/keda.yaml" <<'YAML'
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: keda-metrics-apiserver
          image: ghcr.io/kedacore/keda-metrics-apiserver:2.20.1
---
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: keda-operator
          image: ghcr.io/kedacore/keda:2.20.1
YAML
cat >"${fixture_dir}/service-monitor.yaml" <<'YAML'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
YAML
cat >"${fixture_dir}/pod-monitor.yaml" <<'YAML'
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
YAML

checksum() {
  sha256sum "$1" | awk '{print $1}'
}

write_lock() {
  local destination="$1"
  local keda_checksum="$2"
  cat >"${destination}" <<YAML
components:
  gatewayAPI:
    manifestURL: https://fixtures.invalid/gateway-api.yaml
    manifestSHA256: $(checksum "${fixture_dir}/gateway-api.yaml")
  envoyGateway:
    manifestURL: https://fixtures.invalid/envoy-gateway.yaml
    manifestSHA256: $(checksum "${fixture_dir}/envoy-gateway.yaml")
    manifestControllerImage: envoyproxy/gateway:v1.8.0
    image: docker.io/envoyproxy/gateway
    imageDigest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
    manifestRateLimitImage: docker.io/envoyproxy/ratelimit:ff287602
    rateLimitImage: docker.io/envoyproxy/ratelimit
    rateLimitImageDigest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
  gatewayAPIInferenceExtension:
    manifestURL: https://fixtures.invalid/inference-extension.yaml
    manifestSHA256: $(checksum "${fixture_dir}/inference-extension.yaml")
  llmd:
    objectiveCRDURL: https://fixtures.invalid/llmd-objective.yaml
    objectiveCRDSHA256: $(checksum "${fixture_dir}/llmd-objective.yaml")
  keda:
    manifestURL: https://fixtures.invalid/keda.yaml
    manifestSHA256: ${keda_checksum}
    manifestOperatorImage: ghcr.io/kedacore/keda:2.20.1
    image: ghcr.io/kedacore/keda
    imageDigest: sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
    manifestMetricsServerImage: ghcr.io/kedacore/keda-metrics-apiserver:2.20.1
    metricsServerImage: ghcr.io/kedacore/keda-metrics-apiserver
    metricsServerImageDigest: sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd
  prometheusOperator:
    serviceMonitorCRDURL: https://fixtures.invalid/service-monitor.yaml
    serviceMonitorCRDSHA256: $(checksum "${fixture_dir}/service-monitor.yaml")
    podMonitorCRDURL: https://fixtures.invalid/pod-monitor.yaml
    podMonitorCRDSHA256: $(checksum "${fixture_dir}/pod-monitor.yaml")
YAML
}

cat >"${fake_bin}/curl" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
url=""
destination=""
while (($#)); do
  case "$1" in
    -o)
      destination="$2"
      shift 2
      ;;
    https://*)
      url="$1"
      shift
      ;;
    *)
      shift
      ;;
  esac
done
[[ -n "${url}" && -n "${destination}" ]]
cp -- "${FIXTURE_DIR}/${url##*/}" "${destination}"
SCRIPT

cat >"${fake_bin}/kubectl" <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${KUBECTL_LOG}"
if [[ "${1:-}" != "apply" ]]; then
  exit 0
fi
manifest=""
while (($#)); do
  if [[ "$1" == "-f" ]]; then
    manifest="$2"
    break
  fi
  shift
done
[[ -n "${manifest}" ]]
count="$(find "${APPLIED_DIR}" -maxdepth 1 -type f | wc -l)"
cp -- "${manifest}" "${APPLIED_DIR}/$(printf '%02d' "$((count + 1))").yaml"
SCRIPT
chmod +x "${fake_bin}/curl" "${fake_bin}/kubectl"

run_installer() {
  local lock_file="$1"
  env \
    PATH="${fake_bin}:/usr/bin:/bin" \
    FIXTURE_DIR="${fixture_dir}" \
    APPLIED_DIR="${applied_dir}" \
    KUBECTL_LOG="${test_root}/kubectl.log" \
    INFERSCALE_VERSIONS_LOCK="${lock_file}" \
    "${repo_root}/scripts/install-platform-dependencies.sh"
}

lock_file="${test_root}/versions.lock.yaml"
write_lock "${lock_file}" "$(checksum "${fixture_dir}/keda.yaml")"
run_installer "${lock_file}" >/dev/null

[[ "$(grep -c '^apply ' "${test_root}/kubectl.log")" -eq 8 ]]
[[ "$(grep -c '^wait ' "${test_root}/kubectl.log")" -eq 3 ]]
if grep -Fq 'set image' "${test_root}/kubectl.log"; then
  echo "dependency installer mutates images after apply" >&2
  exit 1
fi

# The controller must receive its deployment mode on first apply, and the
# namespace plus scoped permissions must already exist before that apply.
grep -Fq 'name: inferscale-gateway' "${applied_dir}/02.yaml"
grep -Fq 'name: inferscale-envoy-infra-manager' "${applied_dir}/02.yaml"
grep -Fq 'name: inferscale-envoy-infra-reader' "${applied_dir}/02.yaml"
grep -Fq 'name: inferscale-envoy-token-reviewer' "${applied_dir}/02.yaml"
[[ "$(grep -c 'type: GatewayNamespace' "${applied_dir}/03.yaml")" -eq 1 ]]
awk '
  /^      kubernetes:$/ {
    getline; if ($0 != "        deploy:") exit 1
    getline; if ($0 != "          type: GatewayNamespace") exit 1
    found = 1
  }
  END { if (!found) exit 1 }
' "${applied_dir}/03.yaml"
python3 - "${applied_dir}/02.yaml" "${applied_dir}/03.yaml" <<'PY'
import pathlib
import sys

import yaml

access = list(yaml.safe_load_all(pathlib.Path(sys.argv[1]).read_text()))
by_key = {(document["kind"], document["metadata"]["name"]): document for document in access}
assert len(access) == len(by_key) == 7
assert ("Namespace", "inferscale-gateway") in by_key
role = by_key["Role", "inferscale-envoy-infra-manager"]
assert role["metadata"]["namespace"] == "inferscale-gateway"
expected_resources = {
    "": {"serviceaccounts", "services", "configmaps"},
    "apps": {"deployments", "daemonsets"},
    "autoscaling": {"horizontalpodautoscalers"},
    "policy": {"poddisruptionbudgets"},
}
for rule in role["rules"]:
    assert len(rule["apiGroups"]) == 1
    assert set(rule["resources"]) == expected_resources.pop(rule["apiGroups"][0])
    assert set(rule["verbs"]) == {
        "create", "get", "list", "delete", "deletecollection", "patch", "watch"
    }
assert not expected_resources
reader = by_key["ClusterRole", "inferscale-envoy-infra-reader"]
assert reader["rules"] == [
    {"apiGroups": [""], "resources": ["serviceaccounts"], "verbs": ["get", "list", "watch"]},
    {"apiGroups": ["autoscaling"], "resources": ["horizontalpodautoscalers"], "verbs": ["get", "list", "watch"]},
    {"apiGroups": ["policy"], "resources": ["poddisruptionbudgets"], "verbs": ["get", "list", "watch"]},
]
cluster_role = by_key["ClusterRole", "inferscale-envoy-token-reviewer"]
assert cluster_role["rules"] == [{
    "apiGroups": ["authentication.k8s.io"],
    "resources": ["tokenreviews"],
    "verbs": ["create"],
}]
for binding_kind, role_kind, name in [
    ("RoleBinding", "Role", "inferscale-envoy-infra-manager"),
    ("ClusterRoleBinding", "ClusterRole", "inferscale-envoy-infra-reader"),
    ("ClusterRoleBinding", "ClusterRole", "inferscale-envoy-token-reviewer"),
]:
    binding = by_key[binding_kind, name]
    assert binding["roleRef"]["kind"] == role_kind
    assert binding["roleRef"]["name"] == name
    assert (role_kind, name) in by_key
    assert binding["subjects"] == [{
        "kind": "ServiceAccount", "name": "envoy-gateway", "namespace": "envoy-gateway-system"
    }]
assert by_key["RoleBinding", "inferscale-envoy-infra-manager"]["metadata"]["namespace"] == "inferscale-gateway"
controller_documents = list(yaml.safe_load_all(pathlib.Path(sys.argv[2]).read_text()))
config_map = next(document for document in controller_documents if document["kind"] == "ConfigMap")
config = yaml.safe_load(config_map["data"]["envoy-gateway.yaml"])
assert config["provider"]["kubernetes"]["deploy"]["type"] == "GatewayNamespace"
PY
grep -R -Fq 'docker.io/envoyproxy/gateway@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' "${applied_dir}"
grep -R -Fq 'docker.io/envoyproxy/ratelimit@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' "${applied_dir}"
grep -R -Fq 'ghcr.io/kedacore/keda@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc' "${applied_dir}"
grep -R -Fq 'ghcr.io/kedacore/keda-metrics-apiserver@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd' "${applied_dir}"
if grep -R -Eq '^[[:space:]]*image:[[:space:]]+[^[:space:]@]+:[^[:space:]@]+$' "${applied_dir}"; then
  echo "an applied dependency workload retained a tag-only image" >&2
  exit 1
fi

# Any checksum failure must happen before the first kubectl apply.
bad_checksum_lock="${test_root}/bad-checksum.lock.yaml"
cp -- "${lock_file}" "${bad_checksum_lock}"
sed -i 's/podMonitorCRDSHA256: [0-9a-f]\{64\}/podMonitorCRDSHA256: ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff/' "${bad_checksum_lock}"
: >"${test_root}/kubectl.log"
find "${applied_dir}" -maxdepth 1 -type f -delete
if run_installer "${bad_checksum_lock}" >/dev/null 2>"${test_root}/bad-checksum.err"; then
  echo "dependency installer accepted a checksum mismatch" >&2
  exit 1
fi
[[ ! -s "${test_root}/kubectl.log" ]]
grep -Fq 'checksum mismatch' "${test_root}/bad-checksum.err"

# A future upstream config shape cannot silently restore ControllerNamespace or
# receive duplicate YAML keys. Reject it before any cluster mutation.
cp -- "${fixture_dir}/envoy-gateway.yaml" "${test_root}/envoy-original.yaml"
for change in missing-provider existing-deploy duplicate-config; do
  cp -- "${test_root}/envoy-original.yaml" "${fixture_dir}/envoy-gateway.yaml"
  case "${change}" in
    missing-provider)
      sed -i 's/^      kubernetes:$/      changedKubernetes:/' "${fixture_dir}/envoy-gateway.yaml"
      ;;
    existing-deploy)
      sed -i '/^      kubernetes:$/a\        deploy:\n          type: ControllerNamespace' "${fixture_dir}/envoy-gateway.yaml"
      ;;
    duplicate-config)
      printf '\n---\ndata:\n  envoy-gateway.yaml: |\n    provider:\n      kubernetes: {}\n' >>"${fixture_dir}/envoy-gateway.yaml"
      ;;
  esac
  write_lock "${test_root}/changed-envoy.lock.yaml" "$(checksum "${fixture_dir}/keda.yaml")"
  : >"${test_root}/kubectl.log"
  if run_installer "${test_root}/changed-envoy.lock.yaml" >/dev/null 2>"${test_root}/changed-envoy.err"; then
    echo "dependency installer accepted Envoy config drift: ${change}" >&2
    exit 1
  fi
  [[ ! -s "${test_root}/kubectl.log" ]]
  grep -Fq 'expected exactly one default Kubernetes provider' "${test_root}/changed-envoy.err"
done
cp -- "${test_root}/envoy-original.yaml" "${fixture_dir}/envoy-gateway.yaml"

# The selected KEDA core bundle has no admission-webhook workload. If an
# upstream update adds one, it is rejected before apply until its digest and
# exact rewrite are added to the lock contract.
cat >>"${fixture_dir}/keda.yaml" <<'YAML'
---
kind: Deployment
spec:
  template:
    spec:
      containers:
        - name: keda-admission-webhooks
          image: ghcr.io/kedacore/keda-admission-webhooks:2.20.1
YAML
webhook_lock="${test_root}/webhook.lock.yaml"
write_lock "${webhook_lock}" "$(checksum "${fixture_dir}/keda.yaml")"
: >"${test_root}/kubectl.log"
if run_installer "${webhook_lock}" >/dev/null 2>"${test_root}/webhook.err"; then
  echo "dependency installer accepted an unpinned KEDA admission webhook" >&2
  exit 1
fi
[[ ! -s "${test_root}/kubectl.log" ]]
grep -Fq 'keda-admission-webhooks:2.20.1' "${test_root}/webhook.err"

echo "platform dependency checksum, image-lock, and gateway namespace contracts passed"
