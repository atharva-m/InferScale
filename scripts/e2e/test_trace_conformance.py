"""Offline trace protocol, privacy, and harness tests; no network or GPU use."""

import base64
import contextlib
import copy
import io
import json
import os
from pathlib import Path
import stat
import tempfile
import unittest
from unittest.mock import Mock, patch
import urllib.error
import urllib.request

import trace_conformance as trace


TRACE_ID = "1234567890abcdef1234567890abcdef"
CALLER_ID = "1234567890abcdef"
DEPLOYMENT_ID = "12345678-1234-1234-1234-123456789abc"
TOKEN = "synthetic-test-api-key-DO-NOT-EXPORT"
OUTPUT = "A synthetic completion that must never appear in trace artifacts."


def attribute(key, value):
    return {"key": key, "value": {"stringValue": value}}


def document(trace_id=TRACE_ID, caller_id=CALLER_ID, legacy=False):
    """Four native service spans; admission/EPP/runtime descend from Gateway."""
    batches = []
    parent = caller_id
    for index, service in enumerate(trace.DEFAULT_SERVICES.values(), 1):
        span_id = f"{index:016x}"
        span = {"traceId": trace_id, "spanId": span_id, "parentSpanId": parent, "name": "HTTP POST",
                "attributes": [{"key": "gen_ai.usage.prompt_tokens", "value": {"intValue": "16"}}]}
        if legacy:
            for key in ("traceId", "spanId", "parentSpanId"):
                span[key] = base64.b64encode(bytes.fromhex(span[key])).decode()
        batches.append({"resource": {"attributes": [attribute("service.name", service)]},
                        "instrumentationLibrarySpans" if legacy else "scopeSpans": [{
                            "instrumentationLibrary" if legacy else "scope": {"name": "native-test"},
                            "spans": [span],
                        }]})
        if index == 1:
            parent = span_id
    return {"batches" if legacy else "resourceSpans": batches}


def all_spans(value):
    return [row["span"] for row in trace.spans(value)]


def evaluate(value, needles=(TOKEN, OUTPUT)):
    return trace.assess(value, TRACE_ID, CALLER_ID, trace.DEFAULT_SERVICES, needles)


