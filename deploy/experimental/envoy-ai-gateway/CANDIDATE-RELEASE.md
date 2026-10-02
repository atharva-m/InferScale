# Render and install the dependency qualification candidate

This opt-in path installs the patched Envoy Gateway v1.8.1 and AI Gateway v0.7.0
controllers and selects the patched llm-d endpoint picker for the remote platform
manifest. It does not change `versions.lock.yaml`, record conformance as passed,
or authorize a production release. The operator supplies immutable image digests
built from the recorded candidate sources. A digest validates image identity;
the renderer cannot prove that an arbitrary supplied image contains those patches.
Keep the build records and registry digests with the qualification evidence.

The AI Gateway image must include both the weighted-pool changes and the
`--secretNamespaces` addition in `weighted-canary.provenance.json`. The picker
must match `../llm-d/round-robin-picker.provenance.json`. The addon is used for
InferencePool translation; AI provider routing, MCP and automatic sidecar
injection are not enabled by this integration.

## Offline inputs

Install Python 3 with PyYAML and Helm 3 on the rendering machine. The renderer
makes no network calls and does not require Kubernetes credentials. Obtain these
files separately, retaining the indicated local names:

| Local file | Recorded source and SHA-256 |
| --- | --- |
| `envoy-gateway.yaml` | `candidate-dependencies.json`, Envoy Gateway v1.8.1 `install.yaml` |
| `inference-extension.yaml` | `versions.lock.yaml`, `gatewayAPIInferenceExtension.manifestURL` / `manifestSHA256` |
| `llmd-objective.yaml` | `versions.lock.yaml`, `llmd.objectiveCRDURL` / `objectiveCRDSHA256` |
| `keda.yaml` | `versions.lock.yaml`, `keda.manifestURL` / `manifestSHA256` |
| `service-monitor.yaml` | `versions.lock.yaml`, `prometheusOperator.serviceMonitorCRDURL` / `serviceMonitorCRDSHA256` |
| `pod-monitor.yaml` | `versions.lock.yaml`, `prometheusOperator.podMonitorCRDURL` / `podMonitorCRDSHA256` |
| AI Gateway v0.7.0 source archive, passed separately | `candidate-dependencies.json`, `aiGateway.archiveURL` / `archiveSHA256` |

Every source is verified before parsing or rendering. The Envoy Gateway release
already includes ten Gateway API v1.5.1 experimental CRDs. Use that complete
checksummed inventory; installing the standard bundle afterwards can remove the
experimental fields. The renderer also includes all six AI Gateway CRDs because
the addon starts their informers even when only InferencePool is in use.

To acquire all seven inputs on a connected machine, run this from the repository
root. It uses only the recorded URLs, rejects checksum mismatches before saving
each file, and does not contact Kubernetes:

```bash
export DEPENDENCY_SOURCE_DIR=/absolute/path/to/dependency-manifests
python3 - "$DEPENDENCY_SOURCE_DIR" <<'PY'
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import yaml

spec = importlib.util.spec_from_file_location("candidate", "scripts/render-candidate-dependencies.py")
candidate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(candidate)
record = json.loads((candidate.CANDIDATE / "candidate-dependencies.json").read_text())
lock = yaml.safe_load(Path("versions.lock.yaml").read_text())
sources = candidate.dependency_sources(lock, record)
sources["ai-gateway-v0.7.0.tar.gz"] = {
    "manifestURL": record["aiGateway"]["archiveURL"],
    "manifestSHA256": record["aiGateway"]["archiveSHA256"],
}
directory = Path(sys.argv[1])
directory.mkdir(parents=True, exist_ok=True)
for name, source in sources.items():
    data = subprocess.run([
        "curl", "--fail", "--show-error", "--silent", "--location",
        "--proto", "=https", "--proto-redir", "=https", "--tlsv1.2",
        source["manifestURL"],
    ], check=True, capture_output=True).stdout
    if hashlib.sha256(data).hexdigest() != source["manifestSHA256"]:
        raise SystemExit(f"checksum mismatch: {name}")
    (directory / name).write_bytes(data)
    print(f"verified {name}")
PY
```

Use real registry digests in the three image variables below. Supply the private
Kubernetes API Service address and endpoint address as exact `/32` or `/128`
CIDRs; the correct addresses depend on the cluster's Service NAT and network
policy implementation. The example addresses must be replaced with the cluster's
actual values. API port 443 and the configured endpoint port are allowed.

```bash
python3 scripts/render-candidate-dependencies.py render \
  --manifest-dir /absolute/path/to/dependency-manifests \
  --ai-archive /absolute/path/to/v0.7.0.tar.gz \
  --envoy-image "$CANDIDATE_ENVOY_IMAGE" \
  --addon-image "$CANDIDATE_ADDON_IMAGE" \
  --epp-image "$CANDIDATE_EPP_IMAGE" \
  --kubernetes-api-cidr 10.43.0.1/32 \
  --kubernetes-api-cidr 10.20.0.5/32 \
  --kubernetes-api-port 6443 \
  --output /absolute/path/to/candidate-bundle
```

