#!/usr/bin/env bash
# Real, non-publishable GPU smoke against an explicitly selected local cluster.
# Provision InferScale and a tenant first. Credentials are read from a private
# JSON file: {"base_url":"http://127.0.0.1:8080","api_key":"..."}.
set -euo pipefail
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec python3 "${script_dir}/local_gpu_smoke.py"
