# Local authorization fault evidence

`auth_fault_conformance.py` covers the two authorization gaps intentionally
excluded by `gateway_conformance.py`: a real revoked credential and a Valkey
outage. It creates its own short-lived, inference-only credential through the
prebuilt `inferscalectl`, plus isolated CPU workers, revision EPPs and a more
specific HTTPRoute. It never revokes the credential supplied for management
access.

The experiment enables the native EPP stream metrics only on its own EPPs.
An accepted control request must increment the stream completion counter by
exactly one. Three requests with a valid-format wrong secret and three requests
using the issued credential after revocation must each return `401`, without
changing either EPP stream counters or worker request counters. Both EPPs must
remain idle and retain their process start time. Revocation is checked 20
seconds after the operator confirms revocation; the authentication cache TTL
is 15 seconds.

The outage portion temporarily scales `inferscale-system/valkey` to zero,
waits for all its Pods to disappear, requires three new requests to return
`503` before EPP, restores the original replica count, and verifies inference
recovers. It arms restoration before submitting the mutation and runs it in
`finally`, including on interruption. UID and replica preconditions prevent
overwriting a replacement Deployment or a concurrent scaling decision. The
test rejects a Valkey HPA. Use a disposable local cluster with no concurrent
Valkey changes or other fault experiments.

The script requires an explicit `k3d-inferscale-*` context whose API server uses
a loopback origin without a proxy, and a loopback Gateway forward. It creates
its own ephemeral loopback forwards to local PostgreSQL and its EPPs. The
PostgreSQL connection is constructed in memory from `inferscale-storage`; its
checked service host is replaced with the private forward. No credential is
passed on a command line or written to evidence. The auth file must be a
regular private file with mode `0600` or stricter.

After the candidate dependencies and ordinary CPU harness are working:

```bash
export INFERSCALE_CONFORMANCE_KUBE_CONTEXT=k3d-inferscale-dev
export INFERSCALE_CONFORMANCE_AUTH_FILE=benchmarks/artifacts/local-gpu/auth.json
export INFERSCALE_AUTH_FAULT_CTL=bin/v1-validation/inferscalectl
export KUBECTL=bin/inferscale-tools/kubectl
# The auth file supplies the existing deployment UUID and loopback base_url.
# Set INFERSCALE_CONFORMANCE_FAKE_IMAGE to the tested fake image if needed.
scripts/e2e/live-auth-fault-conformance.sh
```

The retained summary is under
`benchmarks/results/auth-faults/<run-id>/summary.json`; the run directory is
`0700` and the file is `0600`. Override the parent directory with
`INFERSCALE_AUTH_FAULT_RESULTS_DIR`. Temporary logs remain in a private
temporary directory and are removed. Only counters, status codes, image
identities and bounded experiment metadata are retained. Raw HTTP responses,
API keys, database URLs, command output and error messages are excluded.

Cleanup always attempts dependency restoration, owned-key revocation and
fixture deletion independently. A cleanup failure fails the experiment and
records only the failed cleanup category. If issuance returns an ambiguous
failure before revealing a key ID, the experiment reports unconfirmed key
cleanup instead of success; every issued test key also expires after 15
minutes. Consult the operator's durable key records by the unique
`auth-fault-<run-id>` name in that exceptional case.

This is partial CPU evidence with `release_signoff:false`. It does not prove
admission-process or PostgreSQL outage behavior, continuity of an already
accepted stream during an outage, GPU performance, or exact release image
qualification. The production lock and release evidence remain separate.

Offline verification, with no Kubernetes access or operator execution:

```bash
python3 -m unittest discover -s scripts/e2e -p test_auth_fault_conformance.py -v
```

The shell gate `tests/shell/auth_fault_conformance_test.sh` also validates the
wrapper and runs these tests through `make test-shell`.
