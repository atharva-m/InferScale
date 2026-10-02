#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bash -n "${repo_root}/scripts/e2e/live-auth-fault-conformance.sh"
test -x "${repo_root}/scripts/e2e/live-auth-fault-conformance.sh"
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s "${repo_root}/scripts/e2e" -p test_auth_fault_conformance.py