The output directory must not already exist. All validation happens before it
is created. The generated directory is mode 0700 and its files are mode 0600;
`20-support.yaml` contains fresh private webhook TLS material generated by the
authenticated upstream chart. Keep the bundle private and review it locally.
Rerendering rotates this material and changes the addon pod annotation so a
rollout loads the corresponding certificate. Preserve the previous bundle for
rollback. The upstream chart's generated serving certificate expires after one
year; rerender and qualify an updated bundle before expiry.

For private images, add `--image-pull-secret <name>`. That Secret must already
exist in both `envoy-gateway-system` and `envoy-ai-gateway-system` before the
controller stages start. The installer creates namespaces first; it does not
copy registry credentials. The remote platform's separate
`INFERSCALE_IMAGE_PULL_SECRET` setting still controls application and tenant
workload image pulls.

## Review and install

The staged files make startup ordering visible:

1. `00-namespaces.yaml`: dependency namespaces and the Gateway namespace.
2. `10-crds.yaml`: Gateway, InferencePool, InferenceObjective, addon, KEDA and
   monitoring definitions. Every CRD must become Established before proceeding.
3. `20-support.yaml`: Services, RBAC, configurations, required webhook TLS and
   webhook registration, and the addon network policy.
4. `30-addon.yaml`: the AI Gateway controller. Its new rollout must complete
   before the Envoy Gateway controller is applied.
5. `40-controllers.yaml`: Envoy Gateway, its certificate job and KEDA. The
   certificate job name includes the candidate image identity so upgrades do not
   try to mutate an existing Job's immutable pod template.

The network policy admits extension gRPC only from the Envoy Gateway controller
on port 1063. Addon egress is limited to cluster DNS and the configured private
API endpoints. The webhook has an unsatisfiable Pod selector, so it never injects
a second proxy or sidecar. Its TLS registration remains present because the
upstream binary requires it at startup.

Pools and route metadata are watched in all namespaces, including newly created
tenants. Secret caching and Secret RBAC are limited to the addon and Gateway
namespaces. Cluster-wide addon writes are limited to InferencePool status,
Events, and its own named webhook registration. Gateway-namespace Secret writes
and Pod patches are retained for the upstream Gateway controller path. The addon
cannot write Gateway, HTTPRoute or SecurityPolicy specifications, or roll out
proxy Deployments. These are dedicated InferScale cluster dependencies; the
addon should not be shared with unrelated AI Gateway installations.

`extensionManager.failOpen` is explicitly false. Extension hook errors preserve
the last accepted xDS configuration and prevent incomplete replacement updates.
This is distinct from per-request external authorization and EPP failure
behavior, which must also pass the live conformance suite.

After reviewing the bundle and selecting the intended Kubernetes context:

```bash
python3 scripts/render-candidate-dependencies.py verify /absolute/path/to/candidate-bundle
export TARGET_KUBE_CONTEXT=your-qualification-context
kubectl --context "$TARGET_KUBE_CONTEXT" cluster-info
scripts/install-platform-dependencies.sh \
  --candidate-bundle /absolute/path/to/candidate-bundle \
  --context "$TARGET_KUBE_CONTEXT"
```

The installer snapshots and verifies every staged file before its first cluster
write. A CRD or addon rollout failure stops installation before the Envoy Gateway
controller stage. It performs no downloads. A successful rollout proves only
controller readiness, not weighted routing correctness or production conformance.

Set the candidate bundle when using the normal remote release renderer:

```bash
export INFERSCALE_CANDIDATE_DEPENDENCY_BUNDLE=/absolute/path/to/candidate-bundle
# Supply the existing application image, public origin, benchmark and GPU-node
# inputs documented in docs/deployment/vast-5090.md, then render:
scripts/render-remote-release.sh vast-4x5090 > /absolute/path/to/platform-candidate.yaml
```

This verifies the bundle and replaces `INFERSCALE_EPP_IMAGE` with its recorded
picker digest. It annotates the platform configuration as qualification pending.
It never applies the platform manifest or changes the independent release gate.
`vast-8x5090` uses the same dependency path. With the environment variable unset,
the existing locked dependency behavior is unchanged.

Run the CPU fake-worker Gateway suite on the candidate first, then the serial
local GPU smoke and exact-image remote qualification. Preserve bounded test output, redacted trace summaries, accepted xDS and image
identities. Do not retain credentials, prompts, completions or raw request bodies. On failure, restore the previously
reviewed dependency and platform bundles and verify their rollouts; retain the
failed candidate and evidence. CRD schema downgrade and resource pruning are not
automated rollback operations.
