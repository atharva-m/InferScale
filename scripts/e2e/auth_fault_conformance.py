"""Local CPU evidence for revoked keys and Valkey outage before native EPP.

This separate experiment creates its own short-lived key and CPU route, and
temporarily stops local Valkey. It is never a release sign-off by itself.
"""

from __future__ import annotations

import base64
import datetime as dt
import json
import math
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tempfile
import time
import urllib.parse
import urllib.request
import uuid

import gateway_conformance as gateway
from trace_conformance import local_origin, private_auth

check = gateway.check
SYSTEM_NAMESPACE = "inferscale-system"
REVOCATION_BOUND_SECONDS = 20  # Authentication cache TTL is 15 seconds.


def local_cluster(context, config):
    check(context.startswith("k3d-inferscale-"), "an explicit local InferScale k3d context is required")
    check(config.get("current-context") == context and len(config.get("clusters", [])) == 1,
          "selected Kubernetes context is ambiguous")
    cluster = config["clusters"][0]["cluster"]
    check(not cluster.get("proxy-url"), "Kubernetes API proxy is not supported for a local outage")
    local_origin(cluster["server"])


def wrong_secret(raw):
    """Preserve a real key ID and valid encoding, but change its secret bytes."""
    parts = raw.split("_", 2)
    check(len(parts) == 3 and parts[0] == "isk" and str(uuid.UUID(parts[1])) == parts[1],
          "issued credential has an unsupported format")
    secret = base64.urlsafe_b64decode(parts[2] + "=" * (-len(parts[2]) % 4))
    check(len(secret) == 32, "issued credential has an unsupported format")
    altered = bytes([secret[0] ^ 1]) + secret[1:]
    return "isk_" + parts[1] + "_" + base64.urlsafe_b64encode(altered).decode().rstrip("=")


def stream_counters(text):
    """Require native stream instrumentation; never equate missing data to zero."""
    names = {"llm_d_epp_extproc_stream_duration_seconds_count": "completed",
             "llm_d_epp_extproc_streams_inflight": "inflight",
             "process_start_time_seconds": "process_start"}
    values = {}
    for line in text.splitlines():
        if line.startswith("#") or not line.strip():
            continue
        parts = line.split()
        if parts[0] not in names:
            continue
        check(len(parts) == 2 and names[parts[0]] not in values, "ambiguous native EPP counter")
        value = float(parts[1])
        check(math.isfinite(value) and value >= 0, "invalid native EPP counter")
        values[names[parts[0]]] = value
    check(set(values) == set(names.values()), "native EPP stream instrumentation is missing")
    check(values["completed"].is_integer() and values["inflight"].is_integer()
          and values["process_start"] > 0, "invalid native EPP counter")
    return values


def unchanged(before, after):
    check(before == after and all(item["inflight"] == 0 for item in after.values()),
          "rejected request reached EPP or EPP restarted")


def database_url(secret, origin):
    """Use only the selected cluster's local PostgreSQL through our own forward."""
    value = base64.b64decode(secret["data"]["database_url"], validate=True).decode()
    parsed = urllib.parse.urlsplit(value)
    check(parsed.scheme in {"postgres", "postgresql"}
          and parsed.hostname in {"postgres", "postgres.inferscale-system", "postgres.inferscale-system.svc",
                                  "postgres.inferscale-system.svc.cluster.local"}
          and (parsed.port or 5432) == 5432 and parsed.path == "/inferscale"
          and parsed.username and parsed.password and not parsed.fragment,
          "storage must identify the local InferScale PostgreSQL service")
    forwarded = urllib.parse.urlsplit(local_origin(origin))
    credentials = urllib.parse.quote(urllib.parse.unquote(parsed.username), safe="") + ":" + urllib.parse.quote(urllib.parse.unquote(parsed.password), safe="")
    # Do not retain query parameters such as host/hostaddr or service, which
    # could override the checked local host inside libpq-compatible clients.
    return "postgresql://" + credentials + "@" + forwarded.netloc + "/inferscale?sslmode=disable"


