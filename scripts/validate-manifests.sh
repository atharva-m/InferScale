#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
command -v kubectl >/dev/null 2>&1 || { echo "kubectl is required" >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 1; }

validation_dir="$(mktemp -d)"
trap 'rm -rf -- "${validation_dir}"' EXIT

while IFS= read -r -d '' overlay; do
  kubectl kustomize "${overlay}" >/dev/null
  echo "valid: ${overlay#"${repo_root}/"}"
done < <(find "${repo_root}/deploy/overlays" -mindepth 1 -maxdepth 1 -type d -print0 | sort -z)

kubectl kustomize "${repo_root}/deploy/overlays/local-wsl" >"${validation_dir}/local.yaml"
kubectl kustomize "${repo_root}/deploy/overlays/remote" >"${validation_dir}/remote.yaml"
grep -Fq 'local-development-only' "${validation_dir}/local.yaml"
if grep -Fq 'local-development-only' "${validation_dir}/remote.yaml"; then
  echo "remote overlay contains local development credentials" >&2
  exit 1
fi
if grep -Eq 'ghcr.io/inferscale/(benchmark|vllm|trtllm|modelcache):0\.1\.0-dev' "${validation_dir}/remote.yaml"; then
  echo "remote overlay contains a mutable development runtime image" >&2
  exit 1
fi
if grep -Eq 'ghcr.io/inferscale/(api|controller|admission|migrate):0\.1\.0-dev' "${validation_dir}/remote.yaml"; then
  echo "remote overlay contains a mutable development control-plane image" >&2
  exit 1
fi
for component in api controller admission migrate; do
  grep -Fq "image: registry.invalid/inferscale/${component}@sha256:0000000000000000000000000000000000000000000000000000000000000000" "${validation_dir}/remote.yaml" || {
    echo "remote overlay lacks the fail-closed ${component} release-image sentinel" >&2
    exit 1
  }
done
[[ -x "${repo_root}/scripts/render-remote-release.sh" ]] || {
  echo "remote release renderer is missing or not executable" >&2
  exit 1
}
for key in benchmark_runner_image runtime_vllm_image runtime_trtllm_image modelcache_image; do
  grep -Fq "key: ${key}" "${validation_dir}/remote.yaml" || {
    echo "remote workloads do not reference required external image key ${key}" >&2
    exit 1
  }
done

python3 - "${validation_dir}/local.yaml" "${validation_dir}/remote.yaml" <<'PY'
import pathlib
import re
import sys

import yaml


def load(path: str) -> list[dict]:
    return [value for value in yaml.safe_load_all(pathlib.Path(path).read_text()) if value]


local = load(sys.argv[1])
remote = load(sys.argv[2])

for name, documents in (("local", local), ("remote", remote)):
    identities: set[tuple[str, str, str, str]] = set()
    for document in documents:
        metadata = document.get("metadata", {})
        identity = (
            str(document.get("apiVersion", "")),
            str(document.get("kind", "")),
            str(metadata.get("namespace", "")),
            str(metadata.get("name", "")),
        )
        assert all((identity[0], identity[1], identity[3])), f"{name}: incomplete object identity {identity}"
        assert identity not in identities, f"{name}: duplicate rendered object {identity}"
        identities.add(identity)

        if document.get("kind") not in {"Deployment", "StatefulSet", "Job"}:
            continue
        pod = document["spec"]["template"]["spec"]
        assert pod.get("securityContext", {}).get("runAsNonRoot") is True, (
            f"{name}: {document['kind']}/{metadata['name']} is not runAsNonRoot"
        )
        for container in pod.get("initContainers", []) + pod.get("containers", []):
            image = container.get("image", "")
            assert image, f"{name}: {metadata['name']}/{container.get('name')} has no image"
            assert not re.search(r":(?:latest|main)$", image), f"{name}: mutable image {image}"
            security = container.get("securityContext", {})
            assert security.get("allowPrivilegeEscalation") is False, (
                f"{name}: {metadata['name']}/{container.get('name')} permits privilege escalation"
            )
            assert "ALL" in security.get("capabilities", {}).get("drop", []), (
                f"{name}: {metadata['name']}/{container.get('name')} does not drop all capabilities"
            )

    roles = [
        value
        for value in documents
        if value.get("kind") == "ClusterRole"
        and value.get("metadata", {}).get("name") == "inferscale-controller"
    ]
    assert len(roles) == 1, f"{name}: expected exactly one generated controller ClusterRole"
    forbidden = {
        "secrets",
        "namespaces",
        "persistentvolumeclaims",
        "statefulsets",
        "replicasets",
        "referencegrants",
        "inferencemodels",
        "podmonitors",
    }
    granted = {resource for rule in roles[0]["rules"] for resource in rule.get("resources", [])}
    assert not (granted & forbidden), f"{name}: controller has excess resources {sorted(granted & forbidden)}"

    cache_namespace = next(
        value for value in documents
        if value.get("kind") == "Namespace" and value["metadata"]["name"] == "inferscale-model-cache"
    )
    for mode, level in (("enforce", "privileged"), ("audit", "restricted"), ("warn", "restricted")):
        assert cache_namespace["metadata"]["labels"][f"pod-security.kubernetes.io/{mode}"] == level
    control_namespace = next(
        value for value in documents
        if value.get("kind") == "Namespace" and value["metadata"]["name"] == "inferscale-system"
    )
    assert control_namespace["metadata"]["labels"]["pod-security.kubernetes.io/enforce"] == "restricted"
    cache_policy = next(
        value for value in documents
        if value.get("kind") == "NetworkPolicy"
        and value["metadata"].get("namespace") == "inferscale-model-cache"
        and value["metadata"]["name"] == "default-deny"
    )["spec"]
    assert cache_policy["podSelector"] == {}
    assert set(cache_policy["policyTypes"]) == {"Ingress", "Egress"}
    assert not cache_policy.get("ingress") and not cache_policy.get("egress")

