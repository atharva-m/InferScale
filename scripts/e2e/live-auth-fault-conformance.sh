#!/usr/bin/env bash
# Owned credential and temporary Valkey outage on an explicit local cluster.
set -euo pipefail
script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
exec python3 "${script_dir}/auth_fault_conformance.py"