class TraceProtocolTests(unittest.TestCase):
    def test_current_and_legacy_tempo_formats_correlate_to_the_caller(self):
        for legacy in (False, True):
            with self.subTest(legacy=legacy):
                result = evaluate(document(legacy=legacy))
                self.assertTrue(result["passed"])
                self.assertEqual(result["component_spans"], dict.fromkeys(trace.DEFAULT_SERVICES, 1))
                self.assertEqual(result["span_count"], 4)

    def test_missing_service_or_shared_trace_without_parentage_cannot_pass(self):
        value = document()
        value["resourceSpans"].pop()
        self.assertEqual(evaluate(value)["missing_components"], ["runtime"])
        for parent in (None, "eeeeeeeeeeeeeeee", "0000000000000004"):
            value = document()
            span = all_spans(value)[-1]
            if parent is None:
                del span["parentSpanId"]
            else:
                span["parentSpanId"] = parent
            with self.subTest(parent=parent):
                result = evaluate(value)
                self.assertFalse(result["passed"])
                self.assertEqual(result["disconnected_components"], ["runtime"])

    def test_extra_disconnected_span_in_an_existing_component_fails(self):
        value = document()
        extra = copy.deepcopy(all_spans(value)[-1])
        extra.update(spanId="ffffffffffffffff", parentSpanId="eeeeeeeeeeeeeeee")
        value["resourceSpans"][-1]["scopeSpans"][0]["spans"].append(extra)
        self.assertEqual(evaluate(value)["disconnected_components"], ["runtime"])

    def test_wrong_trace_zero_ids_and_reused_caller_fail_closed(self):
        for key, bad in (("traceId", "f" * 32), ("traceId", "0" * 32), ("spanId", "0" * 16),
                         ("spanId", CALLER_ID), ("parentSpanId", "0" * 16), ("spanId", "invalid")):
            value = document()
            all_spans(value)[-1][key] = bad
            with self.subTest(key=key, bad=bad), self.assertRaises(trace.CheckError):
                evaluate(value)

    def test_cycle_and_long_parent_chain_are_handled_without_recursion(self):
        value = document()
        rows = all_spans(value)
        rows[1]["parentSpanId"] = rows[2]["spanId"]
        rows[2]["parentSpanId"] = rows[1]["spanId"]
        self.assertEqual(evaluate(value)["disconnected_components"], ["admission", "epp"])
        value = document()
        runtime = value["resourceSpans"][-1]["scopeSpans"][0]["spans"]
        parent = runtime[0]["spanId"]
        for index in range(10, 2010):
            runtime.append({"traceId": TRACE_ID, "spanId": f"{index:016x}", "parentSpanId": parent})
            parent = f"{index:016x}"
        self.assertTrue(evaluate(value)["passed"])

    def test_exact_duplicate_spans_are_deduplicated_but_conflicts_fail(self):
        value = document()
        group = value["resourceSpans"][-1]["scopeSpans"][0]
        duplicate = copy.deepcopy(group["spans"][0])
        group["spans"].append(duplicate)
        self.assertEqual(evaluate(value)["span_count"], 4)
        for key, bad in (("name", "different operation"), ("parentSpanId", "f" * 16),
                         ("attributes", [attribute("safe", "different")])):
            original = duplicate[key]
            duplicate[key] = bad
            with self.subTest(key=key), self.assertRaises(trace.CheckError):
                evaluate(value)
            duplicate[key] = original

    def test_malformed_resource_scope_span_and_duplicate_service_fail(self):
        invalid = [None, [], {}, {"resourceSpans": {}}, {"resourceSpans": [None]},
                   {"resourceSpans": [{"resource": []}]},
                   {"resourceSpans": [{"resource": {"attributes": [None]}}]},
                   {"resourceSpans": [{"scopeSpans": [None]}]},
                   {"resourceSpans": [{"scopeSpans": [{"scope": []}]}]},
                   {"resourceSpans": [{"scopeSpans": [{"spans": {}}]}]},
                   {"resourceSpans": [{"scopeSpans": [{"spans": [None]}]}]}]
        duplicate_service = document()
        duplicate_service["resourceSpans"][0]["resource"]["attributes"].append(attribute("service.name", "other"))
        invalid.append(duplicate_service)
        for value in invalid:
            with self.subTest(value=value), self.assertRaises(trace.CheckError):
                evaluate(value)

    def test_duplicate_json_keys_and_nonfinite_numbers_cannot_hide_exports(self):
        for raw in ('{"attributes": "secret", "attributes": []}', '{"nested": {"key": 1, "key": 2}}',
                    '{"value": NaN}', '{"value": Infinity}', 'not JSON', b'"\xff"'):
            with self.subTest(raw=raw), self.assertRaises(trace.CheckError):
                trace.parse_json(raw)