namespaces = {
    value["metadata"]["name"]: value["metadata"].get("labels", {})
    for value in remote
    if value.get("kind") == "Namespace"
}
assert namespaces["inferscale-gateway"]["inferscale.io/access-role"] == "gateway"
assert namespaces["inferscale-monitoring"]["inferscale.io/access-role"] == "monitoring"

local_secrets = [
    value
    for value in local
    if value.get("kind") == "Secret" and value.get("metadata", {}).get("name") == "inferscale-storage"
]
assert len(local_secrets) == 1, "local overlay must provide exactly one development storage Secret"
local_secret = local_secrets[0]
assert local_secret["metadata"]["annotations"]["inferscale.io/local-development-only"] == "true"
assert len(local_secret["stringData"]["benchmark_callback_signing_key"]) >= 32
assert local_secret["stringData"]["runtime_vllm_image"] == "ghcr.io/inferscale/fake-runtime:0.1.0-dev"

local_config = next(
    value
    for value in local
    if value.get("kind") == "ConfigMap"
    and value.get("metadata", {}).get("name") == "inferscale-config"
)
assert local_config["data"]["INFERSCALE_FAKE_RUNTIME"] == "true"

remote_config = next(
    value
    for value in remote
    if value.get("kind") == "ConfigMap"
    and value.get("metadata", {}).get("name") == "inferscale-config"
)
assert remote_config.get("data", {}).get("INFERSCALE_FAKE_RUNTIME") != "true"
assert remote_config["data"]["INFERSCALE_PUBLIC_BASE_URL"] == "http://release-render-required.invalid"
assert remote_config["data"]["INFERSCALE_BENCHMARK_PROVIDER"] == "set-at-run-time"
assert remote_config["data"]["INFERSCALE_BENCHMARK_INFERENCE_BASE_URL"] == "http://release-render-required.invalid"
assert remote_config["data"]["INFERSCALE_BENCHMARK_PROMETHEUS_URL"] == "http://release-render-required.invalid"
assert remote_config["data"]["INFERSCALE_BENCHMARK_DRIVER_VERSION"] == "set-at-run-time"
assert remote_config["data"]["INFERSCALE_BENCHMARK_CUDA_VERSION"] == "set-at-run-time"
assert all(
    "inferscale/fake-runtime" not in str(value)
    for value in remote
), "remote overlay must never reference the local fake runtime"

assert not any(
    value.get("kind") == "Secret" and value.get("metadata", {}).get("name") == "inferscale-storage"
    for value in remote
), "remote overlay must receive inferscale-storage from an external secret workflow"

default_denies = {
    (value.get("metadata", {}).get("namespace"), value.get("metadata", {}).get("name"))
    for value in remote
    if value.get("kind") == "NetworkPolicy"
    and value.get("spec", {}).get("podSelector") == {}
    and set(value.get("spec", {}).get("policyTypes", [])) == {"Ingress", "Egress"}
}
assert ("inferscale-system", "default-deny") in default_denies

print("rendered identity, RBAC, fake-runtime boundary, secret, network, image, and pod-security contracts validated")
PY
