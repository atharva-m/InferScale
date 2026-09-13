#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
dockerfile="${repo_root}/benchmarks/runner/Dockerfile"

grep -Eq '^ARG PYTHON_IMAGE=python:[^@]+@sha256:[0-9a-f]{64}$' "${dockerfile}"
grep -Fq 'COPY benchmarks/workloads /workloads' "${dockerfile}"
grep -Fq "'guidellm[recommended]==0.7.0'" "${dockerfile}"
grep -Fq 'USER 65532:65532' "${dockerfile}"
grep -Fq 'ENTRYPOINT ["python", "-m", "inferscale_bench"]' "${dockerfile}"
grep -Fq 'INFERSCALE_RUNNER_GIT_COMMIT=${INFERSCALE_GIT_COMMIT}' "${dockerfile}"

grep -Fq 'BENCHMARK_IMAGE:?BENCHMARK_IMAGE is required' "${repo_root}/scripts/build-release-images.sh"
grep -Fq 'benchmarks/runner/Dockerfile' "${repo_root}/scripts/build-release-images.sh"
grep -Fq 'benchmarks/runner/Dockerfile' "${repo_root}/scripts/dev/up.sh"

echo "benchmark runner image contract tests passed"
