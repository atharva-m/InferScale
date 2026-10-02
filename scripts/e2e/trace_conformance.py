"""One real inference request: native trace continuity and exported-data privacy.

This host-side checker neither changes Kubernetes objects nor starts forwards.
It intentionally writes a summary, never the raw trace or inference response.
"""

from __future__ import annotations

import base64
import datetime as dt
import hashlib
import ipaddress
import json
import math
import os
from pathlib import Path
import re
import secrets
import stat
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid

MAX_JSON = 8 << 20
DEFAULT_SERVICES = {
    "gateway": "inferscale.inferscale-gateway",
    "admission": "inferscale-admission",
    "epp": "inferscale-epp",
    "runtime": "inferscale-vllm",
}


class CheckError(RuntimeError):
    """Only fixed, credential-free messages belong in this exception."""


def require(condition, message):
    if not condition:
        raise CheckError(message)


def local_origin(value):
    require(isinstance(value, str) and value and not any(char.isspace() or ord(char) < 33 or ord(char) == 127 for char in value),
            "invalid loopback origin")
    try:
        url = urllib.parse.urlsplit(value)
        host = url.hostname
        # Pin localhost to a numeric loopback address; do not depend on a host
        # resolver, and canonicalize aliases before comparing the two forwards.
        host = "127.0.0.1" if host == "localhost" else host
        address = ipaddress.ip_address(host)
        port = url.port
    except (ValueError, TypeError):
        raise CheckError("invalid loopback origin") from None
    require(url.scheme in {"http", "https"} and address.is_loopback and "%" not in host and url.path in {"", "/"}
            and not url.query and not url.fragment and "@" not in url.netloc
            and (port is None or 0 < port < 65536), "invalid loopback origin")
    host = "[" + address.compressed + "]" if address.version == 6 else address.compressed
    port = port or (443 if url.scheme == "https" else 80)
    return url.scheme + "://" + host + ":" + str(port)


def bounded(name, default, minimum, maximum):
    value = os.environ.get(name, str(default))
    require(re.fullmatch(r"[0-9]+", value) is not None, "invalid trace checker time bound")
    value = int(value)
    require(minimum <= value <= maximum, "trace checker time bound is outside allowed range")
    return value


def private_auth(path):
    # Validate the opened file, not a separate stat that could race a replacement.
    # O_NONBLOCK prevents an untrusted FIFO from hanging before fstat can reject it.
    descriptor = os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    with os.fdopen(descriptor, "rb") as source:
        mode = os.fstat(source.fileno()).st_mode
        require(stat.S_ISREG(mode) and not stat.S_IMODE(mode) & 0o077,
                "auth file must be a regular private file (0600 or stricter)")
        raw = source.read(65537)
    require(len(raw) <= 65536, "auth file exceeds size limit")
    value = parse_json(raw)
    require(isinstance(value, dict), "auth file must be a JSON object")
    return value


def parse_json(raw):
    def unique_object(pairs):
        result = {}
        for key, value in pairs:
            require(key not in result, "JSON response contains duplicate object keys")
            result[key] = value
        return result

    def reject_constant(value):
        raise CheckError("JSON response contains a non-finite number")

    try:
        # A duplicate field must not erase content before the privacy scan.
        return json.loads(raw, object_pairs_hook=unique_object, parse_constant=reject_constant)
    except (ValueError, UnicodeError):
        raise CheckError("response was not valid JSON") from None


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class LocalHTTP:
    def __init__(self):
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

    def json(self, origin, path, *, token=None, payload=None, headers=None, timeout=20):
        # No redirects, environment proxy or authorization forwarding to Tempo.
        origin = local_origin(origin)
        require(isinstance(path, str) and path.startswith("/") and not path.startswith("//")
                and not any(char.isspace() or ord(char) < 33 or ord(char) == 127 for char in path)
                and not urllib.parse.urlsplit(path).fragment, "invalid local request path")
        request_headers = {"Accept": "application/json", **(headers or {})}
        if token is not None:
            request_headers["Authorization"] = "Bearer " + token
        body = None
        if payload is not None:
            request_headers["Content-Type"] = "application/json"
            body = json.dumps(payload).encode()
        request = urllib.request.Request(origin + path, data=body, headers=request_headers)
        try:
            response = self.opener.open(request, timeout=timeout)
        except urllib.error.HTTPError as error:
            # Error bodies can contain request data. Never copy them to artifacts.
            status = error.code
            error.close()
            return status, None
        with response:
            raw = response.read(MAX_JSON + 1)
            require(len(raw) <= MAX_JSON, "HTTP JSON response exceeds size limit")
            return response.status, parse_json(raw)


