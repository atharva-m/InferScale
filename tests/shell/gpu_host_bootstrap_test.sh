#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
test_root="$(mktemp -d)"
trap 'rm -rf -- "${test_root}"' EXIT
mkdir -p "${test_root}/bin"
export PATH="${test_root}/bin:/usr/bin:/bin"
export GPU_TEST_LOG="${test_root}/commands.log"
export GPU_TEST_NODES="${test_root}/nodes.json"
export GPU_TEST_BIN="${test_root}/bin"
export GPU_TEST_K3S="${test_root}/k3s-template"
export GPU_TEST_INSTALLER="${test_root}/installer"
export INFERSCALE_GPU_SKU=RTX_5090 INFERSCALE_EXPECTED_GPU_COUNT=8 INFERSCALE_NODE_NAME=gpu-01
export INFERSCALE_MODEL_CACHE_ROOT="${test_root}/models" INFERSCALE_ENGINE_CACHE_ROOT="${test_root}/engines"
export K3S_VERSION=v1.36.2+k3s1 INFERSCALE_K3S_TLS_SAN=gpu.example.test
unset K3S_URL K3S_TOKEN

cat >"${test_root}/bin/id" <<'SH'
#!/usr/bin/env bash
echo 0
SH
cat >"${test_root}/bin/nvidia-smi" <<'SH'
#!/usr/bin/env bash
for ((i=0; i<${GPU_TEST_COUNT:-8}; i++)); do
  if [[ "$*" == *query-gpu=name,memory.total* ]]; then
    echo "NVIDIA GeForce RTX 5090, 32607"
  elif [[ "$*" == *query-gpu=index,name* ]]; then
    echo "$i, NVIDIA GeForce RTX 5090, 32607, 580.126.09"
  else
    echo "$i"
  fi
done
SH
cat >"${test_root}/bin/systemctl" <<'SH'
#!/usr/bin/env bash
printf 'systemctl %s\n' "$*" >>"${GPU_TEST_LOG}"
if [[ "${GPU_TEST_NO_SYSTEMD:-false}" == true ]]; then exit 1; fi
SH
cat >"${test_root}/bin/install" <<'SH'
#!/usr/bin/env bash
printf 'install %s\n' "$*" >>"${GPU_TEST_LOG}"
SH
cat >"${test_root}/bin/nvidia-container-runtime" <<'SH'
#!/usr/bin/env bash
exit 0
SH
cat >"${test_root}/bin/curl" <<'SH'
#!/usr/bin/env bash
while (($#)); do
  if [[ "$1" == --output ]]; then cp "${GPU_TEST_INSTALLER}" "$2"; exit; fi
  shift
done
exit 1
SH
cat >"${GPU_TEST_INSTALLER}" <<'SH'
#!/bin/sh
printf 'installer %s %s\n' "${INSTALL_K3S_VERSION}" "$*" >>"${GPU_TEST_LOG}"
cp "${GPU_TEST_K3S}" "${GPU_TEST_BIN}/k3s"
SH
cat >"${GPU_TEST_K3S}" <<'SH'
#!/usr/bin/env bash
printf 'k3s %s\n' "$*" >>"${GPU_TEST_LOG}"
if [[ "$*" == 'crictl info' ]]; then
  printf '{"config":{"containerd":{"defaultRuntimeName":"%s"}}}\n' "${GPU_TEST_RUNTIME:-nvidia}"
fi
SH
cat >"${test_root}/bin/kubectl" <<'SH'
#!/usr/bin/env bash
printf 'kubectl %s\n' "$*" >>"${GPU_TEST_LOG}"
if [[ "$*" == 'get nodes -o json' ]]; then cat "${GPU_TEST_NODES}"; fi
SH
cat >"${test_root}/bin/helm" <<'SH'
#!/usr/bin/env bash
printf 'helm %s\n' "$*" >>"${GPU_TEST_LOG}"
SH
chmod +x "${test_root}/bin/"* "${GPU_TEST_K3S}"
export K3S_INSTALLER_SHA256="$(sha256sum "${GPU_TEST_INSTALLER}" | awk '{print $1}')"

# Fresh installation configures GPU injection and only labels the selected host.
bash "${repo_root}/scripts/bootstrap-gpu-host.sh" >"${test_root}/bootstrap.out"
grep -Fq 'installer v1.36.2+k3s1 server --write-kubeconfig-mode 0640 --disable traefik --default-runtime nvidia --node-name gpu-01 --node-label inferscale.io/gpu-node=true --tls-san gpu.example.test' "${GPU_TEST_LOG}"
grep -Fq 'k3s kubectl label node gpu-01 --overwrite inferscale.io/gpu-node=true inferscale.io/gpu-sku=RTX_5090 inferscale.io/gpu-count=8' "${GPU_TEST_LOG}"
grep -Fq 'install -d -o 65532 -g 65532 -m 2770' "${GPU_TEST_LOG}"
if grep -Fq -- '--all' "${GPU_TEST_LOG}"; then echo 'bootstrap changed other nodes' >&2; exit 1; fi

# A mismatched allocation, container-only rental, or existing non-GPU runtime
# must fail, instead of advertising an unusable GPU node as ready.
for failure in count systemd runtime; do
  : >"${GPU_TEST_LOG}"
  export GPU_TEST_COUNT=8 GPU_TEST_NO_SYSTEMD=false GPU_TEST_RUNTIME=nvidia
  case "${failure}" in
    count) export GPU_TEST_COUNT=4 ;;
    systemd) export GPU_TEST_NO_SYSTEMD=true ;;
    runtime) export GPU_TEST_RUNTIME=runc ;;
  esac
  if bash "${repo_root}/scripts/bootstrap-gpu-host.sh" >"${test_root}/${failure}.out" 2>&1; then
    echo "bootstrap accepted invalid ${failure}" >&2; exit 1
  fi
  if grep -Fq 'kubectl label' "${GPU_TEST_LOG}"; then echo "invalid ${failure} changed node labels" >&2; exit 1; fi