class TracePrivacyTests(unittest.TestCase):
    def test_attribute_denylist_covers_protocol_spellings(self):
        keys = ("http.request.header.authorization", "authorization", "headers.cookie", "baggage",
                "x-api-key", "apiKey", "APIKEY", "access.token", "refresh_token", "id-token",
                "gen_ai.prompt", "gen_ai.completion", "http.request.body", "requestBody",
                "gen_ai.input.messages", "response.payload", "content", "password", "secret")
        for key in keys:
            value = document()
            all_spans(value)[0]["attributes"].append(attribute(key, "not-a-known-needle"))
            with self.subTest(key=key):
                result = evaluate(value)
                self.assertFalse(result["passed"])
                self.assertTrue(result["correlated"])
                self.assertFalse(result["privacy_passed"])

    def test_numeric_operational_metadata_passes_but_text_and_nested_values_fail(self):
        for key in ("gen_ai.usage.prompt_tokens", "gen_ai.usage.completion_tokens", "http.request.body.size"):
            for value in ({"intValue": "42"}, {"doubleValue": 42.5}):
                with self.subTest(key=key, value=value):
                    self.assertFalse(trace.sensitive_attribute(key, value))
            for value in ({"stringValue": "42"}, {"intValue": "secret"}, {"intValue": True},
                          {"doubleValue": float("nan")}, {"doubleValue": float("inf")},
                          {"intValue": "42", "stringValue": "secret"},
                          {"arrayValue": {"values": [{"stringValue": "secret"}]}}):
                with self.subTest(key=key, value=value):
                    self.assertTrue(trace.sensitive_attribute(key, value))

    def test_resource_scope_span_event_link_status_and_unknown_fields_are_scanned(self):
        for location in ("resource", "scope", "span", "event", "link", "status", "unknown"):
            value = document()
            batch = value["resourceSpans"][0]
            group = batch["scopeSpans"][0]
            span = group["spans"][0]
            leak = attribute("otherwise.safe", "prefix-" + TOKEN + "-suffix")
            if location == "resource":
                batch["resource"]["attributes"].append(leak)
            elif location == "scope":
                group["scope"]["attributes"] = [leak]
            elif location == "span":
                span["attributes"].append(leak)
            elif location in {"event", "link"}:
                span[location + "s"] = [{"attributes": [leak]}]
            elif location == "status":
                span["status"] = {"message": TOKEN}
            else:
                value["unexpected"] = {"nested": [TOKEN]}
            with self.subTest(location=location):
                self.assertFalse(evaluate(value)["privacy_passed"])

    def test_completion_prompt_credentials_and_encoded_values_are_detected(self):
        for raw in (TOKEN, OUTPUT, "arbitrary Bearer an-unrecognized-key", "input-unique-nonce"):
            for encoded in (False, True):
                value = document()
                content = {"bytesValue": base64.b64encode(raw.encode()).decode()} if encoded else {"stringValue": raw}
                all_spans(value)[0]["attributes"].append({"key": "otherwise.safe", "value": content})
                with self.subTest(raw=raw, encoded=encoded):
                    result = evaluate(value, (TOKEN, OUTPUT, "input-unique-nonce"))
                    self.assertFalse(result["privacy_passed"])
                    self.assertNotIn(raw, json.dumps(result))

    def test_invalid_base64_and_empty_content_events_fail_closed(self):
        for event in ("gen_ai.user.message", "gen_ai.assistant.message", "gen_ai.system.message",
                      "gen_ai.tool.message", "gen_ai.prompt", "gen_ai.completion"):
            value = document()
            all_spans(value)[0]["events"] = [{"name": event}]
            with self.subTest(event=event):
                self.assertFalse(evaluate(value)["privacy_passed"])
        self.assertGreater(trace.privacy_findings({"bytesValue": "invalid%%%"}, ())[1], 0)

    def test_nested_arrays_and_malicious_keys_never_enter_redacted_results(self):
        value = document()
        all_spans(value)[0]["attributes"].append({"key": "authorization-" + TOKEN, "value": {
            "kvlistValue": {"values": [{"key": TOKEN, "value": {"arrayValue": {
                "values": [{"stringValue": OUTPUT}]
            }}}]}
        }})
        result = evaluate(value)
        self.assertFalse(result["privacy_passed"])
        serialized = json.dumps(result)
        self.assertNotIn(TOKEN, serialized)
        self.assertNotIn(OUTPUT, serialized)
        for finding in result["privacy_findings"]:
            self.assertEqual(set(finding), {"node", "kind"})

    def test_findings_are_capped_without_losing_total_failure_count(self):
        findings, count = trace.privacy_findings([TOKEN] * 150, (TOKEN,))
        self.assertEqual(len(findings), 100)
        self.assertEqual(count, 150)
        with self.assertRaises(trace.CheckError):
            trace.privacy_findings([None] * 200001, ())


