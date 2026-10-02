#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
render_dir="$(mktemp -d)"
trap 'rm -rf -- "${render_dir}"' EXIT

digest="$(printf 'a%.0s' {1..64})"
export API_IMAGE="registry.example/inferscale/api@sha256:${digest}"
export CONTROLLER_IMAGE="registry.example/inferscale/controller@sha256:${digest}"
export ADMISSION_IMAGE="registry.example/inferscale/admission@sha256:${digest}"
export MIGRATE_IMAGE="registry.example/inferscale/migrate@sha256:${digest}"
export PUBLIC_BASE_URL="https://inference.example.com"
export INFERSCALE_BENCHMARK_PROVIDER="vast"
export INFERSCALE_BENCHMARK_INFERENCE_BASE_URL="https://benchmark.inference.example.com"
export INFERSCALE_BENCHMARK_PROMETHEUS_URL="http://prometheus.inferscale-monitoring.svc.cluster.local:9090"
export INFERSCALE_BENCHMARK_DRIVER_VERSION="580.82.07"
export INFERSCALE_BENCHMARK_CUDA_VERSION="13.0"

"${repo_root}/scripts/render-remote-release.sh" remote >"${render_dir}/release.yaml"
for image in "${API_IMAGE}" "${CONTROLLER_IMAGE}" "${ADMISSION_IMAGE}" "${MIGRATE_IMAGE}"; do
  grep -Fq "image: ${image}" "${render_dir}/release.yaml"
done
grep -Fq "INFERSCALE_PUBLIC_BASE_URL: ${PUBLIC_BASE_URL}" "${render_dir}/release.yaml"
python3 - "${render_dir}/release.yaml" <<'PY'
import pathlib
import sys

import yaml

documents = [value for value in yaml.safe_load_all(pathlib.Path(sys.argv[1]).read_text()) if value]
config = next(
    value
    for value in documents
    if value.get("kind") == "ConfigMap" and value.get("metadata", {}).get("name") == "inferscale-config"
)
expected_authority = {
    "INFERSCALE_BENCHMARK_PROVIDER": "vast",
    "INFERSCALE_BENCHMARK_INFERENCE_BASE_URL": "https://benchmark.inference.example.com",
    "INFERSCALE_BENCHMARK_PROMETHEUS_URL": "http://prometheus.inferscale-monitoring.svc.cluster.local:9090",
    "INFERSCALE_BENCHMARK_DRIVER_VERSION": "580.82.07",
    "INFERSCALE_BENCHMARK_CUDA_VERSION": "13.0",
}
assert all(config["data"].get(name) == value for name, value in expected_authority.items())
api = next(
    value
    for value in documents
    if value.get("kind") == "Deployment" and value.get("metadata", {}).get("name") == "inferscale-api"
)
assert {source["configMapRef"]["name"] for source in api["spec"]["template"]["spec"]["containers"][0]["envFrom"]} == {
    "inferscale-config"
}
PY
if grep -Eq 'ghcr.io/inferscale/(api|controller|admission|migrate):0\.1\.0-dev' "${render_dir}/release.yaml"; then
  echo "release render retained a development control-plane image" >&2
  exit 1
fi

export API_IMAGE="registry.example/inferscale/api:mutable"
if "${repo_root}/scripts/render-remote-release.sh" remote >"${render_dir}/invalid.yaml" 2>"${render_dir}/invalid.err"; then
  echo "mutable release image was accepted" >&2
  exit 1
fi
grep -Fq 'API_IMAGE must be a registry image pinned with @sha256' "${render_dir}/invalid.err"

export API_IMAGE="registry.example/inferscale/api@sha256:${digest}"
for invalid_origin in \
  "https://user:secret@inference.example.com/private" \
  "https://inference.example.com?" \
  "https://inference.example.com#" \
  "https://:443" \
  "https://inference.example.com:0" \
  "https://inference.example.com:65536" \
  " https://inference.example.com" \
  'https://inference.example.com\evil'; do
  export PUBLIC_BASE_URL="${invalid_origin}"
  if "${repo_root}/scripts/render-remote-release.sh" remote >"${render_dir}/invalid-origin.yaml" 2>"${render_dir}/invalid-origin.err"; then
    echo "unsafe public release origin was accepted: ${invalid_origin}" >&2
    exit 1
  fi
  grep -Fq 'PUBLIC_BASE_URL must be an HTTPS origin' "${render_dir}/invalid-origin.err"
done

export PUBLIC_BASE_URL="https://inference.example.com"
unset INFERSCALE_BENCHMARK_PROVIDER
if "${repo_root}/scripts/render-remote-release.sh" remote >"${render_dir}/missing-authority.yaml" 2>"${render_dir}/missing-authority.err"; then
  echo "release render accepted missing benchmark provider authority" >&2
  exit 1
