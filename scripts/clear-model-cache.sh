#!/usr/bin/env bash
set -euo pipefail

cache_root=""
confirmed=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --cache-root) cache_root="${2:-}"; shift 2 ;;
    --yes) confirmed=true; shift ;;
    *) echo "usage: $0 --cache-root /var/lib/inferscale/models --yes" >&2; exit 64 ;;
  esac
done
[[ "${confirmed}" == "true" ]] || { echo "--yes is required" >&2; exit 64; }
[[ -d "${cache_root}" ]] || { echo "cache root is not a directory: ${cache_root}" >&2; exit 66; }
resolved="$(realpath "${cache_root}")"
[[ "${resolved}" == /var/lib/inferscale/models || "${resolved}" == /tmp/inferscale-models-* ]] || {
  echo "refusing to clear unexpected path: ${resolved}" >&2
  exit 77
}

find "${resolved}" -mindepth 1 -maxdepth 1 -type d \( -name 'sha256-*' -o -name '.sha256-*.partial-*' \) -print -exec rm -rf -- {} +
echo "Cleared model entries from ${resolved}; lock files and .quarantine evidence were retained. Removed entries are not recoverable."
