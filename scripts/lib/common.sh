#!/usr/bin/env bash

die() {
  echo "error: $*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

repository_root() {
  cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd
}

wait_for_deployment() {
  local namespace="$1"
  local name="$2"
  local timeout="${3:-180s}"
  kubectl -n "${namespace}" rollout status "deployment/${name}" --timeout="${timeout}"
}

cluster_exists() {
  k3d cluster list --no-headers 2>/dev/null | awk '{print $1}' | grep -Fxq "$1"
}
