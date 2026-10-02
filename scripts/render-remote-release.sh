#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
overlay_name="${1:-}"
case "${overlay_name}" in
  remote|vast-1x5090|vast-2x5090|vast-4x5090|vast-8x5090) ;;
  *)
    echo "usage: $0 <remote|vast-1x5090|vast-2x5090|vast-4x5090|vast-8x5090>" >&2
    exit 64
    ;;
esac

command -v kubectl >/dev/null 2>&1 || { echo "kubectl is required" >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { echo "python3 is required" >&2; exit 1; }

# An opt-in candidate bundle binds the remote EPP to the same reviewed image
# inventory as the Gateway controllers. Verification never changes release gates.
export INFERSCALE_CANDIDATE_EPP_IMAGE=""
if [[ -n "${INFERSCALE_CANDIDATE_DEPENDENCY_BUNDLE:-}" ]]; then
  INFERSCALE_CANDIDATE_EPP_IMAGE="$(python3 "${repo_root}/scripts/render-candidate-dependencies.py" \
    verify "${INFERSCALE_CANDIDATE_DEPENDENCY_BUNDLE}" --print-epp-image)"
fi

# These inputs only render a manifest. They neither apply it nor waive the
# independent live Gateway conformance / production release gate.
export REMOTE_EXTERNAL_STORAGE="${REMOTE_EXTERNAL_STORAGE:-false}"
export CLUSTER_POSTGRES_CIDRS="${CLUSTER_POSTGRES_CIDRS:-}"
export CLUSTER_VALKEY_CIDRS="${CLUSTER_VALKEY_CIDRS:-}"
export CLUSTER_KUBERNETES_API_CIDRS="${CLUSTER_KUBERNETES_API_CIDRS:-}"
export CLUSTER_POSTGRES_PORT="${CLUSTER_POSTGRES_PORT:-5432}"
export CLUSTER_VALKEY_PORT="${CLUSTER_VALKEY_PORT:-6379}"
export CLUSTER_KUBERNETES_API_PORT="${CLUSTER_KUBERNETES_API_PORT:-6443}"
export INFERSCALE_GPU_NODE_NAME="${INFERSCALE_GPU_NODE_NAME:-}"
export INFERSCALE_IMAGE_PULL_SECRET="${INFERSCALE_IMAGE_PULL_SECRET:-}"
export INFERSCALE_RENDER_OVERLAY="${overlay_name}"

for variable in API_IMAGE CONTROLLER_IMAGE ADMISSION_IMAGE MIGRATE_IMAGE; do
  value="${!variable:-}"
  if [[ ! "${value}" =~ ^[^[:space:]@]+@sha256:[0-9a-f]{64}$ ]]; then
    echo "${variable} must be a registry image pinned with @sha256:<64 lowercase hex>" >&2
    exit 64
  fi
done
if ! python3 - \
  "${PUBLIC_BASE_URL:-}" \
  "${INFERSCALE_BENCHMARK_PROVIDER:-}" \
  "${INFERSCALE_BENCHMARK_INFERENCE_BASE_URL:-}" \
  "${INFERSCALE_BENCHMARK_PROMETHEUS_URL:-}" \
  "${INFERSCALE_BENCHMARK_DRIVER_VERSION:-}" \
  "${INFERSCALE_BENCHMARK_CUDA_VERSION:-}" <<'PY'
import re
import ipaddress
import json
import os
import sys
import urllib.parse


def valid_origin(raw: str, schemes: set[str]) -> bool:
    try:
        value = urllib.parse.urlsplit(raw)
        valid_port = value.port is None or 1 <= value.port <= 65535
    except ValueError:
        return False
    return (
        raw == raw.strip()
        and not any(character.isspace() or ord(character) < 32 for character in raw)
        and "?" not in raw
        and "#" not in raw
        and "\\" not in value.netloc
        and "%" not in value.netloc
        and value.scheme in schemes
        and value.hostname is not None
        and value.username is None
        and value.password is None
        and not value.netloc.endswith(":")
        and value.path in {"", "/"}
        and not value.query
        and not value.fragment
        and valid_port
    )


public_origin, provider, inference_origin, prometheus_origin, driver_version, cuda_version = sys.argv[1:]
if not valid_origin(public_origin, {"https"}):
    print("PUBLIC_BASE_URL must be an HTTPS origin without a path, credentials, query, or fragment", file=sys.stderr)
    raise SystemExit(1)
public_hostname = urllib.parse.urlsplit(public_origin).hostname
dns_name = r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*"
if not re.fullmatch(dns_name, public_hostname) or len(public_hostname) > 253:
    raise SystemExit("PUBLIC_BASE_URL must use a valid DNS hostname for the Gateway TLS listener")
try:
    ipaddress.ip_address(public_hostname)
except ValueError:
    pass
else:
    raise SystemExit("PUBLIC_BASE_URL must use a DNS hostname, not an IP address")
node_name = os.environ["INFERSCALE_GPU_NODE_NAME"]
pull_secret = os.environ["INFERSCALE_IMAGE_PULL_SECRET"]
if pull_secret and (not re.fullmatch(dns_name, pull_secret) or len(pull_secret) > 253):
    raise SystemExit("INFERSCALE_IMAGE_PULL_SECRET must be a Kubernetes Secret name")
if os.environ["INFERSCALE_RENDER_OVERLAY"].startswith("vast-") or node_name:
    if not re.fullmatch(dns_name, node_name) or len(node_name) > 253:
        raise SystemExit("INFERSCALE_GPU_NODE_NAME must name the selected Kubernetes GPU node for a Vast overlay")
if not re.fullmatch(r"[a-z0-9][a-z0-9._-]{0,63}", provider) or provider == "set-at-run-time":
    print("INFERSCALE_BENCHMARK_PROVIDER must be a lowercase provider identifier", file=sys.stderr)
    raise SystemExit(1)
if not valid_origin(inference_origin, {"https"}):
    print("INFERSCALE_BENCHMARK_INFERENCE_BASE_URL must be an HTTPS origin without credentials, query, or fragment", file=sys.stderr)
    raise SystemExit(1)
if not valid_origin(prometheus_origin, {"http", "https"}):
    print("INFERSCALE_BENCHMARK_PROMETHEUS_URL must be an HTTP(S) origin without credentials, query, or fragment", file=sys.stderr)
    raise SystemExit(1)
if not re.fullmatch(r"[0-9]{3,4}\.[0-9]{1,3}(?:\.[0-9]{1,3})?", driver_version):
    print("INFERSCALE_BENCHMARK_DRIVER_VERSION must be an explicit NVIDIA driver version", file=sys.stderr)
    raise SystemExit(1)
if not re.fullmatch(r"[0-9]{1,2}\.[0-9]{1,2}(?:\.[0-9]+)?", cuda_version):
    print("INFERSCALE_BENCHMARK_CUDA_VERSION must be an explicit CUDA version", file=sys.stderr)
    raise SystemExit(1)

external_storage = os.environ["REMOTE_EXTERNAL_STORAGE"]
if external_storage not in {"true", "false"}:
    raise SystemExit("REMOTE_EXTERNAL_STORAGE must be true or false")
for service in ("POSTGRES", "VALKEY", "KUBERNETES_API"):
    cidr_name = f"CLUSTER_{service}_CIDRS"
    raw = os.environ[cidr_name]
    if external_storage == "true" and service != "KUBERNETES_API" and not raw:
        raise SystemExit(f"{cidr_name} is required with REMOTE_EXTERNAL_STORAGE=true")
    try:
        cidrs = json.loads(raw) if raw else []
        if not isinstance(cidrs, list) or len(cidrs) > 16 or (raw and not cidrs):
            raise ValueError("expected a nonempty list of at most 16 CIDRs")
        for cidr in cidrs:
            if not isinstance(cidr, str):
                raise ValueError("CIDRs must be strings")
            network = ipaddress.ip_network(cidr, strict=True)
            if service == "KUBERNETES_API" and network.prefixlen != network.max_prefixlen:
                raise ValueError("Kubernetes API endpoints must use exact /32 or /128 addresses")
            private_ranges = ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7")
            if not any(
                network.version == private.version and network.subnet_of(private)
                for private in map(ipaddress.ip_network, private_ranges)
            ):
                raise ValueError("use canonical private endpoint/subnet CIDRs")
    except (ValueError, TypeError) as exc:
        raise SystemExit(f"{cidr_name} must be a JSON array of private CIDRs: {exc}") from exc
    port_name = f"CLUSTER_{service}_PORT"
    port = os.environ[port_name]
    if not re.fullmatch(r"[1-9][0-9]{0,4}", port) or int(port) > 65535:
        raise SystemExit(f"{port_name} must be a TCP port between 1 and 65535")
PY
then
  exit 64
fi

render_dir="$(mktemp -d)"
trap 'rm -rf -- "${render_dir}"' EXIT
kubectl kustomize "${repo_root}/deploy/overlays/${overlay_name}" >"${render_dir}/template.yaml"

API_IMAGE="${API_IMAGE}" \
CONTROLLER_IMAGE="${CONTROLLER_IMAGE}" \
ADMISSION_IMAGE="${ADMISSION_IMAGE}" \
MIGRATE_IMAGE="${MIGRATE_IMAGE}" \
PUBLIC_BASE_URL="${PUBLIC_BASE_URL}" \
INFERSCALE_BENCHMARK_PROVIDER="${INFERSCALE_BENCHMARK_PROVIDER}" \
INFERSCALE_BENCHMARK_INFERENCE_BASE_URL="${INFERSCALE_BENCHMARK_INFERENCE_BASE_URL}" \
INFERSCALE_BENCHMARK_PROMETHEUS_URL="${INFERSCALE_BENCHMARK_PROMETHEUS_URL}" \
INFERSCALE_BENCHMARK_DRIVER_VERSION="${INFERSCALE_BENCHMARK_DRIVER_VERSION}" \
INFERSCALE_BENCHMARK_CUDA_VERSION="${INFERSCALE_BENCHMARK_CUDA_VERSION}" \
python3 - "${render_dir}/template.yaml" <<'PY'
import os
import json
import pathlib
import re
import sys
import urllib.parse

import yaml


source = pathlib.Path(sys.argv[1])
documents = [document for document in yaml.safe_load_all(source.read_text()) if document]
replacements = {
    "api": (["ghcr.io/inferscale/api", "registry.invalid/inferscale/api"], os.environ["API_IMAGE"]),
    "controller": (
        ["ghcr.io/inferscale/controller", "registry.invalid/inferscale/controller"],
        os.environ["CONTROLLER_IMAGE"],
    ),
    "admission": (
        ["ghcr.io/inferscale/admission", "registry.invalid/inferscale/admission"],
        os.environ["ADMISSION_IMAGE"],
    ),
    "migrate": (
        ["ghcr.io/inferscale/migrate", "registry.invalid/inferscale/migrate"],
        os.environ["MIGRATE_IMAGE"],
    ),
}
seen = {name: 0 for name in replacements}
configured_public_origin = 0
benchmark_authority = {
    name: os.environ[name]
    for name in (
        "INFERSCALE_BENCHMARK_PROVIDER",
        "INFERSCALE_BENCHMARK_INFERENCE_BASE_URL",
        "INFERSCALE_BENCHMARK_PROMETHEUS_URL",
        "INFERSCALE_BENCHMARK_DRIVER_VERSION",
        "INFERSCALE_BENCHMARK_CUDA_VERSION",
    )
}

for document in documents:
    if document.get("kind") == "ConfigMap" and document.get("metadata", {}).get("name") == "inferscale-config":
        document.setdefault("data", {})["INFERSCALE_PUBLIC_BASE_URL"] = os.environ["PUBLIC_BASE_URL"]
        document["data"].update(benchmark_authority)
        if os.environ["INFERSCALE_CANDIDATE_EPP_IMAGE"]:
            document["data"]["INFERSCALE_EPP_IMAGE"] = os.environ["INFERSCALE_CANDIDATE_EPP_IMAGE"]
            document.setdefault("metadata", {}).setdefault("annotations", {})["inferscale.io/dependency-qualification"] = "pending"
        if os.environ["INFERSCALE_IMAGE_PULL_SECRET"]:
            document["data"]["INFERSCALE_IMAGE_PULL_SECRET"] = os.environ["INFERSCALE_IMAGE_PULL_SECRET"]
        if os.environ["INFERSCALE_GPU_NODE_NAME"]:
            selectors = json.loads(document["data"].get("INFERSCALE_GPU_NODE_SELECTORS", "{}"))
            selectors.setdefault("RTX_5090", {"inferscale.io/gpu-sku": "RTX_5090"})[
                "kubernetes.io/hostname"
            ] = os.environ["INFERSCALE_GPU_NODE_NAME"]
            document["data"]["INFERSCALE_GPU_NODE_SELECTORS"] = json.dumps(selectors, sort_keys=True)
        if os.environ["CLUSTER_KUBERNETES_API_CIDRS"]:
            document["data"]["INFERSCALE_KUBERNETES_API_CIDRS"] = os.environ["CLUSTER_KUBERNETES_API_CIDRS"]
            document["data"]["INFERSCALE_KUBERNETES_API_PORT"] = os.environ["CLUSTER_KUBERNETES_API_PORT"]
        configured_public_origin += 1
    if document.get("kind") == "Gateway" and document.get("metadata", {}).get("name") == "inferscale":
        for listener in document["spec"]["listeners"]:
            listener["hostname"] = urllib.parse.urlsplit(os.environ["PUBLIC_BASE_URL"]).hostname
    if document.get("kind") not in {"Deployment", "StatefulSet", "Job"}:
        continue
    pod = document.get("spec", {}).get("template", {}).get("spec", {})
    if document.get("metadata", {}).get("namespace") == "inferscale-system" and os.environ["INFERSCALE_IMAGE_PULL_SECRET"]:
        refs = pod.setdefault("imagePullSecrets", [])
        ref = {"name": os.environ["INFERSCALE_IMAGE_PULL_SECRET"]}
        if ref not in refs:
            refs.append(ref)
    for container in pod.get("initContainers", []) + pod.get("containers", []):
        image = str(container.get("image", ""))
        for logical_name, (bases, replacement) in replacements.items():
            if any(image == base or image.startswith(base + ":") or image.startswith(base + "@") for base in bases):
                container["image"] = replacement
                seen[logical_name] += 1
                break

missing = sorted(name for name, count in seen.items() if count == 0)
if missing:
    raise SystemExit(f"release render did not find expected workload images: {', '.join(missing)}")
if configured_public_origin != 1:
    raise SystemExit(f"release render found {configured_public_origin} inferscale-config ConfigMaps, want exactly one")

if os.environ["REMOTE_EXTERNAL_STORAGE"] == "true":
    development_storage = {
        ("StatefulSet", "postgres"), ("Deployment", "valkey"),
        ("Service", "postgres"), ("Service", "valkey"),
        ("NetworkPolicy", "allow-control-plane-to-storage"),
    }
    removed = set()
    retained = []
    for document in documents:
        metadata = document.get("metadata", {})
        identity = (document.get("kind"), metadata.get("name"))
        if metadata.get("namespace") == "inferscale-system" and identity in development_storage:
            if identity in removed:
                raise SystemExit(f"duplicate development storage object {identity}")
            removed.add(identity)
        else:
            retained.append(document)
    if removed != development_storage:
        raise SystemExit(f"external storage render could not identify bundled objects: {development_storage - removed}")
    documents = retained

for service, workload_names in (
    ("POSTGRES", ["inferscale-api", "inferscale-controller", "inferscale-admission", "inferscale-migrate"]),
    ("VALKEY", ["inferscale-api", "inferscale-admission"]),
    ("KUBERNETES_API", ["inferscale-api", "inferscale-controller"]),
):
    raw = os.environ[f"CLUSTER_{service}_CIDRS"]
    if not raw:
        continue
    documents.append({
        "apiVersion": "networking.k8s.io/v1",
        "kind": "NetworkPolicy",
        "metadata": {
            "name": f"allow-configured-{service.lower().replace('_', '-')}",
            "namespace": "inferscale-system",
        },
        "spec": {
            "podSelector": {"matchExpressions": [{
                "key": "app.kubernetes.io/name", "operator": "In", "values": workload_names,
            }]},
            "policyTypes": ["Egress"],
            "egress": [{
                "to": [{"ipBlock": {"cidr": cidr}} for cidr in json.loads(raw)],
                "ports": [{"protocol": "TCP", "port": int(os.environ[f"CLUSTER_{service}_PORT"])}],
            }],
        },
    })

for document in documents:
    if document.get("kind") not in {"Deployment", "StatefulSet", "Job"}:
        continue
    pod = document.get("spec", {}).get("template", {}).get("spec", {})
    for container in pod.get("initContainers", []) + pod.get("containers", []):
        image = str(container.get("image", ""))
        if image.startswith("ghcr.io/inferscale/") and re.search(r":0\.1\.0-dev(?:$|@)", image):
            raise SystemExit(f"remote release still contains development image {image}")

yaml.safe_dump_all(documents, sys.stdout, sort_keys=False, explicit_start=True)
PY
