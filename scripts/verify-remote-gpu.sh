#!/usr/bin/env bash
set -euo pipefail

expected="${1:-}"
[[ "${expected}" =~ ^(1|2|4)$ ]] || { echo "usage: $0 <expected GPU count: 1|2|4>" >&2; exit 64; }
for command in nvidia-smi kubectl python3; do
  command -v "${command}" >/dev/null 2>&1 || { echo "required command not found: ${command}" >&2; exit 1; }
done

host_count="$(nvidia-smi --query-gpu=index --format=csv,noheader | wc -l | tr -d ' ')"
[[ "${host_count}" == "${expected}" ]] || { echo "host has ${host_count} GPUs; expected ${expected}" >&2; exit 1; }

python3 - "${expected}" <<'PY'
import json
import subprocess
import sys

expected = int(sys.argv[1])
nodes = json.loads(subprocess.check_output(["kubectl", "get", "nodes", "-o", "json"]))["items"]
counts = []
for node in nodes:
    value = node.get("status", {}).get("allocatable", {}).get("nvidia.com/gpu", "0")
    counts.append((node["metadata"]["name"], int(value)))
eligible = [name for name, count in counts if count >= expected]
if not eligible:
    raise SystemExit(f"no single Kubernetes node exposes all {expected} GPUs; observed {counts}")
print(f"single-node GPU invariant satisfied by {eligible[0]}: {counts}")
PY

kubectl get pods -n nvidia-device-plugin
kubectl get pods -n inferscale-monitoring -l app.kubernetes.io/name=dcgm-exporter
echo "Remote GPU host verification passed."