fi
grep -Fq 'INFERSCALE_BENCHMARK_PROVIDER must be a lowercase provider identifier' "${render_dir}/missing-authority.err"

export INFERSCALE_BENCHMARK_PROVIDER="vast"
export INFERSCALE_BENCHMARK_INFERENCE_BASE_URL="http://benchmark.inference.example.com"
if "${repo_root}/scripts/render-remote-release.sh" remote >"${render_dir}/unsafe-inference.yaml" 2>"${render_dir}/unsafe-inference.err"; then
  echo "release render accepted plaintext benchmark inference authority" >&2
  exit 1
fi
grep -Fq 'INFERSCALE_BENCHMARK_INFERENCE_BASE_URL must be an HTTPS origin' "${render_dir}/unsafe-inference.err"

export INFERSCALE_BENCHMARK_INFERENCE_BASE_URL="https://benchmark.inference.example.com"
export INFERSCALE_BENCHMARK_PROMETHEUS_URL="http://user:secret@prometheus:9090"
if "${repo_root}/scripts/render-remote-release.sh" remote >"${render_dir}/unsafe-prometheus.yaml" 2>"${render_dir}/unsafe-prometheus.err"; then
  echo "release render accepted credentialed benchmark Prometheus authority" >&2
  exit 1
fi
grep -Fq 'INFERSCALE_BENCHMARK_PROMETHEUS_URL must be an HTTP(S) origin' "${render_dir}/unsafe-prometheus.err"

export INFERSCALE_BENCHMARK_PROMETHEUS_URL="http://prometheus.inferscale-monitoring.svc.cluster.local:9090"
export INFERSCALE_BENCHMARK_DRIVER_VERSION="set-at-run-time"
if "${repo_root}/scripts/render-remote-release.sh" remote >"${render_dir}/invalid-driver.yaml" 2>"${render_dir}/invalid-driver.err"; then
  echo "release render accepted placeholder benchmark driver authority" >&2
  exit 1
fi
grep -Fq 'INFERSCALE_BENCHMARK_DRIVER_VERSION must be an explicit NVIDIA driver version' "${render_dir}/invalid-driver.err"

export INFERSCALE_BENCHMARK_DRIVER_VERSION="580.82.07"
export INFERSCALE_BENCHMARK_CUDA_VERSION="set-at-run-time"
if "${repo_root}/scripts/render-remote-release.sh" remote >"${render_dir}/invalid-cuda.yaml" 2>"${render_dir}/invalid-cuda.err"; then
  echo "release render accepted placeholder benchmark CUDA authority" >&2
  exit 1
fi
grep -Fq 'INFERSCALE_BENCHMARK_CUDA_VERSION must be an explicit CUDA version' "${render_dir}/invalid-cuda.err"

# The single-host eight-GPU manifest can use external stores without leaving
# development databases running, and all network exceptions remain scoped.
export INFERSCALE_BENCHMARK_CUDA_VERSION="13.0"
export REMOTE_EXTERNAL_STORAGE=true
export CLUSTER_POSTGRES_CIDRS='["10.20.0.10/32"]'
export CLUSTER_VALKEY_CIDRS='["10.20.0.11/32"]'
export CLUSTER_KUBERNETES_API_CIDRS='["10.20.0.5/32"]'
export INFERSCALE_GPU_NODE_NAME="vast-gpu-host"
"${repo_root}/scripts/render-remote-release.sh" vast-8x5090 >"${render_dir}/external.yaml"
python3 - "${render_dir}/external.yaml" <<'PY'
import json
import pathlib
import sys

import yaml

documents = list(yaml.safe_load_all(pathlib.Path(sys.argv[1]).read_text()))
by_key = {(d["kind"], d["metadata"].get("namespace", ""), d["metadata"]["name"]): d for d in documents}
for kind, name in (("StatefulSet", "postgres"), ("Deployment", "valkey"), ("Service", "postgres"), ("Service", "valkey")):
    assert (kind, "inferscale-system", name) not in by_key
assert ("Job", "inferscale-system", "inferscale-migrate-000006") in by_key
assert not any(d["kind"] == "Secret" for d in documents)
config = by_key["ConfigMap", "inferscale-system", "inferscale-config"]["data"]
assert json.loads(config["INFERSCALE_GPU_NODE_SELECTORS"])["RTX_5090"] == {
    "inferscale.io/gpu-sku": "RTX_5090", "kubernetes.io/hostname": "vast-gpu-host",
}
assert json.loads(config["INFERSCALE_KUBERNETES_API_CIDRS"]) == ["10.20.0.5/32"]
assert config["INFERSCALE_KUBERNETES_API_PORT"] == "6443"
profile = by_key["ConfigMap", "inferscale-system", "inferscale-gpu-profile"]["data"]
assert profile["expected_gpu_count"] == "8"
assert profile["supported_tensor_parallelism"] == "1,2,4"
for service, cidr, port, allowed in (
    ("postgres", "10.20.0.10/32", 5432, ["inferscale-api", "inferscale-controller", "inferscale-admission", "inferscale-migrate"]),
    ("valkey", "10.20.0.11/32", 6379, ["inferscale-api", "inferscale-admission"]),
    ("kubernetes-api", "10.20.0.5/32", 6443, ["inferscale-api", "inferscale-controller"]),
):
    policy = by_key["NetworkPolicy", "inferscale-system", f"allow-configured-{service}"]["spec"]
    assert policy["podSelector"]["matchExpressions"] == [{
        "key": "app.kubernetes.io/name", "operator": "In", "values": allowed,
    }]
    assert policy["egress"] == [{"to": [{"ipBlock": {"cidr": cidr}}], "ports": [{"protocol": "TCP", "port": port}]}]
