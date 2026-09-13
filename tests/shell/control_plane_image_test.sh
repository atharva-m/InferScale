#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
dockerfile="${repo_root}/build/Dockerfile"
build_script="${repo_root}/scripts/build-release-images.sh"

grep -Eq '^ARG GO_IMAGE=golang:[^@]+@sha256:[0-9a-f]{64}$' "${dockerfile}"
grep -Eq '^ARG RUNTIME_IMAGE=[^@]+@sha256:[0-9a-f]{64}$' "${dockerfile}"
grep -Fq 'ARG COMMAND' "${dockerfile}"
grep -Fq 'CGO_ENABLED=0 go build -trimpath' "${dockerfile}"
grep -Fq 'USER nonroot:nonroot' "${dockerfile}"
grep -Fq 'ENTRYPOINT ["/usr/local/bin/inferscale"]' "${dockerfile}"

for command in api controller admission migrate; do
  test -f "${repo_root}/cmd/${command}/main.go"
done
grep -Fq 'for service in api controller admission migrate; do' "${build_script}"

python3 - "${repo_root}" <<'PY'
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
lock = (root / "versions.lock.yaml").read_text(encoding="utf-8")
dockerfile = (root / "build" / "Dockerfile").read_text(encoding="utf-8")

go_digest = re.search(r"buildImageDigest: (sha256:[0-9a-f]{64})", lock).group(1)
runtime_digest = re.search(r"runtimeImageDigest: (sha256:[0-9a-f]{64})", lock).group(1)
assert f"@{go_digest}" in dockerfile
assert f"@{runtime_digest}" in dockerfile
PY

echo "control-plane image contracts passed"
