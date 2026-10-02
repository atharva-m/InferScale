#!/usr/bin/env bash
set -euo pipefail

expected="${1:-}"
[[ "${expected}" =~ ^(1|2|4|8)$ && ( $# -eq 1 || ( $# -eq 3 && "${2:-}" == "--node" && -n "${3:-}" ) ) ]] || { echo "usage: $0 <expected physical host GPU count: 1|2|4|8> [--node NODE]" >&2; exit 64; }
node_name="${3:-${INFERSCALE_NODE_NAME:-$(hostname -s)}}"
gpu_sku="${INFERSCALE_GPU_SKU:-RTX_5090}"
for command in nvidia-smi kubectl python3; do
  command -v "${command}" >/dev/null 2>&1 || { echo "required command not found: ${command}" >&2; exit 1; }
done

host_count="$(nvidia-smi --query-gpu=index --format=csv,noheader | wc -l | tr -d ' ')"
[[ "${host_count}" == "${expected}" ]] || { echo "host has ${host_count} GPUs; expected ${expected}" >&2; exit 1; }

python3 - "${expected}" "${node_name}" "${gpu_sku}" <<'PY'
import csv
import io
import json
import subprocess
import sys

expected = int(sys.argv[1])
node_name, gpu_sku = sys.argv[2:]
physical = list(csv.reader(io.StringIO(subprocess.check_output([
    "nvidia-smi", "--query-gpu=name,memory.total", "--format=csv,noheader,nounits"
], text=True))))
if gpu_sku == "RTX_5090":
    if len(physical) != expected or any("RTX 5090" not in row[0] or float(row[1].strip()) < 30000 for row in physical):
        raise SystemExit(f"expected {expected} RTX 5090 devices with 32 GB each; observed {physical}")
nodes = json.loads(subprocess.check_output(["kubectl", "get", "nodes", "-o", "json"]))["items"]
matches = [node for node in nodes if node["metadata"]["name"] == node_name]
if len(matches) != 1:
    raise SystemExit(f"expected one node named {node_name}; use --node with the name chosen at bootstrap")
node = matches[0]
labels = node["metadata"].get("labels", {})
for key, value in {
    "inferscale.io/gpu-node": "true", "inferscale.io/gpu-sku": gpu_sku,
    "inferscale.io/gpu-count": str(expected),
}.items():
    if labels.get(key) != value:
        raise SystemExit(f"node {node_name} must have {key}={value}; observed {labels.get(key)!r}")
hostname = labels.get("kubernetes.io/hostname")
if not hostname:
    raise SystemExit("target node has no kubernetes.io/hostname label; cannot pin its node-local cache")
status = node.get("status", {})
for field in ("capacity", "allocatable"):
    actual = int(status.get(field, {}).get("nvidia.com/gpu", "0"))
    if actual != expected:
        raise SystemExit(f"node {node_name} {field} GPU count {actual} != physical count {expected}; check device health and disable GPU sharing")
if labels.get("nvidia.com/gpu.sharing-strategy", "none") != "none":
    raise SystemExit("GPU sharing/time slicing is outside the exclusive GPU contract")
if node.get("spec", {}).get("unschedulable") or not any(c.get("type") == "Ready" and c.get("status") == "True" for c in status.get("conditions", [])):
    raise SystemExit(f"node {node_name} must be Ready and schedulable")
print(f"Verified {expected} exclusive GPUs on node {node_name}.")
print(f"Set the runtime inventory selector to kubernetes.io/hostname={hostname} and inferscale.io/gpu-sku={gpu_sku}.")
print("Tensor parallelism remains 1, 2, or 4 GPUs per replica; 8 GPUs is host capacity, not TP=8.")
PY

kubectl get pods -n nvidia-device-plugin
kubectl get pods -n inferscale-gpu-system -l app.kubernetes.io/name=dcgm-exporter
echo "Remote GPU host verification passed."
