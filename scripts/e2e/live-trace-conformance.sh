#!/usr/bin/env bash
# One real inference request against existing loopback Gateway/Tempo forwards.
set -euo pipefail
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec python3 "${script_dir}/trace_conformance.py"
