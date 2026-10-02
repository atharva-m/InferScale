#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
grep -Fx -- '-- +goose Up' "${repo_root}/internal/storage/postgres/migrations/000001_platform.up.sql" >/dev/null
grep -Fx -- '//go:embed *.up.sql' "${repo_root}/internal/storage/postgres/migrations/embed.go" >/dev/null
rendered="$(mktemp)"
trap 'rm -f -- "$rendered"' EXIT
kubectl kustomize "${repo_root}/deploy/overlays/local-wsl" >"${rendered}"

python3 - "${rendered}" <<'PY'
import os
import pathlib
import subprocess
import sys
import tempfile

import yaml

documents = [item for item in yaml.safe_load_all(pathlib.Path(sys.argv[1]).read_text()) if item]
job = next(
    item
    for item in documents
    if item.get("kind") == "Job"
    and item.get("metadata", {}).get("name") == "inferscale-migrate-000006"
)
container = job["spec"]["template"]["spec"]["containers"][0]
assert container["image"] == "ghcr.io/inferscale/migrate:0.1.0-dev"
assert container["args"] == ["up"]
assert container["env"][0]["valueFrom"]["secretKeyRef"] == {
    "name": "inferscale-storage",
    "key": "database_url",
}
database_gate, = job["spec"]["template"]["spec"]["initContainers"]
assert database_gate["name"] == "wait-for-database"
postgres = next(item for item in documents if item.get("kind") == "StatefulSet" and item["metadata"]["name"] == "postgres")
assert database_gate["image"] == postgres["spec"]["template"]["spec"]["containers"][0]["image"]
assert "@sha256:" in database_gate["image"]
assert database_gate["env"] == container["env"]
assert database_gate["resources"] == {
    "requests": {"cpu": "10m", "memory": "16Mi"},
    "limits": {"cpu": "100m", "memory": "64Mi"},
}
assert database_gate["securityContext"] == container["securityContext"]

# Exercise the rendered init command: transient pod-network failures must
# preserve this pod until the database accepts connections, while persistent
# failures must stop after a bounded number of attempts without exposing URLs.
with tempfile.TemporaryDirectory() as directory:
    temporary = pathlib.Path(directory)
    probe = temporary / "pg_isready"
    probe.write_text('''#!/usr/bin/env python3
import os
import pathlib
import sys
assert sys.argv[1:] == ["-d", os.environ["INFERSCALE_DATABASE_URL"], "-t", "2"]
calls = pathlib.Path(os.environ["PROBE_CALLS"])
count = int(calls.read_text()) + 1 if calls.exists() else 1
calls.write_text(str(count))
print(os.environ["INFERSCALE_DATABASE_URL"], file=sys.stderr)
sys.exit(0 if count >= int(os.environ["PROBE_READY_AFTER"]) else 2)
''')
    sleep = temporary / "sleep"
    sleep.write_text('''#!/bin/sh
test "$1" = 2 || exit 99
printf 'sleep\\n' >> "$PROBE_SLEEPS"
''')
    probe.chmod(0o755)
    sleep.chmod(0o755)
    for ready_after, expected_exit, expected_calls in [(1, 0, 1), (3, 0, 3), (61, 1, 60)]:
        calls = temporary / f"calls-{ready_after}"
        sleeps = temporary / f"sleeps-{ready_after}"
        env = {
            **os.environ,
            "PATH": f"{temporary}:{os.environ['PATH']}",
            "INFERSCALE_DATABASE_URL": "postgres://user:secret-test-value@postgres/inferscale",
            "PROBE_CALLS": str(calls),
            "PROBE_SLEEPS": str(sleeps),
            "PROBE_READY_AFTER": str(ready_after),
        }
        result = subprocess.run(
            database_gate["command"] + database_gate["args"], env=env,
            capture_output=True, text=True, timeout=10,
        )
        assert result.returncode == expected_exit, result.stderr
        assert int(calls.read_text()) == expected_calls
        assert len(sleeps.read_text().splitlines() if sleeps.exists() else []) == expected_calls - 1
        assert "secret-test-value" not in result.stdout + result.stderr
        assert ("did not become ready after 60 attempts" in result.stderr) == (expected_exit != 0)

expected = {"inferscale-api", "inferscale-controller", "inferscale-admission"}
gated = set()
for item in documents:
    if item.get("kind") != "Deployment" or item.get("metadata", {}).get("name") not in expected:
        continue
    init_containers = item["spec"]["template"]["spec"].get("initContainers", [])
    gate = next(value for value in init_containers if value["name"] == "wait-for-schema-000006")
    assert "to_regclass('public.sync_outbox')" in gate["args"][0]
    assert "column_name='lease_owner'" in gate["args"][0]
    assert "column_name='execution_contract'" in gate["args"][0]
    assert "column_name='revision'" in gate["args"][0]
    assert "table_name='operations' AND column_name='request_digest'" in gate["args"][0]
    assert "deployments_tenant_name_idx" in gate["args"][0]
    assert "@sha256:" in gate["image"]
    gated.add(item["metadata"]["name"])
assert gated == expected
print("migration job and all control-plane schema gates validated")
PY

grep -Fq 'wait --for=condition=complete job/inferscale-migrate-000006' "${repo_root}/scripts/dev/up.sh"
