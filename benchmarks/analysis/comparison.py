"""Read and match schema-v1 benchmark artifacts for measured comparisons."""

from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Mapping


ComparisonAxis = str
_AXES = {"backend", "gpu_scaling", "concurrency"}


def _mapping(value: Any) -> Mapping[str, Any]:
    return value if isinstance(value, Mapping) else {}


def _number(value: Any) -> float | None:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    number = float(value)
    return number if math.isfinite(number) else None


def _freeze(value: Any) -> Any:
    """Make JSON configuration values comparable and usable in group keys."""
    if isinstance(value, Mapping):
        return tuple(sorted((str(key), _freeze(item)) for key, item in value.items()))
    if isinstance(value, list):
        return tuple(_freeze(item) for item in value)
    return value


@dataclass(frozen=True)
class ResultArtifact:
    """A runner-produced result plus its source path and original provenance."""

    payload: Mapping[str, Any]
    source: Path | None = None

    def __post_init__(self) -> None:
        if self.payload.get("schema_version") != 1:
            raise ValueError("benchmark result schema_version must be 1")

    @property
    def scenario(self) -> Mapping[str, Any]:
        return _mapping(self.payload.get("scenario"))

    @property
    def deployment(self) -> Mapping[str, Any]:
        return _mapping(self.scenario.get("deployment"))

    @property
    def workload(self) -> Mapping[str, Any]:
        return _mapping(self.scenario.get("workload"))

    @property
    def provenance(self) -> Mapping[str, Any]:
        return _mapping(self.payload.get("provenance"))

    def metric(self, section: str, *path: str) -> float | None:
        value: Any = self.payload.get(section)
        for part in path:
            value = _mapping(value).get(part)
        return _number(value)

    @property
    def p95_ttft_ms(self) -> float | None:
        return self.metric("aggregate", "ttft_ms", "p95")

    @property
    def p95_tpot_ms(self) -> float | None:
        return self.metric("aggregate", "tpot_ms", "p95")

    @property
    def output_tokens_per_second(self) -> float | None:
        return self.metric("aggregate", "output_tokens_per_s")

    @property
    def slo_attainment(self) -> float | None:
        return self.metric("aggregate", "slo_attainment")

    @property
    def gpu_hours(self) -> float | None:
        return self.metric("cost", "gpu_hours")

    @property
    def cost_per_million_input_tokens(self) -> float | None:
        return self.metric("cost", "cost_per_1m_input_tokens")

    @property
    def cost_per_million_output_tokens(self) -> float | None:
        return self.metric("cost", "cost_per_1m_output_tokens")


def from_payload(
    payload: Mapping[str, Any], source: str | Path | None = None
) -> ResultArtifact:
    return ResultArtifact(payload, Path(source) if source is not None else None)


def load_result(path: str | Path) -> ResultArtifact:
    source = Path(path)
    payload = json.loads(source.read_text(encoding="utf-8"))
    if not isinstance(payload, Mapping):
        raise ValueError(f"benchmark result must be a JSON object: {source}")
    return ResultArtifact(payload, source)


def load_results(paths: Iterable[str | Path]) -> list[ResultArtifact]:
    return [load_result(path) for path in paths]


