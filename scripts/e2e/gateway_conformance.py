"""Isolated CPU Gateway experiments; results are not release sign-off evidence."""

from __future__ import annotations

import concurrent.futures
import copy
import datetime as dt
import hashlib
import ipaddress
import json
import math
import os
import re
import signal
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from pathlib import Path

MANAGED = "app.kubernetes.io/managed-by"
REVISION = "inferscale.io/revision"
RUN = "inferscale.io/conformance-run"
MAX_RESPONSE = 1 << 20
SPOOF_HEADERS = (
    "x-llm-d-inference-fairness-id", "x-llm-d-inference-objective",
    "x-llm-d-model-name-rewrite", "x-llm-d-slo-ttft-ms", "x-llm-d-slo-tpot-ms",
    "x-inferscale-tenant-id", "x-inferscale-deployment-id", "x-inferscale-priority-class",
    "x-gateway-inference-fairness-id", "x-gateway-inference-objective", "x-gateway-model-name-rewrite",
)


def check(value, message):
    if not value:
        raise RuntimeError(message)


def bounded(name, default, minimum, maximum):
    value = os.environ.get(name, str(default))
    check(re.fullmatch(r"[0-9]+", value), f"{name} must be an integer")
    value = int(value)
    check(minimum <= value <= maximum, f"{name} must be {minimum}–{maximum}")
    return value


def local_origin(value):
    check(isinstance(value, str) and value == value.strip(), "base_url must be a loopback HTTP(S) origin")
    url = urllib.parse.urlsplit(value)
    check(url.scheme in {"http", "https"} and url.hostname and not url.username and not url.password
          and "@" not in url.netloc and url.path in {"", "/"} and not url.query and not url.fragment,
          "base_url must be a loopback HTTP(S) origin")
    check(url.hostname == "localhost" or ipaddress.ip_address(url.hostname).is_loopback,
          "port-forward the Gateway to a loopback origin")
    if url.port is not None:
        check(0 < url.port < 65536, "invalid origin port")
    return value.rstrip("/")


def distribution(count, total, percent):
    check(type(count) is int and type(total) is int and 0 <= count <= total and total > 0,
          "invalid distribution counts")
    check(0 <= percent <= 100, "invalid distribution percentage")
    expected = total * percent / 100
    tolerance = 0 if percent in {0, 100} else max(3, math.ceil(4 * math.sqrt(total * percent / 100 * (1 - percent / 100))))
    passed = abs(count - expected) <= tolerance
    if percent not in {0, 100}:
        passed = passed and 0 < count < total
    return {"observed": count, "total": total, "expected": expected,
            "tolerance_requests": tolerance, "method": "four binomial standard deviations, minimum three; exact at 0/100",
            "passed": passed}


def cyclic_distribution(sequence, identities):
    """Require repeated cycles, not merely balanced counts from random routing."""
    check(len(identities) == 2 and len(set(identities)) == 2, "cyclic fixture needs two distinct worker identities")
    check(len(sequence) >= 4, "cyclic fixture needs at least two complete cycles")
    counts = {identity: sequence.count(identity) for identity in identities}
    passed = (set(sequence) == set(identities)
              and all(sequence[index] != sequence[index - 1] for index in range(1, len(sequence)))
              and abs(counts[identities[0]] - counts[identities[1]]) <= 1)
    return {"sequence": sequence, "counts": counts, "total": len(sequence),
            "method": "strict alternation across sequential requests to one stable two-endpoint pool and one EPP process",
            "passed": passed}


def round_robin_pod_state(pods):
    """A process/endpoints snapshot; None while the three fixture Pods settle."""
    if len(pods) != 3:
        return None
    components = [pod["metadata"].get("labels", {}).get("app.kubernetes.io/component") for pod in pods]
    if components.count("endpoint-picker") != 1 or components.count("model-server") != 2:
        return None
    for pod in pods:
        ready = any(item.get("type") == "Ready" and item.get("status") == "True" for item in pod.get("status", {}).get("conditions", []))
        if not ready or pod["metadata"].get("deletionTimestamp") or not pod.get("status", {}).get("containerStatuses"):
            return None
    return sorted([{
        "name": pod["metadata"]["name"], "uid": pod["metadata"]["uid"],
        "pod_ip": pod["status"].get("podIP"),
        "containers": [{"name": container["name"], "image": container.get("image"),
                        "image_id": container.get("imageID"), "restart_count": container.get("restartCount", 0)}
                       for container in pod["status"]["containerStatuses"]],
    } for pod in pods], key=lambda item: item["name"])