def wire_id(value, byte_count):
    """Tempo legacy protobuf JSON uses base64; OTLP JSON uses hexadecimal."""
    require(isinstance(value, str), "trace contains an invalid span or trace ID")
    if re.fullmatch(r"[0-9a-fA-F]{" + str(byte_count * 2) + "}", value):
        raw = bytes.fromhex(value)
    else:
        try:
            raw = base64.b64decode(value, validate=True)
        except ValueError:
            raise CheckError("trace contains an invalid span or trace ID") from None
    require(len(raw) == byte_count and any(raw), "trace contains an invalid span or trace ID")
    return raw.hex()


def string_attribute(attributes, key):
    require(isinstance(attributes, list) and all(isinstance(item, dict) and isinstance(item.get("value"), dict)
                                              for item in attributes), "invalid OTLP attributes")
    matches = [item.get("value", {}).get("stringValue") for item in attributes if item.get("key") == key]
    require(len(matches) <= 1, "trace contains a duplicate service identity")
    return matches[0] if matches else None


def spans(document):
    require(isinstance(document, dict), "Tempo trace is not a JSON object")
    batches = document.get("resourceSpans", document.get("batches"))
    require(isinstance(batches, list), "Tempo response has no OTLP resource batches")
    result = []
    for batch in batches:
        require(isinstance(batch, dict) and isinstance(batch.get("resource", {}), dict), "invalid OTLP resource batch")
        service = string_attribute(batch.get("resource", {}).get("attributes", []), "service.name")
        require(service is None or isinstance(service, str), "invalid OTLP service identity")
        groups = batch.get("scopeSpans", batch.get("instrumentationLibrarySpans", []))
        require(isinstance(groups, list), "invalid OTLP scope groups")
        for group in groups:
            require(isinstance(group, dict), "invalid OTLP scope group")
            scope = group.get("scope", group.get("instrumentationLibrary", {}))
            require(isinstance(scope, dict) and isinstance(scope.get("name", ""), str), "invalid OTLP scope identity")
            group_spans = group.get("spans", [])
            require(isinstance(group_spans, list), "invalid OTLP spans")
            for span in group_spans:
                require(isinstance(span, dict), "invalid OTLP span")
                result.append({"service": service, "scope": scope.get("name", ""), "span": span})
                require(len(result) <= 10000, "trace exceeds span budget")
    return result


def numeric_attribute(value):
    if not isinstance(value, dict):
        return False
    return (set(value) == {"intValue"} and re.fullmatch(r"-?[0-9]+", str(value["intValue"])) is not None
            or set(value) == {"doubleValue"} and type(value["doubleValue"]) in {int, float}
            and math.isfinite(value["doubleValue"]))


def sensitive_attribute(key, value):
    if not isinstance(key, str):
        return True
    key = re.sub(r"([a-z0-9])([A-Z])", r"\1.\2", key).lower()
    parts = re.sub(r"[^a-z0-9]+", ".", key).strip(".").split(".")
    words = set(parts)
    if words & {"authorization", "bearer", "cookie", "cookies", "baggage", "password", "secret"}:
        return True
    if any(pair in "".join(parts) for pair in ("apikey", "accesstoken", "refreshtoken", "idtoken")):
        return True
    content = words & {"prompt", "prompts", "completion", "completions", "body", "payload", "messages", "message", "content"}
    # Numeric usage/body length is operational metadata, not content. String
    # lookalikes are rejected so a text leak cannot hide under a metric key.
    if content:
        return not (numeric_attribute(value) and parts[-1] in {"tokens", "count", "size", "length", "bytes", "duration"})
    return False