def _key_value(artifact: ResultArtifact, axis: ComparisonAxis) -> tuple[Any, ...]:
    if axis not in _AXES:
        raise ValueError(f"unsupported comparison axis: {axis}")

    scenario = artifact.scenario
    deployment = artifact.deployment
    workload = artifact.workload
    provenance = artifact.provenance

    deployment_fields = (
        "model",
        "model_revision",
        "gpu_type",
        "precision",
        "quantization",
        "prefix_cache",
        "max_model_len",
    )
    if axis != "backend":
        deployment_fields += ("backend", "backend_version")
    if axis != "gpu_scaling":
        deployment_fields += ("gpu_count", "tensor_parallelism")

    workload_fields = ("input_tokens", "output_tokens", "requests", "dataset", "seed")
    if axis != "concurrency":
        workload_fields += ("concurrency",)

    target = _mapping(scenario.get("target"))
    key = [
        scenario.get("schema_version"),
        scenario.get("measurement_tool"),
        scenario.get("publishable"),
        _freeze(target.get("api_mode")),
        tuple((field, _freeze(deployment.get(field))) for field in deployment_fields),
        tuple((field, _freeze(workload.get(field))) for field in workload_fields),
        _freeze(scenario.get("cache_state")),
        _freeze(scenario.get("routing_policy")),
        _freeze(scenario.get("slo")),
        _freeze(scenario.get("experiment")),
        _freeze(artifact.payload.get("measurement_contract")),
        # The selection digest includes concurrency, so the explicit scenario
        # fields above are used for concurrency matching instead.
        _freeze(artifact.payload.get("selection_scenario_digest"))
        if axis != "concurrency"
        else None,
        _freeze(provenance.get("provider")),
        _freeze(provenance.get("driver_cuda_fingerprint")),
        _freeze(provenance.get("benchmark_tool")),
        _freeze(provenance.get("git_commit")),
        _freeze(provenance.get("runner_image_digest")),
        _freeze(provenance.get("gpu_hourly_price")),
    ]

    # Runtime identity is fixed for scaling and concurrency comparisons. It is
    # the deliberately varied identity for matched backend comparisons.
    if axis != "backend":
        key.extend(
            _freeze(provenance.get(field))
            for field in (
                "runtime_version",
                "runtime_image_digest",
                "container_image_digest",
            )
        )
    return tuple(key)


def compatible(
    left: ResultArtifact, right: ResultArtifact, axis: ComparisonAxis
) -> bool:
    """Whether two runs differ only along the requested v1 comparison axis."""
    return _key_value(left, axis) == _key_value(right, axis)


def group_compatible(
    results: Iterable[ResultArtifact], axis: ComparisonAxis
) -> list[list[ResultArtifact]]:
    """Group compatible runs while retaining every source artifact separately."""
    groups: dict[tuple[Any, ...], list[ResultArtifact]] = {}
    for result in results:
        groups.setdefault(_key_value(result, axis), []).append(result)
    return list(groups.values())


@dataclass(frozen=True)
class ScalingComparison:
    run: ResultArtifact
    baseline: ResultArtifact | None
    throughput_speedup: float | None
    scaling_efficiency: float | None


def _is_single_gpu(result: ResultArtifact) -> bool:
    return (
        result.deployment.get("gpu_count") == 1
        and result.deployment.get("tensor_parallelism") == 1
    )


def scaling_comparisons(
    results: Iterable[ResultArtifact],
    baseline: ResultArtifact | str | Path | None = None,
) -> list[ScalingComparison]:
    """Compute measured speedup and efficiency against a unique 1-GPU run.

    An explicit baseline can be an artifact or its source path. Without one,
    a run is compared only when exactly one compatible 1-GPU baseline exists.
    Ambiguous or unusable measurements leave derived values absent.
    """
    runs = list(results)
    explicit = baseline if isinstance(baseline, ResultArtifact) else None
    baseline_source = (
        Path(baseline) if baseline is not None and explicit is None else None
    )
    if baseline_source is not None:
        explicit = next((run for run in runs if run.source == baseline_source), None)
        if explicit is None:
            explicit = load_result(baseline_source)

    comparisons: list[ScalingComparison] = []
    for run in runs:
        candidates = [
            item
            for item in runs
            if _is_single_gpu(item) and compatible(run, item, "gpu_scaling")
        ]
        if explicit is not None:
            selected = (
                explicit
                if _is_single_gpu(explicit) and compatible(run, explicit, "gpu_scaling")
                else None
            )
        else:
            selected = candidates[0] if len(candidates) == 1 else None

        speedup = efficiency = None
        if selected is not None:
            baseline_rate = selected.output_tokens_per_second
            run_rate = run.output_tokens_per_second
            gpu_count = _number(run.deployment.get("gpu_count"))
            baseline_count = _number(selected.deployment.get("gpu_count"))
            if (
                baseline_rate is not None
                and baseline_rate > 0
                and run_rate is not None
                and run_rate > 0
                and gpu_count is not None
                and gpu_count > 0
                and baseline_count is not None
                and baseline_count > 0
            ):
                speedup = run_rate / baseline_rate
                efficiency = speedup / (gpu_count / baseline_count)
        comparisons.append(ScalingComparison(run, selected, speedup, efficiency))
    return comparisons