def picker_config(policy="load-aware"):
    # A bounded load-aware configuration of the pinned native plugins. Prefix
    # producers are deliberately absent because these workers do no inference.
    config = {
        "apiVersion": "llm-d.ai/v1alpha1", "kind": "EndpointPickerConfig", "featureGates": ["flowControl"],
        "plugins": [
            {"type": "round-robin-fairness-policy", "name": "tenant-fairness"},
            {"type": "fcfs-ordering-policy", "name": "fcfs"},
            {"type": "concurrency-detector", "name": "saturation", "parameters": {"maxConcurrency": 1, "concurrencyMode": "requests", "headroom": 0}},
            {"type": "metrics-data-source"}, {"type": "core-metrics-extractor"},
            {"type": "queue-scorer", "name": "routing-scorer"}, {"type": "max-score-picker", "name": "picker"},
        ],
        "dataLayer": {"injectDefaults": False, "sources": [{"pluginRef": "metrics-data-source", "extractors": [{"pluginRef": "core-metrics-extractor"}]}]},
        "saturationDetector": {"pluginRef": "saturation"},
        "flowControl": {"maxRequests": "2", "defaultRequestTTL": "30s", "priorityBands": [
            {"priority": p, "maxRequests": "2", "fairnessPolicyRef": "tenant-fairness", "orderingPolicyRef": "fcfs"} for p in (100, 0, -10)
        ]},
        "schedulingProfiles": [{"name": "default", "plugins": [{"pluginRef": "routing-scorer"}, {"pluginRef": "picker"}]}],
    }
    if policy == "round-robin":
        config["plugins"] = [item for item in config["plugins"] if item.get("name") not in {"routing-scorer", "picker"}]
        config["plugins"].append({"type": "round-robin-picker", "name": "picker"})
        config["schedulingProfiles"][0]["plugins"] = [{"pluginRef": "picker"}]
    else:
        check(policy == "load-aware", "unsupported CPU fixture policy")
    return config


def substitute(value, names):
    if isinstance(value, dict):
        return {key: substitute(item, names) for key, item in value.items()}
    if isinstance(value, list):
        return [substitute(item, names) for item in value]
    if isinstance(value, str):
        for old in sorted(names, key=len, reverse=True):
            value = value.replace(old, names[old])
        return value.replace("inferscale-controller", "inferscale-conformance")
    return value


def revision_fixture(templates, source_revision, target, identity, namespace, model, image, run_id):
    selected = [item for item in templates if item["kind"] in {
        "ConfigMap", "ServiceAccount", "Role", "RoleBinding", "InferencePool", "InferenceObjective", "NetworkPolicy"
    } or item["kind"] in {"Deployment", "Service"} and item["metadata"].get("labels", {}).get("app.kubernetes.io/component") == "endpoint-picker"]
    required = {"ConfigMap", "ServiceAccount", "Role", "RoleBinding", "InferencePool", "InferenceObjective", "Deployment", "Service", "NetworkPolicy"}
    check(required <= {item["kind"] for item in selected}, "source revision is missing EPP/pool/objective/network resources")
    names = {source_revision: target}
    for item in selected:
        kind = item["kind"]
        suffix = {"ConfigMap": "epp-config", "InferencePool": "pool", "NetworkPolicy": "runtime"}.get(kind, "epp")
        if kind == "InferenceObjective":
            priority = item["spec"].get("priority", 0)
            check(priority in {100, 0, -10}, "unknown source objective priority")
            suffix = {100: "interactive", 0: "standard", -10: "batch"}[priority]
        names[item["metadata"]["name"]] = target + "-" + suffix
    result = []
    for original in selected:
        item = substitute(copy.deepcopy(original), names)
        metadata = item["metadata"]
        item["metadata"] = {"name": metadata["name"], "namespace": namespace,
                            "labels": {**metadata.get("labels", {}), MANAGED: "inferscale-conformance", RUN: run_id}}
        item.pop("status", None)
        if item["kind"] == "ServiceAccount":
            # Legacy clusters may still list an account-bound token Secret.
            # A clone must obtain its own projected identity, never reuse it.
            item.pop("secrets", None)
        if item["kind"] == "Service":
            item["spec"] = {key: item["spec"][key] for key in ("selector", "ports")}
            for port in item["spec"]["ports"]:
                port.pop("nodePort", None)
        if item["kind"] == "ConfigMap":
            item["data"] = {"endpoint-picker-config.yaml": json.dumps(picker_config())}
        if item["kind"] == "Deployment":
            item["spec"]["replicas"] = 1
            item["spec"]["strategy"] = {"type": "Recreate"}
            pod = item["spec"]["template"]
            pod["metadata"] = {"labels": {**pod["metadata"]["labels"], RUN: run_id}}
            for container in pod["spec"]["containers"]:
                container["resources"] = {"requests": {"cpu": "100m", "memory": "256Mi"}, "limits": {"cpu": "500m", "memory": "768Mi"}}
                container["args"] = ["--model-server-metrics-port=8000" if arg.startswith("--model-server-metrics-port=") else arg for arg in container.get("args", [])]
        result.append(item)
    pool = next(item for item in result if item["kind"] == "InferencePool")
    labels = {**pool["spec"]["selector"]["matchLabels"], RUN: run_id}
    labels[MANAGED] = "inferscale-conformance"
    worker_name = target + "-worker"
    result.append({"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": worker_name, "namespace": namespace, "labels": labels}, "spec": {
        "replicas": 1, "selector": {"matchLabels": labels}, "strategy": {"type": "Recreate"},
        "template": {"metadata": {"labels": labels}, "spec": {
            "automountServiceAccountToken": False, "terminationGracePeriodSeconds": 5,
            "securityContext": {"runAsNonRoot": True, "runAsUser": 65532, "seccompProfile": {"type": "RuntimeDefault"}},
            "containers": [{"name": "runtime", "image": image, "imagePullPolicy": "IfNotPresent",
                "env": [{"name": "SERVED_MODEL_NAME", "value": model}, {"name": "INFERSCALE_FAKE_TEST_CONTROLS", "value": "true"}, {"name": "INFERSCALE_FAKE_TEST_IDENTITY", "value": identity}],
                "ports": [{"name": "http", "containerPort": 8000}],
                "resources": {"requests": {"cpu": "50m", "memory": "64Mi"}, "limits": {"cpu": "250m", "memory": "128Mi"}},
                "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True, "capabilities": {"drop": ["ALL"]}},
                "readinessProbe": {"httpGet": {"path": "/health", "port": "http"}, "periodSeconds": 2},
            }],
        }},
    }})
    epp = next(item for item in result if item["kind"] == "Deployment" and item["metadata"]["name"] == target + "-epp")
    pull_secrets = epp["spec"]["template"]["spec"].get("imagePullSecrets")
    if pull_secrets:
        result[-1]["spec"]["template"]["spec"]["imagePullSecrets"] = copy.deepcopy(pull_secrets)
    result.append({"apiVersion": "v1", "kind": "Service", "metadata": {"name": target + "-runtime", "namespace": namespace, "labels": labels},
                   "spec": {"selector": labels, "ports": [{"name": "http", "port": 8000, "targetPort": "http"}]}})
    check(all(item["metadata"]["name"].startswith(target + "-") for item in result), "fixture escaped its unique name prefix")
    return result