def privacy_findings(document, needles):
    """Scan all exported fields, including resource/scope/link attrs and events.

    Findings contain only a structural node number and a fixed category. Even
    a malicious attribute *key* must not leak into the report.
    """
    findings, stack, index, finding_count = [], [document], 0, 0
    def record(kind):
        nonlocal finding_count
        finding_count += 1
        if len(findings) < 100:
            findings.append({"node": index, "kind": kind})
    needles = tuple(value for value in needles if isinstance(value, str) and value)
    while stack:
        value = stack.pop()
        index += 1
        require(index <= 200000, "trace exceeds privacy scanner node budget")
        if isinstance(value, dict):
            if "key" in value and "value" in value and sensitive_attribute(value["key"], value["value"]):
                record("sensitive_attribute")
            # Content events are not allowed even if their payload is empty.
            event_name = value.get("name", "")
            if isinstance(event_name, str) and re.match(r"gen_ai\.(user|assistant|system|tool)\.message$|gen_ai\.(prompt|completion)$", event_name):
                record("content_event")
            if isinstance(value.get("bytesValue"), str):
                try:
                    decoded = base64.b64decode(value["bytesValue"], validate=True).decode("utf-8", errors="replace")
                    stack.append(decoded)
                except ValueError:
                    record("invalid_encoded_attribute")
            stack.extend(value.keys())
            stack.extend(value.values())
        elif isinstance(value, list):
            stack.extend(value)
        elif isinstance(value, str):
            if any(needle in value for needle in needles):
                record("synthetic_content_or_credential")
            elif re.search(r"\bbearer\s+[a-z0-9._~+/=-]{8,}", value, flags=re.IGNORECASE):
                record("bearer_value")
    return findings, finding_count


def assess(document, trace_id, caller_id, expected_services, needles):
    findings, finding_count = privacy_findings(document, needles)
    result = {"component_spans": {name: 0 for name in expected_services}, "span_count": 0,
              "missing_components": list(expected_services), "disconnected_components": [],
              "privacy_findings": findings, "privacy_finding_count": finding_count,
              "correlated": False, "privacy_passed": finding_count == 0, "passed": False}
    rows = spans(document)
    by_id, originals, components = {}, {}, {name: [] for name in expected_services}
    for row in rows:
        span = row["span"]
        require(wire_id(span.get("traceId"), 16) == trace_id, "Tempo returned a span from a different trace")
        identity = wire_id(span.get("spanId"), 8)
        parent = span.get("parentSpanId")
        parent = wire_id(parent, 8) if parent else None
        require(identity != caller_id, "exported span reused the synthetic caller ID")
        signature = (parent, row["service"], row["scope"])
        canonical = {**span, "traceId": trace_id, "spanId": identity, "parentSpanId": parent}
        require(identity not in by_id or by_id[identity] == signature and originals[identity] == canonical,
                "trace contains conflicting duplicate span IDs")
        if identity in by_id:
            continue
        by_id[identity] = signature
        originals[identity] = canonical
        for component, service in expected_services.items():
            if row["service"] == service:
                components[component].append(identity)
    result["span_count"] = len(by_id)
    result["component_spans"] = {key: len(value) for key, value in components.items()}
    result["missing_components"] = [key for key, value in components.items() if not value]

    ancestry = {caller_id: True}
    def reaches_caller(identity):
        visited = set()
        while identity not in ancestry:
            if identity in visited or identity not in by_id:
                ancestry[identity] = False
                break
            visited.add(identity)
            identity = by_id[identity][0]
        connected = ancestry[identity]
        ancestry.update((item, connected) for item in visited)
        return connected

    # Shared trace IDs alone are insufficient: every required component must
    # have a recorded ancestor path back to the caller's W3C parent span.
    result["disconnected_components"] = [key for key, identities in components.items()
                                          if identities and not all(reaches_caller(item) for item in identities)]
    result["correlated"] = not result["missing_components"] and not result["disconnected_components"]
    result["passed"] = result["correlated"] and result["privacy_passed"]
    return result