for app in ("api", "admission"):
    container = by_key["Deployment", "inferscale-system", f"inferscale-{app}"]["spec"]["template"]["spec"]["containers"][0]
    env = {entry["name"]: entry for entry in container["env"]}
    assert env["INFERSCALE_VALKEY_URL"]["valueFrom"]["secretKeyRef"] == {
        "name": "inferscale-storage", "key": "valkey_url", "optional": True,
    }
    assert env["INFERSCALE_VALKEY_ADDR"]["valueFrom"]["secretKeyRef"]["optional"] is True
grafana = by_key["Deployment", "inferscale-monitoring", "grafana"]["spec"]["template"]["spec"]["containers"][0]
grafana_env = {entry["name"]: entry for entry in grafana["env"]}
assert grafana_env["GF_AUTH_ANONYMOUS_ENABLED"]["value"] == "false"
assert grafana_env["GF_SECURITY_ADMIN_PASSWORD"]["valueFrom"]["secretKeyRef"] == {
    "name": "grafana-admin", "key": "password",
}
gateway = by_key["Gateway", "inferscale-gateway", "inferscale"]
assert gateway["spec"]["listeners"][0]["hostname"] == "inference.example.com"
PY

for invalid_cidrs in '' '[]' '["0.0.0.0/0"]' '["127.0.0.1/32"]' '["8.8.8.8/32"]' '["10.20.0.1/24"]' '{}'; do
  export CLUSTER_POSTGRES_CIDRS="${invalid_cidrs}"
  if "${repo_root}/scripts/render-remote-release.sh" vast-8x5090 >"${render_dir}/invalid-network.yaml" 2>"${render_dir}/invalid-network.err"; then
    echo "invalid external storage CIDRs were accepted" >&2
    exit 1
  fi
  grep -Fq 'CLUSTER_POSTGRES_CIDRS' "${render_dir}/invalid-network.err"
done
export CLUSTER_POSTGRES_CIDRS='["10.20.0.10/32"]'
export CLUSTER_KUBERNETES_API_CIDRS='["10.20.0.0/24"]'
if "${repo_root}/scripts/render-remote-release.sh" vast-8x5090 >"${render_dir}/invalid-api.yaml" 2>"${render_dir}/invalid-api.err"; then
  echo "subnet-wide Kubernetes API access was accepted" >&2
  exit 1
fi
grep -Fq 'exact /32 or /128' "${render_dir}/invalid-api.err"
export CLUSTER_KUBERNETES_API_CIDRS='["10.20.0.5/32"]'
unset INFERSCALE_GPU_NODE_NAME
if "${repo_root}/scripts/render-remote-release.sh" vast-8x5090 >"${render_dir}/missing-node.yaml" 2>"${render_dir}/missing-node.err"; then
  echo "Vast release omitted explicit GPU node selection" >&2
  exit 1
fi
grep -Fq 'INFERSCALE_GPU_NODE_NAME' "${render_dir}/missing-node.err"
echo "remote image, TLS, external storage, GPU placement, and scoped network rendering passed"

INFERSCALE_IMAGE_PULL_SECRET=private-registry "${repo_root}/scripts/render-remote-release.sh" remote >"${render_dir}/private-registry.yaml"
python3 - "${render_dir}/private-registry.yaml" <<'PY'
import sys
import yaml
documents = list(yaml.safe_load_all(open(sys.argv[1])))
config = next(d for d in documents if d.get("kind") == "ConfigMap" and d["metadata"]["name"] == "inferscale-config")
assert config["data"]["INFERSCALE_IMAGE_PULL_SECRET"] == "private-registry"
for document in documents:
    if document.get("kind") not in {"Deployment", "StatefulSet", "Job"}:
        continue
    refs = document["spec"]["template"]["spec"].get("imagePullSecrets", [])
    if document["metadata"].get("namespace") == "inferscale-system":
        assert {"name": "private-registry"} in refs
    else:
        assert {"name": "private-registry"} not in refs
PY
echo "private registry Secret rendering passed"
