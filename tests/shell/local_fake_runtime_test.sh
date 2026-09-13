#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
command -v kubectl >/dev/null 2>&1 || { echo "kubectl is required" >&2; exit 1; }

render_dir="$(mktemp -d)"
trap 'rm -rf -- "${render_dir}"' EXIT
local_render="${render_dir}/local.yaml"
remote_render="${render_dir}/remote.yaml"
kubectl kustomize "${repo_root}/deploy/overlays/local-wsl" >"${local_render}"
kubectl kustomize "${repo_root}/deploy/overlays/remote" >"${remote_render}"

grep -Fq 'INFERSCALE_FAKE_RUNTIME: "true"' "${local_render}"
grep -Fq 'runtime_vllm_image: ghcr.io/inferscale/fake-runtime:0.1.0-dev' "${local_render}"
if grep -Fq 'INFERSCALE_FAKE_RUNTIME: "true"' "${remote_render}" || grep -Fq 'inferscale/fake-runtime' "${remote_render}"; then
  echo "remote deployment enables the local fake runtime" >&2
  exit 1
fi

# Feature gates live in one shared ConfigMap, while runtime images and inventory
# are controller-only inputs. API/admission must therefore be able to start with
# the fake/backend-auto flags present without receiving runtime image Secrets.
python3 - "${local_render}" <<'PY'
import pathlib
import sys

import yaml

documents = [item for item in yaml.safe_load_all(pathlib.Path(sys.argv[1]).read_text()) if item]
config = next(
    item
    for item in documents
    if item.get("kind") == "ConfigMap" and item.get("metadata", {}).get("name") == "inferscale-config"
)
assert config["data"]["INFERSCALE_FAKE_RUNTIME"] == "true"
assert "INFERSCALE_FEATURE_BACKEND_AUTO" in config["data"]

deployments = {
    item["metadata"]["name"]: item
    for item in documents
    if item.get("kind") == "Deployment" and item.get("metadata", {}).get("name", "").startswith("inferscale-")
}
runtime_inputs = {"INFERSCALE_VLLM_IMAGE", "INFERSCALE_TRTLLM_IMAGE", "INFERSCALE_PREFETCH_IMAGE"}
for name in ("inferscale-api", "inferscale-controller", "inferscale-admission"):
    container = deployments[name]["spec"]["template"]["spec"]["containers"][0]
    assert {source["configMapRef"]["name"] for source in container.get("envFrom", [])} == {"inferscale-config"}
    explicit = {entry["name"] for entry in container.get("env", [])}
    if name == "inferscale-controller":
        assert runtime_inputs <= explicit
    else:
        assert runtime_inputs.isdisjoint(explicit), (name, explicit & runtime_inputs)
PY

up_script="${repo_root}/scripts/dev/up.sh"
grep -Fq 'COMMAND=fakeruntime' "${up_script}"
grep -Fq 'k3d image import -c "${cluster_name}" "${fake_runtime_image}"' "${up_script}"
grep -Fq 'INFERSCALE_FAKE_RUNTIME=true' "${repo_root}/deploy/overlays/local-wsl/kustomization.yaml"

# Development-only code must not become a release backend or release image.
if grep -Fqi 'fake' "${repo_root}/api/openapi/openapi.yaml" "${repo_root}/api/crds/platform.inferscale.io_inferencedeployments.yaml"; then
  echo "development fake runtime leaked into the public API or CRD" >&2
  exit 1
fi
if grep -Eq 'COMMAND=(fakeruntime|fake-runtime)|for service in .*fakeruntime' "${repo_root}/scripts/build-release-images.sh"; then
  echo "development fake runtime leaked into the release image build" >&2
  exit 1
fi

echo "local fake-runtime image, overlay, and public-boundary contracts passed"
