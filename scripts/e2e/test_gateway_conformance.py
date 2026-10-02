"""Offline safety/protocol tests for the CPU Gateway conformance harness."""

import copy
import io
import json
import os
import unittest
from unittest.mock import patch

from gateway_conformance import (
    MANAGED, REVISION, RUN, bounded, cyclic_distribution, distribution, local_origin,
    picker_config, read_sse, revision_fixture, round_robin_fixture, round_robin_pod_state, route_fixture, route_ready,
)


def source_resources():
    revision = "source-revision"
    labels = {MANAGED: "inferscale-controller", REVISION: revision, "app.kubernetes.io/component": "endpoint-picker"}
    worker_labels = {**labels, "app.kubernetes.io/component": "model-server"}
    def obj(kind, suffix, spec):
        return {"apiVersion": "test/v1", "kind": kind, "metadata": {
            "name": revision + "-" + suffix, "namespace": "tenant-test", "labels": dict(labels),
            "uid": "source-uid", "resourceVersion": "42", "ownerReferences": [{"uid": "source-owner"}],
        }, "spec": spec, "status": {"original": True}}
    pool = obj("InferencePool", "pool", {"selector": {"matchLabels": worker_labels}, "targetPorts": [{"number": 8000}],
        "endpointPickerRef": {"name": revision + "-epp", "failureMode": "FailClose"}})
    config = obj("ConfigMap", "epp-config", {})
    config["data"] = {"endpoint-picker-config.yaml": "original-prefix-config"}
    epp = obj("Deployment", "epp", {"selector": {"matchLabels": dict(labels)}, "template": {
        "metadata": {"labels": dict(labels)}, "spec": {
            "serviceAccountName": revision + "-epp", "imagePullSecrets": [{"name": "private-registry"}],
            "containers": [{"name": "endpoint-picker", "image": "native-epp@sha256:123", "args": ["--pool-name=" + revision + "-pool", "--model-server-metrics-port=9000"]}],
            "volumes": [{"name": "config", "configMap": {"name": revision + "-epp-config"}}],
        },
    }})
    service = obj("Service", "epp", {"clusterIP": "10.43.1.1", "clusterIPs": ["10.43.1.1"],
        "selector": dict(labels), "ports": [{"port": 9002, "targetPort": 9002}]})
    role = obj("Role", "epp", {})
    role["rules"] = [{"apiGroups": [""], "resources": ["pods"], "verbs": ["get", "list", "watch"]}]
    binding = obj("RoleBinding", "epp", {})
    binding["subjects"] = [{"kind": "ServiceAccount", "name": revision + "-epp", "namespace": "tenant-test"}]
    binding["roleRef"] = {"kind": "Role", "name": revision + "-epp"}
    policy = obj("NetworkPolicy", "runtime", {"podSelector": {"matchLabels": worker_labels}, "policyTypes": ["Ingress"]})
    runtime = obj("Deployment", "vllm", {"template": {"spec": {"containers": [{"image": "real-gpu-runtime", "resources": {"limits": {"nvidia.com/gpu": 4}}}], "volumes": [{"hostPath": {"path": "/models"}}]}}})
    runtime["metadata"]["labels"] = worker_labels
    return [config, epp, service, pool, role, binding, policy, runtime, obj("ServiceAccount", "epp", {}),
            *[obj("InferenceObjective", suffix, {"priority": priority, "poolRef": {"name": revision + "-pool"}})
              for priority, suffix in ((100, "interactive"), (0, "standard"), (-10, "batch"))]]