class Harness(gateway.Harness):
    def __init__(self, work):
        private_auth(os.environ.get("INFERSCALE_CONFORMANCE_AUTH_FILE", ""))
        super().__init__(work)
        self.origin = local_origin(self.origin)
        self.cli = Path(os.environ.get("INFERSCALE_AUTH_FAULT_CTL", "bin/v1-validation/inferscalectl")).resolve()
        check(self.cli.is_file() and os.access(self.cli, os.X_OK), "a prebuilt inferscalectl is required")
        self.owned_key, self.owned_token, self.operator_env = None, None, None
        self.issue_attempted = False
        self.revoked, self.restore_valkey, self.metrics_origins = False, None, {}
        self.result.update({"scope": "local CPU invalid/revoked credentials and Valkey outage before native EPP",
                            "excluded": ["admission-process outage", "PostgreSQL outage", "existing stream continuity during outage",
                                         "GPU behavior/performance", "exact pinned-image release sign-off"]})
        self.result.pop("requests_per_weight", None)

    def forward(self, namespace, resource, port, name):
        logfile = self.work / (name + "-forward.log")
        with logfile.open("w") as handle:
            process = subprocess.Popen([self.kubectl, "--context", self.context, "-n", namespace,
                                        "port-forward", "--address=127.0.0.1", resource, "0:" + str(port)],
                                       stdout=handle, stderr=subprocess.STDOUT)
        self.forwards.append(process)
        def ready():
            check(process.poll() is None, "private port-forward stopped")
            return re.search(r"Forwarding from 127\.0\.0\.1:(\d+)", logfile.read_text())
        self.wait("private loopback forward", ready, 30)
        return "http://127.0.0.1:" + ready()[1]

    def apply(self, item):
        if item["kind"] == "Deployment" and item["metadata"]["name"].endswith("-epp"):
            for container in item["spec"]["template"]["spec"]["containers"]:
                container["args"] = [arg for arg in container.get("args", []) if not arg.startswith("--enable-grpc-stream-metrics")]
                container["args"].append("--enable-grpc-stream-metrics=true")
        return super().apply(item)

    def operator(self, *args):
        result = subprocess.run([str(self.cli), "apikey", *args], env=self.operator_env,
                                capture_output=True, text=True, timeout=30)
        check(result.returncode == 0, "credential operator command failed")
        check(len(result.stdout) <= 65536, "credential operator output exceeded its bound")
        return json.loads(result.stdout) if result.stdout.strip() else None

    def issue_key(self):
        self.stage = "owned short-lived inference credential"
        self.result["credential_cleanup"] = False
        self.issue_attempted = True
        expires = (dt.datetime.now(dt.timezone.utc) + dt.timedelta(minutes=15)).isoformat()
        value = self.operator("issue", "--tenant", self.tenant_id, "--name", "auth-fault-" + self.run_id,
                              "--scope", "inference", "--expires-at", expires)
        key_id = value.get("id", "")
        check(str(uuid.UUID(key_id)) == key_id, "operator did not return a credential identity")
        # Record ownership before checking any remaining output so that a
        # malformed response still triggers revocation in cleanup.
        self.owned_key = key_id
        check(value.get("scopes") == ["inference"], "owned key must grant only inference")
        self.owned_token = value.get("apiKey", "")
        check(self.owned_token.split("_", 2)[:2] == ["isk", key_id], "issued key identity mismatch")
        wrong_secret(self.owned_token)

    def revoke_key(self):
        if self.owned_key and not self.revoked:
            self.operator("revoke", self.owned_key, "--tenant", self.tenant_id)
            self.revoked = True
        self.result["credential_cleanup"] = self.revoked or not self.issue_attempted
        check(self.result["credential_cleanup"], "issued credential cleanup could not be confirmed")

    def metrics(self):
        result = {}
        for role, origin in self.metrics_origins.items():
            with self.http.open(urllib.request.Request(origin + "/metrics"), timeout=10) as response:
                raw = response.read(gateway.MAX_RESPONSE + 1)
                check(response.status == 200 and len(raw) <= gateway.MAX_RESPONSE, "EPP metrics unavailable")
                result[role] = stream_counters(raw.decode())
        check(set(result) == set(self.revisions), "one or more EPP observations are missing")
        return result

    def idle_metrics(self):
        self.wait("EPP streams to close", lambda: all(item["inflight"] == 0 for item in self.metrics().values()), 10)
        return self.metrics()

    def owned_chat(self, raw=None):
        return self.chat(headers={"Authorization": "Bearer " + (raw or self.owned_token)})

    def rejected(self, name, raw, expected):
        self.stage = name
        before, runtime_before = self.idle_metrics(), self.snapshot()
        statuses = [self.owned_chat(raw)[0] for _ in range(3)]
        check(statuses == [expected] * 3, "authorization did not return the required rejection status")
        # A completed rejected HTTP request must not have opened even a failed
        # ext_proc stream; both the inflight gauge and completion counter matter.
        time.sleep(1)
        unchanged(before, self.metrics())
        runtime_after = self.snapshot()
        check(all(runtime_after[role]["requests"] == runtime_before[role]["requests"] for role in self.revisions),
              "rejected request reached the isolated runtime")
        self.passed(name, statuses=statuses, epp_stream_delta=0, runtime_request_delta=0)

    def replica_patch(self, uid, before, after):
        self.kube("-n", SYSTEM_NAMESPACE, "patch", "deployment", "valkey", "--type=json", "-p",
                  json.dumps([{"op": "test", "path": "/metadata/uid", "value": uid},
                              {"op": "test", "path": "/spec/replicas", "value": before},
                              {"op": "replace", "path": "/spec/replicas", "value": after}]))

    def restore_dependency(self):
        if self.restore_valkey is None:
            return
        saved = self.restore_valkey
        current = self.kube("-n", SYSTEM_NAMESPACE, "get", "deployment", "valkey", "-o", "json")
        check(current["metadata"]["uid"] == saved["uid"], "Valkey identity changed during fault experiment")
        replicas = current["spec"].get("replicas", 1)
        check(replicas in {0, saved["replicas"]}, "concurrent Valkey scale change requires operator attention")
        if replicas == 0:
            self.replica_patch(saved["uid"], 0, saved["replicas"])
        self.wait("original Valkey replicas to recover", lambda: self.kube(
            "-n", SYSTEM_NAMESPACE, "get", "deployment", "valkey", "-o", "json").get("status", {}).get("availableReplicas", 0) == saved["replicas"])
        self.restore_valkey = None
        self.result["dependency_restored"] = True

    def outage(self):
        self.stage = "local Valkey outage"
        deployment = self.kube("-n", SYSTEM_NAMESPACE, "get", "deployment", "valkey", "-o", "json")
        replicas = deployment["spec"].get("replicas", 1)
        check(type(replicas) is int and 1 <= replicas <= 3, "Valkey must have a bounded positive replica count")
        selector = deployment["spec"]["selector"]
        check(selector == {"matchLabels": {"app.kubernetes.io/name": "valkey"}}, "unexpected local Valkey selector")
        autoscalers = self.kube("-n", SYSTEM_NAMESPACE, "get", "hpa", "-o", "json")["items"]
        check(not any(item["spec"]["scaleTargetRef"].get("name") == "valkey" for item in autoscalers),
              "Valkey autoscaling must be disabled before a local outage experiment")
        # Arm restoration before sending the mutation: a timeout is ambiguous.
        self.restore_valkey = {"uid": deployment["metadata"]["uid"], "replicas": replicas}
        self.result["dependency_restored"] = False
        try:
            self.replica_patch(deployment["metadata"]["uid"], replicas, 0)
            self.wait("all local Valkey pods to stop", lambda: not self.kube(
                "-n", SYSTEM_NAMESPACE, "get", "pods", "-l", "app.kubernetes.io/name=valkey", "-o", "json")["items"])
            self.rejected("Valkey outage rejects new inference before EPP", self.owned_token, 503)
        finally:
            self.restore_dependency()
        self.stage = "Valkey recovery"
        self.wait("authorized inference after Valkey recovery", lambda: self.owned_chat()[0] == 200)
        self.identity(self.owned_chat())
        self.passed(self.stage, restored_replicas=replicas)

    def run(self):
        self.stage = "local fault preflight"
        local_cluster(self.context, self.kube("config", "view", "--minify", "-o", "json"))
        status, _, source = self.request("/v1/deployments/" + self.deployment_id)
        check(status == 200, "source deployment is not accessible")
        self.tenant_id = source.get("tenantId", "")
        check(str(uuid.UUID(self.tenant_id)) == self.tenant_id, "source deployment has no tenant identity")
        db_origin = self.forward(SYSTEM_NAMESPACE, "service/postgres", 5432, "postgres")
        secret = self.kube("-n", SYSTEM_NAMESPACE, "get", "secret", "inferscale-storage", "-o", "json")
        self.operator_env = {**os.environ, "INFERSCALE_ENV": "development",
                             "INFERSCALE_DATABASE_URL": database_url(secret, db_origin),
                             "INFERSCALE_FAKE_RUNTIME": "false", "INFERSCALE_FEATURE_BACKEND_AUTO": "false",
                             "INFERSCALE_FEATURE_TRTLLM": "false", "INFERSCALE_FEATURE_PROGRESSIVE_ROLLOUT": "false",
                             "INFERSCALE_FEATURE_SCALE_TO_ZERO": "false"}
        self.issue_key()
        self.prepare()
        self.configure_route(0)
        for role, revision in self.revisions.items():
            self.metrics_origins[role] = self.forward(self.namespace, "service/" + revision + "-epp", "metrics", role + "-metrics")
        before = self.idle_metrics()
        self.identity(self.owned_chat())
        after = self.idle_metrics()
        check(after["stable"]["completed"] - before["stable"]["completed"] == 1
              and after["candidate"] == before["candidate"]
              and after["stable"]["process_start"] == before["stable"]["process_start"],
              "accepted control request did not exercise exactly one native EPP stream")
        self.passed("native EPP observation calibrated", accepted_streams=1)
        self.rejected("valid-format wrong secret rejected before EPP", wrong_secret(self.owned_token), 401)
        self.outage()
        self.stage = "bounded credential revocation"
        self.identity(self.owned_chat())  # Populate admission's credential cache.
        self.revoke_key()
        # Wait for the documented cache bound instead of allowing successful
        # post-revocation requests to count as rejection evidence.
        time.sleep(REVOCATION_BOUND_SECONDS)
        self.rejected("revoked issued credential rejected before EPP", self.owned_token, 401)
        self.result["revocation_bound_seconds"] = REVOCATION_BOUND_SECONDS
        self.result["passed"] = True

    def cleanup(self):
        failures = []
        for label, action in (("dependency", self.restore_dependency), ("owned-key", self.revoke_key), ("fixtures", super().cleanup)):
            try:
                action()
            except BaseException:
                # Continue cleanup even after a second interrupt or a failed
                # restoration. Never put dependency/credential output in JSON.
                failures.append(label)
        self.operator_env, self.owned_token = None, None
        self.result["fault_cleanup_failures"] = failures
        if failures:
            self.result["passed"] = False


def main():
    os.umask(0o077)
    def interrupted(_signal, _frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupted)
    harness = None
    with tempfile.TemporaryDirectory(prefix="inferscale-auth-fault-") as temporary:
        try:
            harness = Harness(Path(temporary))
            harness.run()
        except BaseException as error:
            if harness is not None:
                harness.result.update(passed=False, failed_stage=harness.stage, error_type=type(error).__name__)
            print("Auth fault experiment failed (" + type(error).__name__ + ")", file=sys.stderr)
        finally:
            if harness is not None:
                harness.cleanup()
                root = Path(os.environ.get("INFERSCALE_AUTH_FAULT_RESULTS_DIR", "benchmarks/results/auth-faults"))
                root.mkdir(mode=0o700, parents=True, exist_ok=True)
                output = root / harness.run_id
                output.mkdir(mode=0o700)
                path = output / "summary.json"
                path.write_text(json.dumps(harness.result, indent=2) + "\n")
                print("Auth fault evidence: " + str(path.resolve()))
    return 0 if harness is not None and harness.result["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
