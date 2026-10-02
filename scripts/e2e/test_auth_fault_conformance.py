"""Offline safety and evidence tests; never invokes Kubernetes or an operator."""

import base64
import copy
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch
import uuid

import auth_fault_conformance as faults
import gateway_conformance as gateway


def issued_token():
    return "isk_" + str(uuid.uuid4()) + "_" + base64.urlsafe_b64encode(bytes(range(32))).decode().rstrip("=")


def metrics(completed=1, inflight=0, started=100):
    return ("# TYPE llm_d_epp_extproc_stream_duration_seconds histogram\n"
            f"llm_d_epp_extproc_stream_duration_seconds_count {completed}\n"
            f"llm_d_epp_extproc_streams_inflight {inflight}\n"
            f"process_start_time_seconds {started}\n")


def bare_harness():
    h = object.__new__(faults.Harness)
    h.result = {"passed": True, "checks": []}
    h.stage = "test"
    h.run_id = "test"
    h.revisions = {"stable": "gc-test-stable", "candidate": "gc-test-candidate"}
    h.owned_token = issued_token()
    h.owned_key = h.owned_token.split("_", 2)[1]
    h.issue_attempted = True
    h.tenant_id = str(uuid.uuid4())
    h.revoked = False
    h.restore_valkey = None
    h.metrics_origins = {}
    h.operator_env = {"PRIVATE": "do-not-print"}
    h.kube = Mock()
    h.operator = Mock()
    h.wait = Mock(side_effect=lambda _name, predicate, *args: faults.check(predicate(), "test wait failed"))
    h.passed = Mock()
    return h


