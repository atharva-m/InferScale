#!/usr/bin/env bash
# CPU-only native Gateway experiments on an explicitly selected local cluster.
set -euo pipefail
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec python3 "${script_dir}/gateway_conformance.py"