def round_robin_fixture(templates, source_revision, target, namespace, model, image, run_id):
    """Two distinct single-replica CPU workers behind one pool and one EPP."""
    items = revision_fixture(templates, source_revision, target, "rr-a", namespace, model, image, run_id)
    worker = next(item for item in items if item["kind"] == "Deployment" and item["metadata"]["name"] == target + "-worker")
    runtime_service = next(item for item in items if item["kind"] == "Service" and item["metadata"]["name"] == target + "-runtime")
    config = next(item for item in items if item["kind"] == "ConfigMap")
    config["data"]["endpoint-picker-config.yaml"] = json.dumps(picker_config("round-robin"))
    items.remove(worker)
    for identity in ("rr-a", "rr-b"):
        clone = copy.deepcopy(worker)
        clone["metadata"]["name"] = target + "-worker-" + identity
        # Distinct Deployment selectors avoid overlapping ReplicaSet ownership;
        # the InferencePool retains the common revision/run selector.
        label = "inferscale.io/conformance-worker"
        for labels in (clone["metadata"]["labels"], clone["spec"]["selector"]["matchLabels"], clone["spec"]["template"]["metadata"]["labels"]):
            labels[label] = identity
        env = clone["spec"]["template"]["spec"]["containers"][0]["env"]
        next(item for item in env if item["name"] == "INFERSCALE_FAKE_TEST_IDENTITY")["value"] = identity
        diagnostic_service = copy.deepcopy(runtime_service)
        diagnostic_service["metadata"]["name"] = target + "-runtime-" + identity
        diagnostic_service["spec"]["selector"][label] = identity
        items.extend((clone, diagnostic_service))
    return items


def route_fixture(name, namespace, parent_refs, path, run_id, stable, candidate, percent, mirror=0):
    check(0 <= percent <= 100 and 0 <= mirror <= 100, "invalid route fraction")
    filters = [
        {"type": "URLRewrite", "urlRewrite": {"path": {"type": "ReplaceFullPath", "replaceFullPath": "/v1/chat/completions"}}},
        {"type": "RequestHeaderModifier", "requestHeaderModifier": {"remove": ["x-inferscale-priority-class"]}},
    ]
    if mirror:
        filters.append({"type": "RequestMirror", "requestMirror": {"backendRef": {"group": "", "kind": "Service", "name": candidate + "-runtime", "port": 8000}, "percent": mirror}})
    return {"apiVersion": "gateway.networking.k8s.io/v1", "kind": "HTTPRoute", "metadata": {
        "name": name, "namespace": namespace, "labels": {MANAGED: "inferscale-conformance", RUN: run_id},
    }, "spec": {"parentRefs": copy.deepcopy(parent_refs), "rules": [{
        "matches": [{"method": "POST", "path": {"type": "Exact", "value": path}, "headers": [
            {"type": "Exact", "name": "x-inferscale-conformance-run", "value": run_id},
            {"type": "Exact", "name": "x-inferscale-conformance-fixture", "value": "cpu-native"},
        ]}],
        "filters": filters, "backendRefs": [
            {"group": "inference.networking.k8s.io", "kind": "InferencePool", "name": revision + "-pool", "weight": weight,
             "filters": [{"type": "RequestHeaderModifier", "requestHeaderModifier": {"set": [{"name": "x-llm-d-inference-objective", "value": revision + "-standard"}]}}]}
            for revision, weight in ((stable, 100 - percent), (candidate, percent))
        ],
    }]}}


