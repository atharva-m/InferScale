#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
bash -n "${repo_root}/scripts/e2e/live-trace-conformance.sh"
test -x "${repo_root}/scripts/e2e/live-trace-conformance.sh"
test -f "${repo_root}/scripts/e2e/test_trace_conformance.py"
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s "${repo_root}/scripts/e2e" -p test_trace_conformance.py
