#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
harness="${repo_root}/scripts/e2e/live-local-stack.sh"
makefile="${repo_root}/Makefile"
workflow="${repo_root}/.github/workflows/ci.yml"
testing_doc="${repo_root}/docs/testing.md"
versions_lock="${repo_root}/versions.lock.yaml"
gateway_routes="${repo_root}/deploy/base/gateway/routes.yaml"

test -x "${harness}"
bash -n "${harness}"

# The live gate must reuse the deployable stack and public/operator interfaces;
# it must not quietly become another in-memory controller test.
grep -Fq 'scripts/dev/up.sh' "${harness}"
grep -Fq '${public_base_url}/healthz' "${harness}"
grep -Fq '${internal_api_url}/readyz' "${harness}"
if grep -Fq '${public_base_url}/readyz' "${harness}"; then
  echo "live harness must not require public readiness disclosure" >&2
  exit 1
fi
grep -Fq '/healthz' "${gateway_routes}"
if grep -Fq '/readyz' "${gateway_routes}"; then
  echo "Gateway must not expose the dependency-aware readiness endpoint" >&2
  exit 1
fi
grep -Fq 'port-forward --address 127.0.0.1 service/postgres' "${harness}"
grep -Fq 'tenant create' "${harness}"
grep -Fq 'apikey issue' "${harness}"
grep -Fq '/v1/deployments"' "${harness}"
grep -Fq 'inferencedeployment "${deployment_name}" -o json' "${harness}"
grep -Fq 'status.get("phase") == "Ready"' "${harness}"
grep -Fq 'app.kubernetes.io/component=model-server' "${harness}"
grep -Fq '/chat/completions' "${harness}"
grep -Fq 'outage_status' "${harness}"
grep -Fq 'rollout restart deployment/inferscale-controller' "${harness}"

grep -Fq 'e2e-local-live:' "${makefile}"
grep -Fq './scripts/e2e/live-local-stack.sh' "${makefile}"
grep -Fq 'make e2e-local-live' "${workflow}"
grep -Fq 'dbaa79a76ace7f4ca230a1ff41dc7d8a5036a8ad0309e9c54f9bf3836dbe853e' "${workflow}"
grep -Fq 'version: v5.8.3' "${versions_lock}"
grep -Fq 'linuxAMD64SHA256: dbaa79a76ace7f4ca230a1ff41dc7d8a5036a8ad0309e9c54f9bf3836dbe853e' "${versions_lock}"
grep -Fq 'k3sImage: rancher/k3s:v1.36.2-k3s1' "${versions_lock}"

# Documentation must bound the evidence: this smoke is useful but is not GPU,
# performance, or full Gateway/llm-d conformance evidence.
grep -Fq 'make e2e-local-live' "${testing_doc}"
grep -Fq 'does not establish Gateway/llm-d' "${testing_doc}"
grep -Fq 'conformance, fractional mirroring' "${testing_doc}"
grep -Fq 'remains absent from the public Gateway routes' "${testing_doc}"

echo "live local-stack harness static contract passed"