def route_ready(value):
    generation = value.get("metadata", {}).get("generation")
    parents = value.get("status", {}).get("parents", [])
    for parent in parents:
        conditions = {item.get("type"): item for item in parent.get("conditions", [])}
        if all(conditions.get(key, {}).get("status") == "True" and conditions[key].get("observedGeneration") == generation for key in ("Accepted", "ResolvedRefs")):
            return True
    return False


def read_sse(response, model):
    start = time.monotonic()
    total, events, generated, usage = 0, 0, 0, None
    first, done = None, False
    while time.monotonic() - start < 20:
        line = response.readline(65537)
        total += len(line)
        check(len(line) <= 65536 and total <= MAX_RESPONSE, "SSE exceeded bounded response size")
        if not line:
            break
        if not line.startswith(b"data:"):
            continue
        check(not done, "SSE data appeared after [DONE]")
        data = line[5:].strip()
        if data == b"[DONE]":
            done = True
            continue
        item = json.loads(data)
        check(isinstance(item, dict), "SSE data must be a JSON object")
        events += 1
        if item.get("choices"):
            check(item.get("model") == model, "SSE model identity changed")
            if any(choice.get("delta", {}).get("content") for choice in item["choices"]):
                generated += 1
                first = first if first is not None else time.monotonic() - start
        if item.get("usage"):
            usage = item["usage"]
    check(done and generated > 0 and usage is not None, "native SSE omitted content, usage, or [DONE]")
    check(all(type(usage.get(key)) is int and usage[key] > 0 for key in ("prompt_tokens", "completion_tokens", "total_tokens"))
          and usage["prompt_tokens"] + usage["completion_tokens"] == usage["total_tokens"], "invalid SSE usage")
    return {"events": events, "generated_events": generated, "usage": usage,
            "first_content_seconds": first, "stream_seconds": time.monotonic() - start}


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Harness:
    def __init__(self, work):
        self.work = work
        self.context = os.environ.get("INFERSCALE_CONFORMANCE_KUBE_CONTEXT", "")
        check(self.context and not self.context.startswith("-"), "INFERSCALE_CONFORMANCE_KUBE_CONTEXT is required")
        auth_path = Path(os.environ.get("INFERSCALE_CONFORMANCE_AUTH_FILE", ""))
        check(auth_path.is_file() and not stat.S_IMODE(auth_path.stat().st_mode) & 0o077, "auth file must have mode 0600 or stricter")
        auth = json.loads(auth_path.read_text())
        self.origin = local_origin(auth["base_url"])
        self.token = auth["api_key"]
        check(isinstance(self.token, str) and self.token and not any(char.isspace() for char in self.token), "invalid API key")
        self.deployment_id = os.environ.get("INFERSCALE_CONFORMANCE_DEPLOYMENT_ID", auth.get("deployment_id", ""))
        check(str(uuid.UUID(self.deployment_id)) == self.deployment_id, "an existing authorized deployment UUID is required")
        self.image = os.environ.get("INFERSCALE_CONFORMANCE_FAKE_IMAGE", "ghcr.io/inferscale/fake-runtime:0.1.0-dev")
        check(self.image and "fake" in self.image and not any(char.isspace() for char in self.image), "an explicit fake-runtime image is required")
        self.count = bounded("INFERSCALE_CONFORMANCE_REQUESTS", 100, 20, 1000)
        round_robin = os.environ.get("INFERSCALE_CONFORMANCE_ROUND_ROBIN", "false")
        check(round_robin in {"true", "false"}, "INFERSCALE_CONFORMANCE_ROUND_ROBIN must be true or false")
        self.round_robin_enabled = round_robin == "true"
        self.round_robin_count = bounded("INFERSCALE_CONFORMANCE_ROUND_ROBIN_REQUESTS", 20, 10, 200)
        self.wait_seconds = bounded("INFERSCALE_CONFORMANCE_WAIT_SECONDS", 180, 30, 600)
        self.interval = bounded("INFERSCALE_CONFORMANCE_INTERVAL_MS", 0, 0, 1000) / 1000
        self.kubectl = os.environ.get("KUBECTL", "kubectl")
        self.run_id = uuid.uuid4().hex[:12]
        self.prefix = "gc-" + self.run_id
        self.route_name = self.prefix + "-route"
        self.revisions = {role: self.prefix + "-" + role for role in ("stable", "candidate")}
        self.path = "/v1/deployments/" + self.deployment_id + "/chat/completions"
        self.http = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
        self.created, self.forwards, self.local = [], [], {}
        self.stage, self.namespace = "preflight", ""
        self.result = {"schema_version": 1, "run_id": self.run_id, "recorded_at": dt.datetime.now(dt.timezone.utc).isoformat(),
                       "passed": False, "release_signoff": False, "requests_per_weight": self.count, "checks": [],
                       "excluded": ["GPU runtime behavior/performance", "controller automatic rollout promotion", "auth/Valkey outage", "revocation propagation", "distributed trace export/privacy", "exact pinned-image release sign-off"]}
        self.result["round_robin"] = {"requested": self.round_robin_enabled, "status": "not-run"}
        if not self.round_robin_enabled:
            self.result["excluded"].append("within-pool cyclic round-robin (optional check disabled)")

    def kube(self, *args, payload=None):
        result = subprocess.run([self.kubectl, "--context", self.context, "--request-timeout=25s", *args],
                                input=json.dumps(payload) if payload is not None else None, capture_output=True, text=True, timeout=35)
        check(result.returncode == 0, f"kubectl failed during {self.stage}")
        return json.loads(result.stdout) if result.stdout.strip().startswith(("{", "[")) else result.stdout

    def wait(self, description, predicate, seconds=None):
        deadline = time.monotonic() + (seconds or self.wait_seconds)
        while time.monotonic() < deadline:
            if predicate():
                return
            time.sleep(0.5)
        raise RuntimeError("timed out waiting for " + description)

    def request(self, path=None, payload=None, headers=None, token=True, origin=None, stream=False):
        endpoint = (origin or self.origin) + (path or self.path)
        request_headers = {"Content-Type": "application/json"}
        if origin is None:
            request_headers.update({"x-inferscale-conformance-run": self.run_id, "x-inferscale-conformance-fixture": "cpu-native"})
            if token:
                request_headers["Authorization"] = "Bearer " + self.token
        request_headers.update(headers or {})
        data = json.dumps(payload).encode() if payload is not None else None
        try:
            response = self.http.open(urllib.request.Request(endpoint, data=data, headers=request_headers), timeout=20 if stream else 40)
        except urllib.error.HTTPError as response:
            with response:
                # Bodies may contain credentials or request data from an
                # intermediary. Keep only bounded machine-readable codes.
                raw = response.read(MAX_RESPONSE + 1)
                check(len(raw) <= MAX_RESPONSE, "error response exceeded size bound")
                code = ""
                try:
                    parsed = json.loads(raw)
                    code = parsed.get("code", parsed.get("error", {}).get("type", ""))
                except (ValueError, AttributeError):
                    pass
                return response.code, dict(response.headers), {"code": code}
        if stream:
            return response
        with response:
            raw = response.read(MAX_RESPONSE + 1)
            check(len(raw) <= MAX_RESPONSE, "response exceeded size bound")
            return response.status, dict(response.headers), json.loads(raw)

    def chat(self, **kwargs):
        payload = {"model": self.model, "messages": [{"role": "user", "content": "conformance"}], "max_tokens": 8, "temperature": 0}
        return self.request(payload=payload, **kwargs)

    def identity(self, response, allowed=None):
        status, headers, body = response
        check(status == 200, f"inference returned HTTP {status} during {self.stage}")
        headers = {key.lower(): value for key, value in headers.items()}
        identity = headers.get("x-inferscale-test-identity", "")
        check(identity in (self.revisions if allowed is None else allowed) and body.get("model") == self.model, "response did not come from an isolated fake worker")
        return identity

    def passed(self, name, **evidence):
        self.result["checks"].append({"name": name, "passed": True, **evidence})
        print("PASS " + name, flush=True)

    def state(self, role):
        status, _, value = self.request("/__inferscale_test/state", origin=self.local[role])
        check(status == 200 and value.get("identity") == role, "fake image is missing gated conformance diagnostics; rebuild from current code")
        return value

    def snapshot(self):
        return {role: self.state(role) for role in self.revisions}

    def control(self, role, response_delay=0, stream_delay=0):
        status, _, _ = self.request("/__inferscale_test/control", origin=self.local[role], payload={"response_delay_ms": response_delay, "stream_delay_ms": stream_delay})
        check(status == 200, "failed to apply bounded fake-worker control")

    def forward_worker(self, role, service):
        logfile = self.work / (role + "-forward.log")
        handle = logfile.open("w")
        process = subprocess.Popen([self.kubectl, "--context", self.context, "-n", self.namespace, "port-forward", "--address=127.0.0.1", "service/" + service, "0:8000"], stdout=handle, stderr=subprocess.STDOUT)
        handle.close()
        self.forwards.append(process)
        def forwarded():
            check(process.poll() is None, "fake-worker port-forward stopped")
            match = re.search(r"Forwarding from 127\.0\.0\.1:(\d+)", logfile.read_text())
            if match:
                self.local[role] = "http://127.0.0.1:" + match[1]
                return True
            return False
        self.wait(role + " diagnostics port-forward", forwarded, 30)
        self.state(role)

    def apply(self, item):
        key = (item["kind"], item["metadata"]["name"])
        if key not in self.created:
            self.created.append(key)
        return self.kube("-n", self.namespace, "apply", "--server-side", "--field-manager=inferscale-conformance", "-f", "-", "-o", "json", payload=item)

    def configure_route(self, percent, mirror=0):
        item = route_fixture(self.route_name, self.namespace, self.parent_refs, self.path, self.run_id,
                             self.revisions["stable"], self.revisions["candidate"], percent, mirror)
        self.apply(item)
        self.wait("current HTTPRoute Accepted/ResolvedRefs", lambda: route_ready(self.kube("-n", self.namespace, "get", "httproute", self.route_name, "-o", "json")))
        # Allow asynchronous xDS delivery to finish after controller status.
        time.sleep(2)
        route = self.kube("-n", self.namespace, "get", "httproute", self.route_name, "-o", "json")
        self.result.setdefault("route_conditions", []).append({"candidate_percent": percent, "mirror_percent": mirror,
            "generation": route["metadata"]["generation"], "parents": route.get("status", {}).get("parents", [])})

    def prepare(self):
        status, _, source = self.request("/v1/deployments/" + self.deployment_id)
        check(status == 200, "existing deployment is not accessible with this credential")
        self.namespace, self.model = source["namespace"], source["name"]
        cr = self.kube("-n", self.namespace, "get", "inferencedeployment", self.model, "-o", "json")
        source_revision = cr.get("status", {}).get("revision", {}).get("stable", "")
        check(source_revision and not cr.get("status", {}).get("revision", {}).get("candidate"), "source deployment must have a stable revision and no candidate")
        kinds = "deployments,services,configmaps,serviceaccounts,roles,rolebindings,inferencepools,inferenceobjectives,networkpolicies"
        templates = self.kube("-n", self.namespace, "get", kinds, "-l", REVISION + "=" + source_revision, "-o", "json")["items"]
        self.source_revision, self.templates = source_revision, templates
        routes = self.kube("-n", self.namespace, "get", "httproutes", "-l", "inferscale.io/deployment=" + self.model, "-o", "json")["items"]
        matches = [route for route in routes if any(match.get("path", {}).get("value") == self.path for rule in route.get("spec", {}).get("rules", []) for match in rule.get("matches", []))]
        check(len(matches) == 1, "expected one managed inference route for the source public UUID")
        self.parent_refs = matches[0]["spec"]["parentRefs"]
        self.result["source"] = {"deployment_id": self.deployment_id, "namespace": self.namespace, "model": self.model, "revision": source_revision}
        lock = Path(__file__).resolve().parents[2] / "versions.lock.yaml"
        self.result["versions_lock_sha256"] = "sha256:" + hashlib.sha256(lock.read_bytes()).hexdigest()
        self.result["gateway_images"] = []
        for namespace in sorted({parent.get("namespace", self.namespace) for parent in self.parent_refs}):
            gateway_pods = self.kube("-n", namespace, "get", "pods", "-o", "json")["items"]
            self.result["gateway_images"].extend({"namespace": namespace, "pod": pod["metadata"]["name"],
                "images": [{"name": item["name"], "image": item.get("image"), "image_id": item.get("imageID")} for item in pod.get("status", {}).get("containerStatuses", [])]} for pod in gateway_pods)
        self.stage = "isolated CPU fixture creation"
        for role, revision in self.revisions.items():
            items = revision_fixture(templates, source_revision, revision, role, self.namespace, self.model, self.image, self.run_id)
            for item in items:
                self.apply(item)
            self.wait(role + " worker and EPP availability", lambda rev=revision: all(
                self.kube("-n", self.namespace, "get", "deployment", rev + suffix, "-o", "json").get("status", {}).get("availableReplicas", 0) == 1 for suffix in ("-worker", "-epp")))
            self.forward_worker(role, revision + "-runtime")
        pods = self.kube("-n", self.namespace, "get", "pods", "-l", RUN + "=" + self.run_id, "-o", "json")["items"]
        self.result["fixture_images"] = [{"pod": pod["metadata"]["name"], "images": [{"name": item["name"], "image": item.get("image"), "image_id": item.get("imageID")} for item in pod.get("status", {}).get("containerStatuses", [])]} for pod in pods]
        self.passed("two isolated CPU workers and revision EPPs ready")

    def weights(self):
        for percent in (0, 5, 25, 50, 100):
            self.stage = f"native weighted InferencePools at {percent}% candidate"
            self.configure_route(percent)
            before = self.snapshot()
            counts = {role: 0 for role in self.revisions}
            for _ in range(self.count):
                counts[self.identity(self.chat())] += 1
                if self.interval: time.sleep(self.interval)
            self.wait("weighted workers idle", lambda: all(state["active"] == 0 for state in self.snapshot().values()), 15)
            after = self.snapshot()
            deltas = {role: after[role]["requests"] - before[role]["requests"] for role in self.revisions}
            statistics = distribution(counts["candidate"], self.count, percent)
            self.result.setdefault("weight_results", []).append({"candidate_percent": percent, "responses": counts, "runtime_deltas": deltas, "statistics": statistics})
            check(deltas == counts, "runtime request counters disagree with weighted client responses")
            check(statistics["passed"], "native backend weights exceeded stated statistical tolerance")
            for role, count in counts.items():
                if count:
                    check(after[role].get("headers", {}).get("x-llm-d-inference-objective") == self.revisions[role] + "-standard", "backend objective did not match the selected revision")
            self.passed(self.stage)

    def shadow(self):
        self.stage = "native fractional Service mirror"
        self.configure_route(0, 25)
        before = self.snapshot()
        for _ in range(self.count):
            check(self.identity(self.chat()) == "stable", "shadow response escaped to client")
            if self.interval: time.sleep(self.interval)
        self.wait("mirrored workers idle", lambda: all(state["active"] == 0 for state in self.snapshot().values()), 15)
        time.sleep(2)
        after = self.snapshot()
        stable = after["stable"]["requests"] - before["stable"]["requests"]
        candidate = after["candidate"]["requests"] - before["candidate"]["requests"]
        self.result["mirror_result"] = {"stable_runtime_delta": stable, "candidate_runtime_delta": candidate}
        check(stable == self.count, "shadow duplicated or dropped stable runtime work")
        statistics = distribution(candidate, self.count, 25)
        self.result["mirror_result"]["statistics"] = statistics
        check(statistics["passed"], "native request mirror did not satisfy the configured fraction")
        self.passed(self.stage)

    def authorization(self):
        self.stage = "authorization and scheduling-header spoof protection"
        self.configure_route(0)
        self.identity(self.chat())
        baseline = self.state("stable")
        self.identity(self.chat(headers={key: "client-spoof" for key in SPOOF_HEADERS}))
        observed = self.state("stable")
        check(observed["headers"] == baseline["headers"] and not observed["authorization_present"], "client scheduling metadata or bearer reached runtime")
        headers = observed["headers"]
        check(headers.get("x-llm-d-inference-fairness-id") and headers.get("x-llm-d-inference-fairness-id") == headers.get("x-inferscale-tenant-id")
              and headers.get("x-inferscale-deployment-id") == self.deployment_id
              and headers.get("x-llm-d-model-name-rewrite") == self.model
              and headers.get("x-llm-d-inference-objective") == self.revisions["stable"] + "-standard",
              "trusted scheduling identity was not delivered to selected backend")
        before = self.snapshot()
        statuses = [self.chat(token=False)[0], self.chat(headers={"Authorization": "Bearer invalid-conformance-key"})[0]]
        check(statuses == [401, 401], "missing/invalid API keys were not rejected by authorization")
        after = self.snapshot()
        check(all(after[role]["requests"] == before[role]["requests"] for role in self.revisions), "unauthorized request reached a fake runtime")
        self.passed(self.stage, rejected_statuses=statuses, scheduling_fields=sorted(headers))

    def streaming(self):
        self.stage = "native incremental SSE and cancellation"
        self.control("stable", stream_delay=2000)
        payload = {"model": self.model, "messages": [{"role": "user", "content": "conformance"}], "stream": True, "stream_options": {"include_usage": True}, "max_tokens": 8}
        started = time.monotonic()
        response = self.request(payload=payload, stream=True)
        check(not isinstance(response, tuple), "SSE request was rejected")
        with response:
            check(response.headers.get("Content-Type", "").startswith("text/event-stream"), "native SSE content type missing")
            headers_seconds = time.monotonic() - started
            evidence = read_sse(response, self.model)
        check(headers_seconds + evidence["first_content_seconds"] < 1.5 and evidence["stream_seconds"] > 1.5, "Gateway buffered native SSE until generation completed")
        self.control("stable", stream_delay=10000)
        before = self.state("stable")
        response = self.request(payload=payload, stream=True)
        check(not isinstance(response, tuple), "cancellation request was rejected")
        with response:
            first = response.readline(65537)
            check(first.startswith(b"data:") and b"[DONE]" not in first, "cancellation test did not receive initial SSE content")
        self.wait("runtime cancellation propagation", lambda: self.state("stable")["canceled"] > before["canceled"] and self.state("stable")["active"] == 0, 8)
        self.control("stable")
        self.passed(self.stage, sse=evidence, cancellation_observed=True)

    def backpressure(self):
        self.stage = "bounded native EPP queue and recovery"
        self.control("stable", response_delay=3000)
        before = self.snapshot()
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as executor:
            responses = list(executor.map(lambda _: self.chat(), range(8)))
        statuses = [response[0] for response in responses]
        check(set(statuses) <= {200, 429} and 200 in statuses and 429 in statuses, "bounded EPP overload must serve admitted requests and reject overflow with 429")
        for status, headers, body in responses:
            if status == 429:
                # A tenant RPM rejection does not prove EPP queue behavior.
                check(body.get("code") != "rate_limit_exceeded", "tenant rate limit masked EPP backpressure; use an isolated tenant with sufficient RPM")
        self.control("stable")
        self.wait("runtime queue drain", lambda: self.state("stable")["active"] == 0, 15)
        after = self.snapshot()
        check(after["stable"]["requests"] - before["stable"]["requests"] == statuses.count(200)
              and after["candidate"]["requests"] == before["candidate"]["requests"], "rejected or misrouted work reached runtime")
        self.identity(self.chat())
        self.passed(self.stage, admitted=statuses.count(200), rejected=statuses.count(429), recovered=True)

    def round_robin(self):
        self.stage = "cyclic round-robin within one two-worker InferencePool"
        revision = self.prefix + "-round-robin"
        identities = ("rr-a", "rr-b")
        for item in round_robin_fixture(self.templates, self.source_revision, revision, self.namespace, self.model, self.image, self.run_id):
            self.apply(item)
        def pod_state():
            pods = self.kube("-n", self.namespace, "get", "pods", "-l", RUN + "=" + self.run_id + "," + REVISION + "=" + revision, "-o", "json")["items"]
            return round_robin_pod_state(pods)
        self.wait("two round-robin workers and one EPP process", lambda: pod_state() is not None)
        for identity in identities:
            self.forward_worker(identity, revision + "-runtime-" + identity)
        route = route_fixture(self.route_name, self.namespace, self.parent_refs, self.path, self.run_id,
                              revision, revision, 0)
        # The existing weighted tests retain two distinct backends. This phase
        # deliberately has one backendRef so it measures EPP endpoint selection.
        route["spec"]["rules"][0]["backendRefs"] = route["spec"]["rules"][0]["backendRefs"][:1]
        self.apply(route)
        self.wait("round-robin HTTPRoute Accepted/ResolvedRefs", lambda: route_ready(self.kube("-n", self.namespace, "get", "httproute", self.route_name, "-o", "json")))
        time.sleep(2)
        observed = set()
        def warm():
            response = self.chat()
            if response[0] in {429, 503}:
                return False
            observed.add(self.identity(response, allowed=identities))
            return observed == set(identities)
        self.wait("both round-robin endpoints discovered by EPP", warm)
        self.wait("round-robin warmup drain", lambda: all(self.state(identity)["active"] == 0 for identity in identities), 15)
        processes = pod_state()
        check(processes is not None, "round-robin fixture changed during warmup")
        before = {identity: self.state(identity) for identity in identities}
        sequence = []
        for _ in range(self.round_robin_count):
            sequence.append(self.identity(self.chat(), allowed=identities))
            self.wait("sequential round-robin request completion", lambda: all(self.state(identity)["active"] == 0 for identity in identities), 15)
            time.sleep(max(self.interval, 0.05))
        after = {identity: self.state(identity) for identity in identities}
        statistics = cyclic_distribution(sequence, identities)
        deltas = {identity: after[identity]["requests"] - before[identity]["requests"] for identity in identities}
        self.result["round_robin"] = {
            "requested": True, "status": "checked", "pool": revision + "-pool", "policy": "round-robin-picker",
            "statistics": statistics, "runtime_deltas": deltas, "fixture_processes": processes,
        }
        check(pod_state() == processes, "round-robin endpoints or EPP process changed during the measured cycle")
        check(deltas == statistics["counts"], "round-robin runtime counters disagree with client identities")
        check(all(after[identity].get("headers", {}).get("x-llm-d-inference-objective") == revision + "-standard" for identity in identities),
              "round-robin request did not retain its pool objective")
        check(statistics["passed"], "two-worker round-robin did not alternate for every sequential request")
        self.passed(self.stage, requests=self.round_robin_count, counts=statistics["counts"])

    def run(self):
        self.prepare()
        self.weights()
        self.shadow()
        self.authorization()
        self.streaming()
        self.backpressure()
        if self.round_robin_enabled:
            self.round_robin()
        self.result["passed"] = True

    def cleanup(self):
        for process in self.forwards:
            process.terminate()
            try: process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        failures = []
        # Remove only the named objects created by this invocation, including
        # its more-specific route, without touching the source deployment.
        for kind, name in sorted(self.created, key=lambda pair: pair[0] != "HTTPRoute"):
            if not name.startswith(self.prefix + "-"):
                failures.append("unsafe-name")
                continue
            try:
                self.kube("-n", self.namespace, "delete", kind, name, "--ignore-not-found", "--wait=false")
            except (RuntimeError, subprocess.SubprocessError):
                failures.append(kind + "/" + name)
        self.result["cleanup"] = {"deleted": not failures, "remaining": failures}
        if failures:
            self.result["passed"] = False


