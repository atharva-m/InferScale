#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <tls.crt> <tls.key>" >&2
  exit 64
fi
cert="$1"
key="$2"
[[ -r "${cert}" && -r "${key}" ]] || { echo "certificate and key must be readable" >&2; exit 66; }
kubectl create namespace inferscale-gateway --dry-run=client -o yaml | kubectl apply -f -
kubectl -n inferscale-gateway create secret tls inferscale-tls \
  --cert="${cert}" --key="${key}" --dry-run=client -o yaml | kubectl apply -f -
