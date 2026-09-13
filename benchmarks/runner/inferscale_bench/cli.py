from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import cast

from .callback import BENCHMARK_TOKEN_ENV, CallbackError, bounded_error, post_result
from .client import run_requests
from .guidellm import GuideLLMError
from .guidellm import run as run_guidellm
from .metrics import (
    aggregate,
    scrape_prometheus,
    validate_publishable_telemetry,
)
from .provenance import collect
from .report import failed_report, successful_report
from .scenario import (
    GUIDELLM_TOOL,
    NATIVE_HTTP_TOOL,
    Scenario,
    ScenarioError,
    load_scenario,
)
from .workload import load_messages


def _repo_root() -> Path:
    return Path(__file__).resolve().parents[3]


def _write_result(path: Path, payload: dict[str, object]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(
        json.dumps(payload, indent=2, sort_keys=True) + "\n", encoding="utf-8"
    )
    temporary.replace(path)


@dataclass(frozen=True)
class RunExecution:
    exit_code: int
    scenario_path: Path
    scenario: Scenario
    payload: dict[str, object]
    artifact: Path


def _validate(paths: list[str]) -> int:
    for path in paths:
        scenario = load_scenario(path)
        print(f"valid: {scenario.name} ({path})")
    return 0


def _default_results_dir(scheduled: bool) -> Path:
    configured = os.getenv("INFERSCALE_BENCHMARK_RESULTS_DIR")
    if configured:
        return Path(configured)
    if scheduled:
        persistent = Path("/results")
        if persistent.is_dir() and os.access(persistent, os.W_OK):
            return persistent
        return Path("/tmp/results")
    return _repo_root() / "benchmarks" / "results"


def _sensitive_values() -> list[str]:
    names = {BENCHMARK_TOKEN_ENV, "INFERSCALE_API_KEY"}
    names.update(name for name in os.environ if name.endswith("_API_KEY"))
    return [os.environ.get(name, "") for name in names]


def _run(args: argparse.Namespace) -> RunExecution | None:
    scheduled = bool(args.callback)
    scenario_path = Path(args.scenario).resolve()
    scenario = load_scenario(args.scenario)
    if scheduled and scenario.target.api_mode != "inferscale":
        raise ScenarioError(
            "scheduled benchmark jobs require target.api_mode=inferscale"
        )
    if scheduled and (
        scenario.measurement_tool != GUIDELLM_TOOL or not scenario.publishable
    ):
        raise ScenarioError(
            "scheduled benchmarks require publishable=true and "
            "measurement_tool=guidellm-0.7.0"
        )
    if scenario.publishable and scenario.target.api_mode != "inferscale":
        raise ScenarioError(
            "publishable benchmarks must use the authenticated InferScale path"
        )
    run_metadata = dict(scenario.metadata)
    if args.provider:
        run_metadata["provider"] = args.provider
    if args.gpu_hourly_price is not None:
        run_metadata["gpu_hourly_price"] = str(args.gpu_hourly_price)
    if args.container_image_digest:
        run_metadata["container_image_digest"] = args.container_image_digest
        run_metadata["runtime_image_digest"] = args.container_image_digest
    if args.runner_image_digest:
        run_metadata["runner_image_digest"] = args.runner_image_digest
    if args.runtime_version:
        if args.runtime_version != scenario.deployment.backend_version:
            raise ScenarioError(
                "--runtime-version does not match the authoritative scenario"
            )
        run_metadata["runtime_version"] = args.runtime_version
    if args.driver_version:
        run_metadata["driver_version"] = args.driver_version
    if args.cuda_version:
        run_metadata["cuda_version"] = args.cuda_version
    if scenario.target.api_mode != "openai":
        provider = run_metadata.get("provider", "")
        digest = run_metadata.get("container_image_digest", "")
        try:
            remote_hourly_price = float(run_metadata.get("gpu_hourly_price", "0"))
        except ValueError as exc:
            raise ScenarioError("gpu hourly price must be numeric") from exc
        if not provider or provider == "set-at-run-time":
            raise ScenarioError("remote runs require --provider")
        if remote_hourly_price <= 0:
            raise ScenarioError("remote runs require a positive --gpu-hourly-price")
        if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
            raise ScenarioError(
                "remote runs require --container-image-digest sha256:<64 hex>"
            )
        if scenario.publishable:
            if not re.fullmatch(
                r"sha256:[0-9a-f]{64}", run_metadata.get("runner_image_digest", "")
            ):
                raise ScenarioError(
                    "publishable runs require --runner-image-digest sha256:<64 hex>"
                )
            if not args.prometheus_url:
                raise ScenarioError("publishable runs require --prometheus-url")
            if (
                not args.deployment_namespace
                or not args.runtime_pod_prefix
                or not args.epp_service
            ):
                raise ScenarioError(
                    "publishable runs require --deployment-namespace, "
                    "--runtime-pod-prefix, and --epp-service"
                )
        if scheduled:
            if not run_metadata.get("runtime_version"):
                raise ScenarioError("scheduled runs require --runtime-version")
            if not re.fullmatch(r"[0-9a-f]{64}", args.configuration_digest or ""):
                raise ScenarioError(
                    "scheduled runs require --configuration-digest <64 lowercase hex>"
                )
            if not re.fullmatch(r"[0-9a-f]{64}", args.selection_scenario_digest or ""):
                raise ScenarioError(
                    "scheduled runs require --selection-scenario-digest "
                    "<64 lowercase hex>"
                )
        for field_name, option_name in (
            ("driver_version", "--driver-version"),
            ("cuda_version", "--cuda-version"),
        ):
            value = run_metadata.get(field_name, "")
            if not value or value == "set-at-run-time":
                raise ScenarioError(f"remote runs require {option_name}")
        if not re.fullmatch(
            r"[0-9]{3,4}\.[0-9]{1,3}(?:\.[0-9]{1,3})?", run_metadata["driver_version"]
        ):
            raise ScenarioError("remote NVIDIA driver version has an invalid format")
        if not re.fullmatch(
            r"[0-9]{1,2}\.[0-9]{1,2}(?:\.[0-9]+)?", run_metadata["cuda_version"]
        ):
            raise ScenarioError(
                "remote CUDA compatibility version has an invalid format"
            )
    api_key = (
        os.getenv(scenario.target.api_key_env) if scenario.target.api_key_env else None
    )
    if scheduled and not api_key:
        # The scheduler's signed token is accepted by admission only for this
        # running benchmark's tenant and deployment. It is also the callback
        # credential and becomes unusable when the run reaches a terminal state.
        api_key = os.getenv(BENCHMARK_TOKEN_ENV)
    if (
        not api_key
        and scenario.target.api_mode != "openai"
        and not args.allow_unauthenticated
    ):
        raise ScenarioError(
            f"{scenario.target.api_key_env} is unset; use --allow-unauthenticated only for a local direct runtime"
        )
    deployment_id = args.deployment_id or scenario.target.deployment_id
    if not deployment_id and scenario.target.deployment_id_env:
        deployment_id = os.getenv(scenario.target.deployment_id_env, "")
    if scenario.target.api_mode == "inferscale" and not deployment_id:
        raise ScenarioError(
            "InferScale runs require --deployment-id or the "
            f"{scenario.target.deployment_id_env} environment variable"
        )
    if args.dry_run:
        print(json.dumps(scenario.to_dict(), indent=2, sort_keys=True))
        return None
    if scenario.measurement_tool == GUIDELLM_TOOL:
        measured = run_guidellm(scenario, api_key, deployment_id)
    elif scenario.measurement_tool == NATIVE_HTTP_TOOL:
        messages = load_messages(
            scenario.workload.dataset,
            scenario.workload.requests,
            scenario.workload.seed,
        )
        measured = run_requests(scenario, messages, api_key, deployment_id)
    else:  # Defensive in case validation and dispatch drift apart.
        raise ScenarioError("unsupported measurement tool")
    results, wall_time_s = measured.requests, measured.wall_time_s
    provenance = collect(_repo_root(), run_metadata, scenario.measurement_tool)
    aggregate_metrics = aggregate(results, wall_time_s, scenario.slo)
    prometheus_snapshot = scrape_prometheus(
        args.prometheus_url,
        measured.interval,
        deployment_namespace=args.deployment_namespace,
        runtime_pod_prefix=args.runtime_pod_prefix,
        epp_service=args.epp_service,
    )
    if scenario.publishable:
        telemetry_evidence = validate_publishable_telemetry(
            prometheus_snapshot,
            scenario.deployment.gpu_count,
            scenario.deployment.gpu_type,
            measured.interval,
        )
        provenance["telemetry_evidence"] = telemetry_evidence
        provenance["nvidia_gpus"] = [
            gpu["model"]
            for gpu in cast(list[dict[str, str]], telemetry_evidence["observed_gpus"])
        ]
    gpu_count = scenario.deployment.gpu_count
    hourly_price = cast(float | None, provenance.get("gpu_hourly_price"))
    run_cost = (
        float(hourly_price) * gpu_count * wall_time_s / 3600 if hourly_price else None
    )
    output_tokens = cast(int, aggregate_metrics["output_tokens"])
    input_tokens = cast(int, aggregate_metrics["prompt_tokens"])
    successful_requests = cast(int, aggregate_metrics["successful_requests"])
    costs = {
        "gpu_hours": gpu_count * wall_time_s / 3600,
        "run_cost": run_cost,
        "cost_per_1m_input_tokens": run_cost * 1_000_000 / input_tokens
        if run_cost and input_tokens
        else None,
        "cost_per_1m_output_tokens": run_cost * 1_000_000 / output_tokens
        if run_cost and output_tokens
        else None,
        "cost_per_successful_request": (
            run_cost / successful_requests if run_cost and successful_requests else None
        ),
    }
    selection_scenario_digest = (
        args.selection_scenario_digest or scenario.selection_digest()
    )
    payload: dict[str, object] = {
        "schema_version": 1,
        "scenario": scenario.to_dict(),
        # Scheduled jobs use the control plane's frozen digest directly. This
        # avoids treating independent Go/Python JSON canonicalizers as a
        # security boundary; the callback validates the value against the
        # immutable ExecutionContract.
        "selection_scenario_digest": selection_scenario_digest,
        "measurement_contract": {
            "tool": scenario.measurement_tool,
            "publishable": scenario.publishable,
            "dataset": "synthetic_text"
            if scenario.measurement_tool == GUIDELLM_TOOL
            else "jsonl",
            "input_tokens": scenario.workload.input_tokens,
            "output_tokens": scenario.workload.output_tokens,
            "exact_token_lengths": scenario.measurement_tool == GUIDELLM_TOOL,
        },
        "scenario_configuration_digest": args.configuration_digest
        or hashlib.sha256(scenario_path.read_bytes()).hexdigest(),
        "provenance": provenance,
        "aggregate": aggregate_metrics,
        "cost": costs,
        "prometheus_snapshot": prometheus_snapshot,
        "measurement_interval": {
            "start_unix_s": measured.interval.start,
            "end_unix_s": measured.interval.end,
        },
        "requests": [result.to_dict() for result in results],
    }
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    results_dir = (
        Path(args.results_dir) if args.results_dir else _default_results_dir(scheduled)
    )
    output = (
        Path(args.output)
        if args.output
        else results_dir / f"{scenario.name}-{timestamp}.json"
    )
    artifact = output.resolve()
    _write_result(artifact, payload)
    print(artifact)
    return RunExecution(
        exit_code=0 if aggregate_metrics["failed_requests"] == 0 else 2,
        scenario_path=scenario_path,
        scenario=scenario,
        payload=payload,
        artifact=artifact,
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="InferScale reproducible benchmark runner"
    )
    subparsers = parser.add_subparsers(dest="command", required=True)
    validate = subparsers.add_parser(
        "validate", help="validate scenario files and datasets"
    )
    validate.add_argument("scenarios", nargs="+")
    run = subparsers.add_parser("run", help="execute one benchmark scenario")
    run.add_argument("--scenario", required=True)
    run.add_argument("--results-dir")
    run.add_argument("--output")
    run.add_argument("--prometheus-url")
    run.add_argument("--deployment-namespace")
    run.add_argument("--runtime-pod-prefix")
    run.add_argument("--epp-service")
    run.add_argument("--provider")
    run.add_argument("--gpu-hourly-price", type=float)
    run.add_argument("--container-image-digest")
    run.add_argument("--runner-image-digest")
    run.add_argument("--runtime-version")
    run.add_argument("--configuration-digest")
    run.add_argument("--selection-scenario-digest")
    run.add_argument("--driver-version", help="measured target NVIDIA driver version")
    run.add_argument(
        "--cuda-version", help="measured target CUDA compatibility version"
    )
    run.add_argument("--deployment-id", help="public InferScale deployment UUID")
    run.add_argument("--run-id", help="scheduler-assigned benchmark run ID")
    run.add_argument("--callback", help="authenticated terminal-result callback URL")
    run.add_argument("--allow-unauthenticated", action="store_true")
    run.add_argument("--dry-run", action="store_true")
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        if args.command == "validate":
            return _validate(args.scenarios)
        if bool(args.run_id) != bool(args.callback):
            raise ScenarioError("--run-id and --callback must be supplied together")
        if args.callback and args.dry_run:
            raise ScenarioError("scheduled benchmark jobs cannot use --dry-run")
        execution = _run(args)
        if execution is None:
            return 0
        if args.callback:
            if execution.exit_code == 0:
                report = successful_report(
                    execution.scenario,
                    execution.payload,
                    execution.artifact,
                )
            else:
                aggregate_payload = execution.payload.get("aggregate")
                failed = (
                    aggregate_payload.get("failed_requests", 0)
                    if isinstance(aggregate_payload, dict)
                    else 0
                )
                report = failed_report(
                    f"benchmark completed with {failed} failed request(s)"
                )
            post_result(args.callback, args.run_id, report)
        return execution.exit_code
    except CallbackError as exc:
        secrets = _sensitive_values()
        print(f"callback error: {bounded_error(exc, secrets)}", file=sys.stderr)
        return 1
    except (GuideLLMError, ScenarioError, ValueError, OSError) as exc:
        secrets = _sensitive_values()
        safe_error = bounded_error(exc, secrets)
        if (
            args.command == "run"
            and getattr(args, "callback", None)
            and getattr(args, "run_id", None)
        ):
            try:
                post_result(args.callback, args.run_id, failed_report(safe_error))
            except CallbackError as callback_exc:
                callback_error = bounded_error(callback_exc, secrets)
                print(f"callback error: {callback_error}", file=sys.stderr)
        print(f"error: {safe_error}", file=sys.stderr)
        return 1