def main():
    os.umask(0o077)
    def interrupted(_signal, _frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, interrupted)
    harness = None
    with tempfile.TemporaryDirectory(prefix="inferscale-gateway-check-") as temporary:
        try:
            harness = Harness(Path(temporary))
            harness.run()
        except (RuntimeError, ValueError, LookupError, OSError, TypeError, subprocess.SubprocessError, KeyboardInterrupt) as error:
            if harness is None:
                print("Conformance configuration failed: " + type(error).__name__, file=sys.stderr)
                return 1
            harness.result["failed_stage"] = harness.stage
            # Avoid copying exception strings containing URLs, response text,
            # subprocess output or credentials into the retained artifact.
            harness.result["error_type"] = type(error).__name__
            if isinstance(error, RuntimeError):
                # RuntimeErrors originate only from this module's fixed
                # assertions, never from returned HTTP bodies/tool output.
                harness.result["failure"] = str(error)
            print("FAIL " + harness.stage + " (" + type(error).__name__ + ")", file=sys.stderr)
        finally:
            if harness is not None:
                harness.cleanup()
                output = Path(os.environ.get("INFERSCALE_CONFORMANCE_RESULTS_DIR", "benchmarks/results"))
                output.mkdir(parents=True, exist_ok=True)
                path = output / ("gateway-cpu-" + harness.run_id + ".json")
                path.write_text(json.dumps(harness.result, indent=2) + "\n")
                print("Evidence: " + str(path.resolve()), flush=True)
    return 0 if harness is not None and harness.result["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