class AuthFaultTests(unittest.TestCase):
    def test_cluster_guard_requires_selected_local_context_without_proxy(self):
        config = {"current-context": "k3d-inferscale-dev", "clusters": [{"cluster": {"server": "https://127.0.0.1:6443"}}]}
        faults.local_cluster("k3d-inferscale-dev", config)
        for server in ("https://example.com", "https://127.0.0.1.evil.invalid", "https://user@127.0.0.1:6443", "https://127.0.0.1/path"):
            bad = copy.deepcopy(config)
            bad["clusters"][0]["cluster"]["server"] = server
            with self.subTest(server=server), self.assertRaises(RuntimeError):
                faults.local_cluster("k3d-inferscale-dev", bad)
        for change in ({"current-context": "production"}, {"clusters": []}):
            with self.assertRaises(RuntimeError):
                faults.local_cluster("k3d-inferscale-dev", {**config, **change})
        with self.assertRaises(RuntimeError):
            faults.local_cluster("production", config)
        config["clusters"][0]["cluster"]["proxy-url"] = "http://127.0.0.1:3128"
        with self.assertRaises(RuntimeError):
            faults.local_cluster("k3d-inferscale-dev", config)

    def test_invalid_credential_has_existing_id_and_different_real_secret(self):
        original = issued_token()
        changed = faults.wrong_secret(original)
        self.assertEqual(changed.split("_", 2)[:2], original.split("_", 2)[:2])
        self.assertEqual(len(base64.urlsafe_b64decode(changed.split("_", 2)[2] + "=")), 32)
        self.assertNotEqual(changed, original)
        for invalid in ("invalid-conformance-key", "isk_bad_id", original[:-5]):
            with self.assertRaises((RuntimeError, ValueError)):
                faults.wrong_secret(invalid)

    def test_metrics_reject_missing_nonfinite_duplicate_fractional_and_active_streams(self):
        self.assertEqual(faults.stream_counters(metrics()), {"completed": 1, "inflight": 0, "process_start": 100})
        for raw in ("", metrics().replace("count 1", "count NaN"), metrics(completed=0.5),
                    metrics(started=0), metrics() + "llm_d_epp_extproc_streams_inflight 0\n"):
            with self.subTest(raw=raw), self.assertRaises(RuntimeError):
                faults.stream_counters(raw)
        before = {"stable": faults.stream_counters(metrics())}
        for raw in (metrics(completed=2), metrics(inflight=1), metrics(started=101)):
            with self.assertRaises(RuntimeError):
                faults.unchanged(before, {"stable": faults.stream_counters(raw)})

    def test_database_is_local_forward_and_query_cannot_override_host(self):
        secret = {"data": {"database_url": base64.b64encode(
            b"postgres://user:private%40secret@postgres.inferscale-system.svc:5432/inferscale?hostaddr=203.0.113.1").decode()}}
        value = faults.database_url(secret, "http://127.0.0.1:25432")
        self.assertEqual(value, "postgresql://user:private%40secret@127.0.0.1:25432/inferscale?sslmode=disable")
        for url in (b"postgres://user:secret@remote.example/inferscale", b"postgres://user:secret@postgres/other"):
            secret["data"]["database_url"] = base64.b64encode(url).decode()
            with self.assertRaises(RuntimeError):
                faults.database_url(secret, "http://127.0.0.1:25432")

    def test_rejection_requires_epp_and_worker_unchanged_with_exact_status(self):
        for status, completed, runtime_delta in ((401, 1, 0), (200, 1, 0), (401, 2, 0), (401, 1, 1)):
            h = bare_harness()
            before = {role: faults.stream_counters(metrics()) for role in h.revisions}
            after = copy.deepcopy(before)
            after["stable"]["completed"] = completed
            h.idle_metrics = Mock(return_value=before)
            h.metrics = Mock(return_value=after)
            h.snapshot = Mock(side_effect=[{role: {"requests": 2} for role in h.revisions},
                                          {role: {"requests": 2 + runtime_delta} for role in h.revisions}])
            h.owned_chat = Mock(return_value=(status, {}, {}))
            with patch.object(faults.time, "sleep"):
                if (status, completed, runtime_delta) == (401, 1, 0):
                    h.rejected("test", h.owned_token, 401)
                    h.passed.assert_called_once()
                else:
                    with self.assertRaises(RuntimeError):
                        h.rejected("test", h.owned_token, 401)

    def test_key_ownership_recorded_before_remaining_validation_and_cleaned(self):
        h = bare_harness()
        h.owned_key = None
        key_id = str(uuid.uuid4())
        h.operator.return_value = {"id": key_id, "scopes": ["deployments:write"], "apiKey": "private"}
        with self.assertRaises(RuntimeError):
            h.issue_key()
        self.assertEqual(h.owned_key, key_id)
        h.revoke_key()
        self.assertTrue(h.result["credential_cleanup"])
        self.assertEqual(h.operator.call_args.args, ("revoke", key_id, "--tenant", h.tenant_id))

    def test_ambiguous_issuance_does_not_claim_credential_cleanup(self):
        h = bare_harness()
        h.owned_key = None
        h.operator.side_effect = subprocess.TimeoutExpired("operator", 30)
        with self.assertRaises(subprocess.TimeoutExpired):
            h.issue_key()
        with self.assertRaises(RuntimeError):
            h.revoke_key()
        self.assertFalse(h.result["credential_cleanup"])

    def test_outage_arms_restore_before_ambiguous_scale_and_always_restores(self):
        for error in (RuntimeError("rejected test"), KeyboardInterrupt()):
            h = bare_harness()
            deployment = {"metadata": {"uid": "original"}, "spec": {"replicas": 2,
                          "selector": {"matchLabels": {"app.kubernetes.io/name": "valkey"}}}}
            h.kube.side_effect = [deployment, {"items": []}]
            def ambiguous_scale(*_args):
                self.assertEqual(h.restore_valkey, {"uid": "original", "replicas": 2})
                raise error
            h.replica_patch = Mock(side_effect=ambiguous_scale)
            h.restore_dependency = Mock()
            with self.assertRaises(type(error)):
                h.outage()
            h.restore_dependency.assert_called_once()

    def test_restore_exact_original_replicas_and_reject_concurrent_identity_change(self):
        h = bare_harness()
        h.restore_valkey = {"uid": "original", "replicas": 2}
        h.kube.side_effect = [{"metadata": {"uid": "original"}, "spec": {"replicas": 0}},
                              {"status": {"availableReplicas": 2}}]
        h.replica_patch = Mock()
        h.restore_dependency()
        h.replica_patch.assert_called_once_with("original", 0, 2)
        self.assertIsNone(h.restore_valkey)
        self.assertTrue(h.result["dependency_restored"])
        for current in ({"metadata": {"uid": "replacement"}, "spec": {"replicas": 0}},
                        {"metadata": {"uid": "original"}, "spec": {"replicas": 3}}):
            h.restore_valkey = {"uid": "original", "replicas": 2}
            h.kube.side_effect = None
            h.kube.return_value = current
            h.replica_patch.reset_mock()
            with self.assertRaises(RuntimeError):
                h.restore_dependency()
            h.replica_patch.assert_not_called()

    def test_patch_has_identity_and_replica_preconditions(self):
        h = bare_harness()
        h.replica_patch("original", 2, 0)
        args = h.kube.call_args.args
        self.assertEqual(args[:6], ("-n", "inferscale-system", "patch", "deployment", "valkey", "--type=json"))
        self.assertEqual(json.loads(args[-1]), [
            {"op": "test", "path": "/metadata/uid", "value": "original"},
            {"op": "test", "path": "/spec/replicas", "value": 2},
            {"op": "replace", "path": "/spec/replicas", "value": 0}])

    def test_cleanup_continues_after_restore_failure_and_redacts_failure(self):
        h = bare_harness()
        h.restore_dependency = Mock(side_effect=RuntimeError("private-dependency-value"))
        with patch.object(gateway.Harness, "cleanup") as fixture_cleanup:
            h.cleanup()
        self.assertTrue(h.revoked)
        self.assertFalse(h.result["passed"])
        self.assertEqual(h.result["fault_cleanup_failures"], ["dependency"])
        self.assertNotIn("private", json.dumps(h.result))
        self.assertIsNone(h.owned_token)
        self.assertIsNone(h.operator_env)
        fixture_cleanup.assert_called_once()

    def test_cli_capture_never_puts_tokens_in_arguments_or_error_messages(self):
        h = bare_harness()
        h.cli = Path("/private/inferscalectl")
        # Use the real method because the bare harness mocks command execution.
        del h.operator
        with patch.object(faults.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "secret-token", "database-secret")) as execute:
            with self.assertRaisesRegex(RuntimeError, "^credential operator command failed$"):
                h.operator("issue", "--tenant", h.tenant_id)
        self.assertNotIn("secret", json.dumps(execute.call_args.args))
        self.assertTrue(execute.call_args.kwargs["capture_output"])

    def test_finally_revokes_key_and_writes_only_private_redacted_summary(self):
        h = bare_harness()
        token = h.owned_token
        h.run = Mock(side_effect=ValueError("sensitive-response-" + token))
        with tempfile.TemporaryDirectory() as destination, patch.dict(os.environ, {"INFERSCALE_AUTH_FAULT_RESULTS_DIR": destination}), \
                patch.object(faults, "Harness", return_value=h), patch.object(faults.signal, "signal"), \
                patch.object(gateway.Harness, "cleanup") as fixtures, \
                patch.object(faults.sys, "stdout", new_callable=io.StringIO) as stdout, \
                patch.object(faults.sys, "stderr", new_callable=io.StringIO) as stderr:
            old_umask = os.umask(0o077)
            try:
                self.assertEqual(faults.main(), 1)
            finally:
                os.umask(old_umask)
            summary = Path(destination) / h.run_id / "summary.json"
            retained = summary.read_text()
            self.assertEqual(summary.stat().st_mode & 0o777, 0o600)
            self.assertEqual(summary.parent.stat().st_mode & 0o777, 0o700)
            self.assertNotIn(token, retained + stdout.getvalue() + stderr.getvalue())
            self.assertNotIn("sensitive-response", retained + stdout.getvalue() + stderr.getvalue())
            self.assertEqual(json.loads(retained)["error_type"], "ValueError")
        self.assertTrue(h.revoked)
        fixtures.assert_called_once()

    def test_revocation_is_measured_after_operator_acknowledgement(self):
        h = bare_harness()
        h.context = "k3d-inferscale-dev"
        h.deployment_id = str(uuid.uuid4())
        h.namespace = "tenant-test"
        h.request = Mock(return_value=(200, {}, {"tenantId": h.tenant_id}))
        h.forward = Mock(return_value="http://127.0.0.1:15432")
        h.issue_key = Mock()
        h.prepare = Mock()
        h.configure_route = Mock()
        before = {role: faults.stream_counters(metrics()) for role in h.revisions}
        after = copy.deepcopy(before)
        after["stable"]["completed"] += 1
        h.idle_metrics = Mock(side_effect=[before, after])
        h.identity = Mock()
        h.owned_chat = Mock()
        h.outage = Mock()
        calls = []
        h.revoke_key = Mock(side_effect=lambda: calls.append("revoke-ack"))
        h.rejected = Mock(side_effect=lambda name, *args: calls.append("rejected-" + name))
        with patch.object(faults, "local_cluster"), patch.object(faults, "database_url", return_value="private"), \
                patch.object(faults.time, "sleep", side_effect=lambda seconds: calls.append(("wait", seconds))):
            h.run()
        self.assertEqual(calls[-3:], ["revoke-ack", ("wait", 20), "rejected-revoked issued credential rejected before EPP"])
        self.assertTrue(h.result["passed"])


if __name__ == "__main__":
    unittest.main()