done
unset GPU_TEST_COUNT GPU_TEST_NO_SYSTEMD GPU_TEST_RUNTIME

write_nodes() {
  python3 - "${GPU_TEST_NODES}" "${1:-valid}" <<'PY'
import json
import pathlib
import sys

node = {
    "metadata": {"name": "gpu-01", "labels": {
        "inferscale.io/gpu-node": "true", "inferscale.io/gpu-sku": "RTX_5090",
        "inferscale.io/gpu-count": "8", "kubernetes.io/hostname": "physical-host-01",
    }},
    "status": {
        "capacity": {"nvidia.com/gpu": "8"}, "allocatable": {"nvidia.com/gpu": "8"},
        "conditions": [{"type": "Ready", "status": "True"}],
    },
}
case = sys.argv[2]
if case == "split":
    node["status"]["allocatable"]["nvidia.com/gpu"] = "4"
elif case == "sharing":
    node["metadata"]["labels"]["nvidia.com/gpu.sharing-strategy"] = "time-slicing"
elif case == "unready":
    node["status"]["conditions"][0]["status"] = "False"
elif case == "cache-placement":
    del node["metadata"]["labels"]["kubernetes.io/hostname"]
elif case == "wrong-host":
    node["metadata"]["name"] = "another-gpu-host"
pathlib.Path(sys.argv[1]).write_text(json.dumps({"items": [node]}))
PY
}
write_nodes
bash "${repo_root}/scripts/verify-remote-gpu.sh" 8 --node gpu-01 >"${test_root}/verification.out"
grep -Fq 'kubernetes.io/hostname=physical-host-01' "${test_root}/verification.out"
for failure in split sharing unready cache-placement wrong-host; do
  write_nodes "${failure}"
  if bash "${repo_root}/scripts/verify-remote-gpu.sh" 8 --node gpu-01 >"${test_root}/verify-${failure}.out" 2>&1; then
    echo "GPU verification accepted invalid ${failure}" >&2; exit 1
  fi
done

# Charts must schedule on GPU hosts without NFD, use the NVIDIA runtime, avoid
# stale Helm sharing values, and keep privileged DCGM out of monitoring.
: >"${GPU_TEST_LOG}"
NVIDIA_DEVICE_PLUGIN_CHART_VERSION=0.17.1 DCGM_EXPORTER_CHART_VERSION=4.8.1 \
  bash "${repo_root}/scripts/install-gpu-components.sh" >/dev/null
[[ "$(grep -c -- '--reset-values' "${GPU_TEST_LOG}")" -eq 2 ]]
[[ "$(grep -c -- '--set runtimeClassName=nvidia' "${GPU_TEST_LOG}")" -eq 2 ]]
[[ "$(grep -Fc -- '--set-string nodeSelector.inferscale\.io/gpu-node=true' "${GPU_TEST_LOG}")" -eq 2 ]]
grep -Fq -- '--set affinity=null --set migStrategy=none --set failOnInitError=true' "${GPU_TEST_LOG}"
grep -Fq 'dcgm-exporter nvidia/dcgm-exporter --namespace inferscale-gpu-system' "${GPU_TEST_LOG}"
if grep -Fq -- '--namespace inferscale-monitoring' "${GPU_TEST_LOG}"; then echo 'DCGM uses restricted monitoring namespace' >&2; exit 1; fi
python3 - "${repo_root}/infra/bootstrap/gpu-components.yaml" <<'PY'
import pathlib
import sys
import yaml

docs = list(yaml.safe_load_all(pathlib.Path(sys.argv[1]).read_text()))
namespaces = {d["metadata"]["name"]: d for d in docs if d["kind"] == "Namespace"}
assert set(namespaces) == {"nvidia-device-plugin", "inferscale-gpu-system"}
assert all(d["metadata"]["labels"]["pod-security.kubernetes.io/enforce"] == "privileged" for d in namespaces.values())
policy = next(d for d in docs if d["kind"] == "NetworkPolicy")
assert policy["metadata"]["namespace"] == "inferscale-gpu-system"
assert policy["spec"]["ingress"] == [{
    "from": [{"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "inferscale-monitoring"}}}],
    "ports": [{"protocol": "TCP", "port": 9400}],
}]
PY
echo "GPU bootstrap, physical host inventory, and GPU component placement checks passed"