class TraceBoundaryTests(unittest.TestCase):
    def test_only_canonical_loopback_origins_are_accepted(self):
        for value, expected in (("http://127.0.0.1:8080/", "http://127.0.0.1:8080"),
                                ("http://localhost", "http://127.0.0.1:80"),
                                ("https://[0:0:0:0:0:0:0:1]", "https://[::1]:443")):
            self.assertEqual(trace.local_origin(value), expected)
        for value in (None, "", "https://example.com", "http://127.0.0.1.evil", "http://127.1", "file:///tmp/key",
                      "http://0.0.0.0", "http://[::]", "http://user@localhost", "http://@localhost",
                      "http://localhost/path", "http://localhost?token=secret", "http://localhost#fragment",
                      " http://localhost", "http://local\thost", "http://localhost:0", "http://localhost:65536",
                      "http://localhost:-1", "http://[::1%lo]"):
            with self.subTest(value=value), self.assertRaises(trace.CheckError):
                trace.local_origin(value)

    def test_auth_file_must_be_private_regular_bounded_and_not_a_symlink(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "auth.json"
            path.write_text(json.dumps({"api_key": TOKEN}))
            path.chmod(0o600)
            self.assertEqual(trace.private_auth(path), {"api_key": TOKEN})
            path.chmod(0o640)
            with self.assertRaises(trace.CheckError):
                trace.private_auth(path)
            path.chmod(0o600)
            link = Path(temporary) / "symlink"
            link.symlink_to(path)
            with self.assertRaises(OSError):
                trace.private_auth(link)
            fifo = Path(temporary) / "fifo"
            os.mkfifo(fifo, 0o600)
            with self.assertRaises(trace.CheckError):
                trace.private_auth(fifo)
            path.write_bytes(b"x" * 65537)
            with self.assertRaises(trace.CheckError):
                trace.private_auth(path)

    def test_http_has_no_proxy_or_redirect_and_does_not_read_error_bodies(self):
        with patch.object(urllib.request, "build_opener") as builder:
            client = trace.LocalHTTP()
            self.assertEqual(builder.call_args.args[0].proxies, {})
            redirect = builder.call_args.args[1]
            self.assertIsNone(redirect.redirect_request(None, None, 302, "", {}, "https://external.invalid"))
            error_body = Mock()
            client.opener.open.side_effect = urllib.error.HTTPError("http://localhost", 302, TOKEN, {}, error_body)
            self.assertEqual(client.json("http://localhost", "/test", token=TOKEN), (302, None))
            error_body.read.assert_not_called()
            error_body.close.assert_called_once()

    def test_http_revalidates_origin_and_bounds_json_without_network(self):
        client = trace.LocalHTTP()
        client.opener = Mock()
        for origin, path in (("http://example.com", "/"), ("http://localhost", "//example.com"),
                             ("http://localhost", "/path\r\nHeader: injected"), ("http://localhost", "/#fragment")):
            with self.subTest(origin=origin, path=path), self.assertRaises(trace.CheckError):
                client.json(origin, path, token=TOKEN)
        client.opener.open.assert_not_called()
        for raw in (b"x" * (trace.MAX_JSON + 1), b"not JSON", b'{"a": 1, "a": 2}'):
            response = io.BytesIO(raw)
            response.status = 200
            client.opener.open.return_value = response
            with self.subTest(raw_length=len(raw)), self.assertRaises(trace.CheckError):
                client.json("http://localhost", "/test")

    def test_time_bounds_are_strict(self):
        for value in ("0", "19", "301", "-1", "1.5", " 30", "NaN"):
            with self.subTest(value=value), patch.dict(os.environ, {"TEST_BOUND": value}), self.assertRaises(trace.CheckError):
                trace.bounded("TEST_BOUND", 90, 20, 300)


class Clock:
    now = 0

    def monotonic(self):
        return self.now

    def sleep(self, seconds):
        self.now += seconds


class FakeHTTP:
    def __init__(self, harness, poll=None, deployment=None, completion=None):
        self.harness, self.poll = harness, poll
        self.calls, self.polls = [], 0
        self.deployment = deployment or {"name": "served-model", "spec": {"observability": {"tracing": True}},
                                         "status": {"runtime": {"backend": "vllm"}, "cache": {"weights": "Warm"}}}
        self.completion = completion or {"model": "served-model", "choices": [{"message": {"content": OUTPUT}}]}

    def json(self, origin, path, **kwargs):
        self.calls.append((origin, path, kwargs))
        if path.startswith("/api/traces/"):
            self.polls += 1
            value = document(self.harness.trace_id, self.harness.caller_id)
            return self.poll(self.polls, value) if self.poll else (200, value)
        if path.endswith("/chat/completions"):
            return 200, self.completion
        return 200, self.deployment


class TraceHarnessTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.directory = Path(temporary.name)
        auth = self.directory / "auth.json"
        auth.write_text(json.dumps({"base_url": "http://127.0.0.1:8080", "api_key": TOKEN,
                                    "deployment_id": DEPLOYMENT_ID}))
        auth.chmod(0o600)
        environment = {"INFERSCALE_TRACE_AUTH_FILE": str(auth), "INFERSCALE_TRACE_TEMPO_URL": "http://127.0.0.1:3200",
                       "INFERSCALE_TRACE_WAIT_SECONDS": "20", "INFERSCALE_TRACE_SETTLE_SECONDS": "5",
                       "INFERSCALE_TRACE_ARTIFACT_DIR": str(self.directory / "results")}
        self.stack = contextlib.ExitStack()
        self.addCleanup(self.stack.close)
        self.stack.enter_context(patch.dict(os.environ, environment, clear=True))
        self.clock = Clock()
        self.stack.enter_context(patch.object(trace.time, "monotonic", self.clock.monotonic))
        self.stack.enter_context(patch.object(trace.time, "sleep", self.clock.sleep))

    def test_one_inference_preserves_w3c_caller_and_tempo_never_gets_auth(self):
        harness = trace.Harness()
        harness.http = FakeHTTP(harness)
        harness.run()
        self.assertTrue(harness.result["passed"])
        self.assertFalse(harness.result["release_signoff"])
        self.assertEqual(harness.result["inference_requests"], 1)
        inference = [call for call in harness.http.calls if call[1].endswith("/chat/completions")]
        self.assertEqual(len(inference), 1)
        headers = inference[0][2]["headers"]
        self.assertEqual(headers["traceparent"], f"00-{harness.trace_id}-{harness.caller_id}-01")
        self.assertFalse(inference[0][2]["payload"]["stream"])
        self.assertEqual(harness.http.polls, 4)
        for origin, path, kwargs in harness.http.calls:
            if origin == harness.tempo:
                self.assertEqual(path, "/api/traces/" + harness.trace_id)
                self.assertNotIn("token", kwargs)
                self.assertNotIn("headers", kwargs)
        artifact = harness.save()
        self.assertEqual(stat.S_IMODE(artifact.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(artifact.parent.stat().st_mode), 0o700)
        self.assertEqual(json.loads(artifact.read_text()), harness.result)
        for secret in (TOKEN, OUTPUT, inference[0][2]["payload"]["messages"][0]["content"]):
            self.assertNotIn(secret, artifact.read_text())
        with self.assertRaises(FileExistsError):
            harness.save()

    def test_partial_or_missing_exports_reset_the_settle_window(self):
        def poll(number, value):
            if number == 2:
                return 404, None
            if number == 4:
                value["resourceSpans"].pop()
            return 200, value
        harness = trace.Harness()
        harness.http = FakeHTTP(harness, poll=poll)
        harness.run()
        self.assertEqual(harness.http.polls, 8)
        self.assertEqual(self.clock.now, 14)

    def test_missing_native_component_never_passes_and_wait_is_bounded(self):
        def poll(number, value):
            value["resourceSpans"].pop()
            return 200, value
        harness = trace.Harness()
        harness.http = FakeHTTP(harness, poll=poll)
        with self.assertRaises(trace.CheckError):
            harness.run()
        self.assertFalse(harness.result["passed"])
        self.assertEqual(harness.result["trace"]["missing_components"], ["runtime"])
        self.assertEqual(self.clock.now, 20)
        self.assertEqual(harness.http.polls, 10)

    def test_late_sensitive_export_aborts_before_success_and_artifact_is_redacted(self):
        def poll(number, value):
            if number == 3:
                all_spans(value)[-1]["events"] = [{"name": "diagnostic", "attributes": [attribute("safe", TOKEN)]}]
            return 200, value
        harness = trace.Harness()
        harness.http = FakeHTTP(harness, poll=poll)
        stdout, stderr = io.StringIO(), io.StringIO()
        with patch.object(trace, "Harness", return_value=harness), contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            self.assertEqual(trace.main(), 1)
        self.assertFalse(harness.result["passed"])
        self.assertEqual(harness.result["failed_stage"], "trace_export")
        self.assertEqual(harness.http.polls, 3)
        artifacts = list((self.directory / "results").glob("*.json"))
        self.assertEqual(len(artifacts), 1)
        report = stdout.getvalue() + stderr.getvalue() + artifacts[0].read_text()
        self.assertNotIn(TOKEN, report)
        self.assertNotIn(OUTPUT, report)

    def test_prompt_and_response_tail_fragments_are_in_the_live_privacy_probe(self):
        for source in ("input_nonce", "output_nonce", "completion_tail"):
            harness = trace.Harness()
            def poll(number, value):
                inference = next(call for call in harness.http.calls if call[1].endswith("/chat/completions"))
                prompt = inference[2]["payload"]["messages"][0]["content"]
                if source == "input_nonce":
                    leaked = prompt.split("Test input nonce: ")[1]
                elif source == "output_nonce":
                    leaked = prompt.split(": ", 1)[1].split(".", 1)[0]
                else:
                    leaked = OUTPUT[-24:]
                all_spans(value)[-1]["status"] = {"message": leaked}
                return 200, value
            harness.http = FakeHTTP(harness, poll=poll)
            with self.subTest(source=source), self.assertRaises(trace.CheckError):
                harness.run()
            self.assertFalse(harness.result["trace"]["privacy_passed"])

    def test_preflight_rejects_fake_runtime_disabled_tracing_and_candidates(self):
        for mutation in (lambda value: value["status"]["runtime"].update(backend="fake"),
                         lambda value: value["spec"]["observability"].update(tracing=False),
                         lambda value: value.update(candidateRevisionId="pending-revision")):
            harness = trace.Harness()
            harness.http = FakeHTTP(harness)
            mutation(harness.http.deployment)
            with self.assertRaises(trace.CheckError):
                harness.run()
            self.assertEqual(harness.result["inference_requests"], 0)
            self.assertEqual(len(harness.http.calls), 1)

    def test_preflight_rejects_fake_or_unready_cache_with_canonical_vllm_backend(self):
        # The local fake adapter deliberately reports backend=vllm; its cache
        # status distinguishes it from a real runtime with prefetched weights.
        for cache in ({"weights": "NotRequired"}, {"weights": "Cold"}, {"weights": "Warming"}, {}, None):
            harness = trace.Harness()
            harness.http = FakeHTTP(harness)
            if cache is None:
                del harness.http.deployment["status"]["cache"]
            else:
                harness.http.deployment["status"]["cache"] = cache
            self.assertEqual(harness.http.deployment["status"]["runtime"]["backend"], "vllm")
            with self.subTest(cache=cache), self.assertRaisesRegex(trace.CheckError, "warm model cache"):
                harness.run()
            self.assertFalse(harness.result["passed"])
            self.assertEqual(harness.result["inference_requests"], 0)
            self.assertEqual(harness.http.polls, 0)
            self.assertEqual(len(harness.http.calls), 1)

    def test_same_forward_alias_and_duplicate_service_identity_are_rejected(self):
        for environment in ({"INFERSCALE_TRACE_TEMPO_URL": "http://localhost:8080/"},
                            {"INFERSCALE_TRACE_EPP_SERVICE": trace.DEFAULT_SERVICES["gateway"]}):
            with patch.dict(os.environ, environment), self.assertRaises(trace.CheckError):
                trace.Harness()

    def test_unexpected_errors_cannot_disclose_credentials_in_console_or_artifacts(self):
        harness = trace.Harness()
        harness.run = Mock(side_effect=RuntimeError(TOKEN + OUTPUT))
        stdout, stderr = io.StringIO(), io.StringIO()
        with patch.object(trace, "Harness", return_value=harness), contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            self.assertEqual(trace.main(), 1)
        report = stdout.getvalue() + stderr.getvalue() + next((self.directory / "results").glob("*.json")).read_text()
        self.assertIn("RuntimeError", report)
        self.assertNotIn(TOKEN, report)
        self.assertNotIn(OUTPUT, report)


if __name__ == "__main__":
    unittest.main()