class Harness:
    def __init__(self):
        auth = private_auth(os.environ.get("INFERSCALE_TRACE_AUTH_FILE", ""))
        self.origin = local_origin(auth.get("base_url"))
        self.tempo = local_origin(os.environ.get("INFERSCALE_TRACE_TEMPO_URL", ""))
        require(self.origin != self.tempo, "Gateway and Tempo must use separate explicit forwards")
        self.token = auth.get("api_key")
        require(isinstance(self.token, str) and 16 <= len(self.token) <= 8192 and self.token.isascii()
                and all(33 <= ord(char) <= 126 for char in self.token), "invalid API key")
        self.deployment_id = os.environ.get("INFERSCALE_TRACE_DEPLOYMENT_ID", auth.get("deployment_id", ""))
        require(str(uuid.UUID(self.deployment_id)) == self.deployment_id, "existing deployment UUID is required")
        self.services = {key: os.environ.get("INFERSCALE_TRACE_" + key.upper() + "_SERVICE", value)
                         for key, value in DEFAULT_SERVICES.items()}
        require(all(re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_./:-]{0,127}", value) for value in self.services.values())
                and len(set(self.services.values())) == 4, "four distinct exact OTEL service names are required")
        self.request_timeout = bounded("INFERSCALE_TRACE_REQUEST_TIMEOUT_SECONDS", 300, 30, 900)
        self.wait_seconds = bounded("INFERSCALE_TRACE_WAIT_SECONDS", 90, 20, 300)
        self.settle_seconds = bounded("INFERSCALE_TRACE_SETTLE_SECONDS", 10, 5, 30)
        self.http = LocalHTTP()
        self.trace_id, self.caller_id = secrets.token_hex(16), secrets.token_hex(8)
        self.result = {"schema_version": 1, "recorded_at": dt.datetime.now(dt.timezone.utc).isoformat(),
                       "trace_id": self.trace_id, "caller_span_id": self.caller_id, "caller_span_exported": False,
                       "deployment_id": self.deployment_id, "expected_services": self.services,
                       "passed": False, "release_signoff": False, "inference_requests": 0,
                       "scope": "one successful non-streaming real-vLLM request through native Gateway/admission/EPP",
                       "excluded": ["streaming and error-path privacy", "other concurrent traces and logs", "exact pinned-image release sign-off"]}
        lock = Path(__file__).resolve().parents[2] / "versions.lock.yaml"
        self.result["versions_lock_sha256"] = "sha256:" + hashlib.sha256(lock.read_bytes()).hexdigest()
        self.stage = "preflight"

    def run(self):
        status, deployment = self.http.json(self.origin, "/v1/deployments/" + self.deployment_id, token=self.token)
        require(status == 200 and isinstance(deployment, dict), "authorized deployment lookup failed")
        require(deployment.get("spec", {}).get("observability", {}).get("tracing") is True, "deployment tracing must be enabled")
        backend = deployment.get("status", {}).get("runtime", {}).get("backend")
        require(backend == "vllm", "trace qualification requires an observed real vLLM runtime")
        require(deployment.get("status", {}).get("cache", {}).get("weights") == "Warm",
                "trace qualification requires an observed warm model cache")
        model = deployment.get("name")
        require(isinstance(model, str) and model, "deployment has no served model name")
        require(not deployment.get("candidateRevisionId"), "deployment must have a stable revision without a candidate")

        self.stage = "inference"
        input_marker, output_marker = "TRACE_INPUT_" + secrets.token_hex(12), "TRACE_REPLY_" + secrets.token_hex(12)
        prompt = "Reply with exactly this identifier and nothing else: " + output_marker + ". Test input nonce: " + input_marker
        payload = {"model": model, "messages": [{"role": "user", "content": prompt}], "max_tokens": 48,
                   "temperature": 0, "stream": False}
        self.result["inference_requests"] = 1
        started = time.monotonic()
        status, completion = self.http.json(self.origin, "/v1/deployments/" + self.deployment_id + "/chat/completions",
                                           token=self.token, payload=payload, timeout=self.request_timeout,
                                           headers={"traceparent": "00-" + self.trace_id + "-" + self.caller_id + "-01"})
        self.result["inference_status"] = status
        self.result["inference_seconds"] = round(time.monotonic() - started, 3)
        require(status == 200 and isinstance(completion, dict), "synthetic inference request failed")
        require(completion.get("model") == model, "inference response model identity did not match")
        choices = completion.get("choices", [])
        require(isinstance(choices, list) and len(choices) == 1, "inference response must contain one completion")
        output = choices[0].get("message", {}).get("content")
        require(isinstance(output, str) and len(output.strip()) >= 8, "completion was too short for reliable literal privacy scanning")
        needles = [self.token, prompt, input_marker, output_marker, output.strip()]
        # Also catch partial completion exports, without comparing tiny common
        # words such as 'OK' to unrelated span status values.
        needles.extend(output[index:index + 24] for index in range(0, len(output) - 23, 12))
        if len(output) >= 24:
            needles.append(output[-24:])

        self.stage = "trace_export"
        deadline, good_since = time.monotonic() + self.wait_seconds, None
        while time.monotonic() < deadline:
            status, document = self.http.json(self.tempo, "/api/traces/" + self.trace_id,
                                              timeout=min(15, max(0.1, deadline - time.monotonic())))
            require(status in {200, 404}, "Tempo trace query failed")
            self.result["tempo_status"] = status
            if status == 200:
                evaluation = assess(document, self.trace_id, self.caller_id, self.services, needles)
                self.result["trace"] = evaluation
                require(evaluation["privacy_passed"], "exported trace failed the content privacy checks")
                if evaluation["passed"]:
                    good_since = good_since if good_since is not None else time.monotonic()
                    if time.monotonic() - good_since >= self.settle_seconds:
                        self.result["settle_seconds"] = self.settle_seconds
                        self.result["passed"] = True
                        return
                else:
                    good_since = None
            else:
                good_since = None
            time.sleep(min(2, max(0, deadline - time.monotonic())))
        raise CheckError("complete correlated spans did not remain available before the trace export deadline")

    def save(self):
        directory = Path(os.environ.get("INFERSCALE_TRACE_ARTIFACT_DIR", "benchmarks/artifacts/trace-conformance"))
        directory.mkdir(mode=0o700, parents=True, exist_ok=True)
        path = directory / (self.trace_id + ".json")
        descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        with os.fdopen(descriptor, "w") as output:
            json.dump(self.result, output, indent=2, sort_keys=True)
            output.write("\n")
        return path.resolve()


def main():
    harness = None
    try:
        harness = Harness()
        harness.run()
    except (Exception, KeyboardInterrupt) as error:
        # Unexpected exceptions might contain a response, bearer or URL. Keep
        # only the type; explicitly constructed CheckErrors are safe to report.
        message = str(error) if isinstance(error, CheckError) else type(error).__name__
        if harness is not None:
            harness.result.update(failure=message, failed_stage=harness.stage)
        print("Trace qualification failed: " + message, file=sys.stderr)
        return_code = 1
    else:
        print("Trace qualification passed for Gateway, admission, EPP and vLLM; no tested content was exported.")
        return_code = 0
    if harness is not None:
        try:
            print("Summary: " + str(harness.save()))
        except Exception:
            print("Trace qualification summary could not be written.", file=sys.stderr)
            return_code = 1
    return return_code


if __name__ == "__main__":
    sys.exit(main())