class GatewayConformanceOfflineTests(unittest.TestCase):
    def test_origin_and_request_budget_stay_local_and_bounded(self):
        self.assertEqual(local_origin("http://127.0.0.1:18080/"), "http://127.0.0.1:18080")
        self.assertEqual(local_origin("https://[::1]:8443"), "https://[::1]:8443")
        for origin in ("https://example.com", "http://user@localhost", "http://localhost/path", "http://localhost?token=secret", " http://localhost", "http://@localhost"):
            with self.subTest(origin=origin), self.assertRaises((RuntimeError, ValueError)):
                local_origin(origin)
        for bad in ("0", "1001", "-1", "1.5"):
            with patch.dict(os.environ, {"TEST_BUDGET": bad}), self.assertRaises(RuntimeError):
                bounded("TEST_BUDGET", 100, 20, 1000)

    def test_worker_clones_have_no_gpu_or_source_ownership(self):
        original = source_resources()
        preserved = copy.deepcopy(original)
        all_names = set()
        for identity in ("stable", "candidate"):
            target = "gc-123456789abc-" + identity
            objects = revision_fixture(original, "source-revision", target, identity, "tenant-test", "chat", "fake-runtime:conformance", "123456789abc")
            by_name = {(item["kind"], item["metadata"]["name"]): item for item in objects}
            for item in objects:
                metadata = item["metadata"]
                self.assertEqual(metadata["labels"][MANAGED], "inferscale-conformance")
                self.assertEqual(metadata["labels"][RUN], "123456789abc")
                self.assertNotIn("uid", metadata)
                self.assertNotIn("ownerReferences", metadata)
                self.assertNotIn("resourceVersion", metadata)
                self.assertNotIn("status", item)
                self.assertTrue(metadata["name"].startswith(target + "-"))
                self.assertNotIn((item["kind"], metadata["name"]), all_names)
                all_names.add((item["kind"], metadata["name"]))
            worker = by_name[("Deployment", target + "-worker")]
            pod = worker["spec"]["template"]
            container = pod["spec"]["containers"][0]
            self.assertEqual(container["image"], "fake-runtime:conformance")
            self.assertNotIn("nvidia.com/gpu", json.dumps(worker))
            self.assertNotIn("hostPath", json.dumps(worker))
            self.assertNotIn("source-revision", json.dumps(objects))
            self.assertEqual(pod["spec"]["imagePullSecrets"], [{"name": "private-registry"}])
            env = {item["name"]: item["value"] for item in container["env"]}
            self.assertEqual(env["INFERSCALE_FAKE_TEST_CONTROLS"], "true")
            self.assertEqual(env["INFERSCALE_FAKE_TEST_IDENTITY"], identity)
            pool = by_name[("InferencePool", target + "-pool")]
            self.assertTrue(all(pod["metadata"]["labels"].get(key) == value for key, value in pool["spec"]["selector"]["matchLabels"].items()))
            self.assertEqual(pool["spec"]["endpointPickerRef"]["failureMode"], "FailClose")
            self.assertNotIn("clusterIP", by_name[("Service", target + "-epp")]["spec"])
        self.assertEqual(original, preserved)

    def test_native_route_is_specific_and_keeps_revision_objective(self):
        route = route_fixture("gc-run-route", "tenant-test", [{"name": "inferscale", "namespace": "inferscale-gateway"}],
                              "/v1/deployments/test/chat/completions", "random-run", "stable", "candidate", 25, 10)
        rule = route["spec"]["rules"][0]
        self.assertEqual(len(rule["matches"][0]["headers"]), 2)
        self.assertEqual(rule["matches"][0]["path"]["type"], "Exact")
        self.assertEqual([ref["weight"] for ref in rule["backendRefs"]], [75, 25])
        for ref in rule["backendRefs"]:
            self.assertEqual(ref["kind"], "InferencePool")
            objective = ref["filters"][0]["requestHeaderModifier"]["set"][0]["value"]
            self.assertEqual(objective, ref["name"].removesuffix("-pool") + "-standard")
        mirror = next(item for item in rule["filters"] if item["type"] == "RequestMirror")["requestMirror"]
        self.assertEqual(mirror["percent"], 10)
        self.assertEqual(mirror["backendRef"]["kind"], "Service")
        self.assertEqual(mirror["backendRef"]["name"], "candidate-runtime")
        self.assertEqual(rule["filters"][0]["urlRewrite"]["path"]["replaceFullPath"], "/v1/chat/completions")

    def test_cyclic_fixture_keeps_one_pool_and_distinct_worker_ownership(self):
        original = source_resources()
        preserved = copy.deepcopy(original)
        target = "gc-run-round-robin"
        items = round_robin_fixture(original, "source-revision", target, "tenant-test", "chat", "fake-runtime:test", "run")
        pools = [item for item in items if item["kind"] == "InferencePool"]
        deployments = [item for item in items if item["kind"] == "Deployment"]
        self.assertEqual(len(pools), 1)
        self.assertEqual(len(deployments), 3)
        epp = next(item for item in deployments if item["metadata"]["name"] == target + "-epp")
        self.assertEqual(epp["spec"]["replicas"], 1)
        self.assertEqual(pools[0]["spec"]["endpointPickerRef"]["name"], epp["metadata"]["name"])
        workers = [item for item in deployments if item is not epp]
        selectors = [worker["spec"]["selector"]["matchLabels"] for worker in workers]
        self.assertNotEqual(selectors[0], selectors[1])
        for worker in workers:
            self.assertEqual(worker["spec"]["replicas"], 1)
            labels = worker["spec"]["template"]["metadata"]["labels"]
            self.assertEqual(worker["spec"]["selector"]["matchLabels"], labels)
            self.assertTrue(all(labels.get(key) == value for key, value in pools[0]["spec"]["selector"]["matchLabels"].items()))
            self.assertNotIn("nvidia.com/gpu", json.dumps(worker))
            identity = next(entry["value"] for entry in worker["spec"]["template"]["spec"]["containers"][0]["env"] if entry["name"] == "INFERSCALE_FAKE_TEST_IDENTITY")
            self.assertIn(identity, {"rr-a", "rr-b"})
            service = next(item for item in items if item["kind"] == "Service" and item["metadata"]["name"] == target + "-runtime-" + identity)
            self.assertTrue(all(labels.get(key) == value for key, value in service["spec"]["selector"].items()))
        config = json.loads(next(item for item in items if item["kind"] == "ConfigMap")["data"]["endpoint-picker-config.yaml"])
        self.assertIn({"type": "round-robin-picker", "name": "picker"}, config["plugins"])
        self.assertFalse(any(item.get("name") == "routing-scorer" for item in config["plugins"]))
        self.assertEqual(config["schedulingProfiles"][0]["plugins"], [{"pluginRef": "picker"}])
        self.assertEqual(original, preserved)

    def test_balanced_random_order_is_not_cyclic_evidence(self):
        self.assertTrue(cyclic_distribution(["a", "b"] * 10, ("a", "b"))["passed"])
        self.assertTrue(cyclic_distribution(["b", "a"] * 10 + ["b"], ("a", "b"))["passed"])
        for sequence in (["a", "a", "b", "b"], ["a"] * 20, ["a", "b", "a", "unknown"]):
            self.assertFalse(cyclic_distribution(sequence, ("a", "b"))["passed"])
        with self.assertRaises(RuntimeError):
            cyclic_distribution(["a", "b"], ("a", "b"))

    def test_cyclic_evidence_is_bound_to_stable_ready_processes(self):
        pods = [{"metadata": {"name": name, "uid": name + "-uid", "labels": {"app.kubernetes.io/component": component}},
                 "status": {"podIP": "10.0.0." + str(index + 1), "conditions": [{"type": "Ready", "status": "True"}],
                            "containerStatuses": [{"name": name, "image": "candidate", "imageID": "sha256:123", "restartCount": 0}]}}
                for index, (name, component) in enumerate((("epp", "endpoint-picker"), ("rr-a", "model-server"), ("rr-b", "model-server")))]
        before = round_robin_pod_state(pods)
        self.assertIsNotNone(before)
        self.assertEqual(before, round_robin_pod_state(list(reversed(pods))))
        pods[0]["status"]["containerStatuses"][0]["restartCount"] = 1
        self.assertNotEqual(before, round_robin_pod_state(pods))
        pods[0]["status"]["conditions"][0]["status"] = "False"
        self.assertIsNone(round_robin_pod_state(pods))
        self.assertIsNone(round_robin_pod_state(pods[:2]))

    def test_old_or_rejected_route_conditions_never_pass(self):
        value = {"metadata": {"generation": 5}, "status": {"parents": [{"conditions": [
            {"type": key, "status": "True", "observedGeneration": 5} for key in ("Accepted", "ResolvedRefs")
        ]}]}}
        self.assertTrue(route_ready(value))
        value["status"]["parents"][0]["conditions"][0]["observedGeneration"] = 4
        self.assertFalse(route_ready(value))
        value["status"]["parents"][0]["conditions"][0].update(status="False", observedGeneration=5)
        self.assertFalse(route_ready(value))
        self.assertFalse(route_ready({}))

    def test_distribution_rejects_wrong_weights_and_records_tolerance(self):
        self.assertTrue(distribution(50, 1000, 5)["passed"])
        self.assertTrue(distribution(250, 1000, 25)["passed"])
        for candidate, total, percent in ((0, 100, 5), (100, 100, 5), (250, 1000, 50), (1, 100, 0), (999, 1000, 100)):
            self.assertFalse(distribution(candidate, total, percent)["passed"])
        self.assertGreater(distribution(50, 1000, 5)["tolerance_requests"], 0)
        self.assertEqual(distribution(0, 1000, 0)["tolerance_requests"], 0)

    def test_sse_requires_native_model_usage_done_and_rejects_trailing_data(self):
        frame = {"model": "chat", "choices": [{"delta": {"content": "hello"}}]}
        usage = {"choices": [], "usage": {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3}}
        raw = b"".join(("data: " + json.dumps(item) + "\n\n").encode() for item in (frame, usage)) + b"data: [DONE]\n\n"
        self.assertEqual(read_sse(io.BytesIO(raw), "chat")["generated_events"], 1)
        for malformed in (raw.replace(b"[DONE]", b"{}"), raw + raw, b"data: [DONE]\n\n", b"data: []\n\n", raw.replace(b'"chat"', b'"other"'), b"data: " + b"a" * 65537):
            with self.assertRaises((RuntimeError, ValueError)):
                read_sse(io.BytesIO(malformed), "chat")

    def test_native_flow_control_has_strict_burst_bounds(self):
        config = picker_config()
        saturation = next(item for item in config["plugins"] if item.get("name") == "saturation")
        self.assertEqual(saturation["parameters"]["maxConcurrency"], 1)
        self.assertEqual(config["flowControl"]["maxRequests"], "2")
        self.assertEqual(config["flowControl"]["defaultRequestTTL"], "30s")
        self.assertFalse(any("prefix" in item["type"] for item in config["plugins"]))
        self.assertIn({"type": "max-score-picker", "name": "picker"}, config["plugins"])


if __name__ == "__main__":
    unittest.main()
