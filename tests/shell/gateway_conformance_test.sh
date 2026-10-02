#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bash -n "${repo_root}/scripts/e2e/live-gateway-conformance.sh"
test -x "${repo_root}/scripts/e2e/live-gateway-conformance.sh"
python3 -m unittest discover -s "${repo_root}/scripts/e2e" -p test_gateway_conformance.py
