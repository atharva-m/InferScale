from __future__ import annotations

import hashlib
import json
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

import yaml


class ScenarioError(ValueError):
    pass


GUIDELLM_TOOL = "guidellm-0.7.0"
NATIVE_HTTP_TOOL = "inferscale-native-http-v1"
MEASUREMENT_TOOLS = {GUIDELLM_TOOL, NATIVE_HTTP_TOOL}


def _mapping(value: Any, field_name: str) -> dict[str, Any]:
    if not isinstance(value, dict):
        raise ScenarioError(f"{field_name} must be a mapping")
    return value


def _positive_int(value: Any, field_name: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or value <= 0:
        raise ScenarioError(f"{field_name} must be a positive integer")
    return value


@dataclass(frozen=True)
class Target:
    base_url: str
    # `deployment` is the tenant-visible OpenAI model name. InferScale paths
    # use the separate globally unique deployment UUID.
    deployment: str
    deployment_id: str = ""
    deployment_id_env: str = "INFERSCALE_DEPLOYMENT_ID"
    api_mode: str = "inferscale"
    api_key_env: str = "INFERSCALE_API_KEY"
    request_timeout_s: float = 300.0
    verify_tls: bool = True

    @classmethod
    def from_mapping(cls, value: Any) -> Target:
        data = _mapping(value, "target")
        base_url = str(data.get("base_url", "")).rstrip("/")
        deployment = str(data.get("deployment", ""))
        if not base_url.startswith(("http://", "https://")):
            raise ScenarioError("target.base_url must use http:// or https://")
        if not deployment:
            raise ScenarioError("target.deployment is required")
        api_mode = str(data.get("api_mode", "inferscale"))
        if api_mode not in {"inferscale", "openai"}:
            raise ScenarioError("target.api_mode must be inferscale or openai")
        timeout = float(data.get("request_timeout_s", 300.0))
        if timeout <= 0:
            raise ScenarioError("target.request_timeout_s must be positive")
        return cls(
            base_url=base_url,
            deployment=deployment,
            deployment_id=str(data.get("deployment_id", "")),
            deployment_id_env=str(
                data.get("deployment_id_env", "INFERSCALE_DEPLOYMENT_ID")
            ),
            api_mode=api_mode,
            api_key_env=str(data.get("api_key_env", "INFERSCALE_API_KEY")),
            request_timeout_s=timeout,
            verify_tls=bool(data.get("verify_tls", True)),
        )


@dataclass(frozen=True)
class Deployment:
    model: str
    model_revision: str
    backend: str
    backend_version: str
    precision: str
    quantization: str
    gpu_type: str
    gpu_count: int
    tensor_parallelism: int
    prefix_cache: bool
    max_model_len: int

    @classmethod
    def from_mapping(cls, value: Any) -> Deployment:
        data = _mapping(value, "deployment")
        required = ("model", "model_revision", "backend", "backend_version", "gpu_type")
        missing = [name for name in required if not str(data.get(name, ""))]
        if missing:
            raise ScenarioError(f"deployment is missing: {', '.join(missing)}")
        revision = str(data["model_revision"]).lower()
        if len(revision) != 40 or any(
            character not in "0123456789abcdef" for character in revision
        ):
            raise ScenarioError(
                "deployment.model_revision must be an immutable 40-hex commit SHA"
            )
        gpu_count = _positive_int(data.get("gpu_count"), "deployment.gpu_count")
        tp = _positive_int(
            data.get("tensor_parallelism"), "deployment.tensor_parallelism"
        )
        if gpu_count != tp:
            raise ScenarioError(
                "v1 requires deployment.gpu_count == tensor_parallelism"
            )
        backend = str(data["backend"]).lower()
        if backend not in {"vllm", "tensorrt-llm"}:
            raise ScenarioError(
                "benchmark scenarios require a resolved vllm or tensorrt-llm backend"
            )
        precision = str(data.get("precision", "bf16")).lower()
        if precision not in {"bf16", "fp8"}:
            raise ScenarioError("deployment.precision must be bf16 or fp8")
        quantization = str(data.get("quantization", "none")).lower()
        if quantization not in {"none", "awq"}:
            raise ScenarioError("deployment.quantization must be none or awq")
        if precision == "fp8" and quantization == "awq":
            raise ScenarioError(
                "fp8 precision cannot be combined with awq quantization"
            )
        return cls(
            model=str(data["model"]),
            model_revision=revision,
            backend=backend,
            backend_version=str(data["backend_version"]),
            precision=precision,
            quantization=quantization,
            gpu_type=str(data["gpu_type"]),
            gpu_count=gpu_count,
            tensor_parallelism=tp,
            prefix_cache=bool(data.get("prefix_cache", False)),
            max_model_len=_positive_int(
                data.get("max_model_len", 8192), "deployment.max_model_len"
            ),
        )


@dataclass(frozen=True)
class Workload:
    input_tokens: int
    output_tokens: int
    concurrency: int
    requests: int
    dataset: str
    seed: int = 1

    @classmethod
    def from_mapping(cls, value: Any) -> Workload:
        data = _mapping(value, "workload")
        dataset = str(data.get("dataset", ""))
        if not dataset:
            raise ScenarioError("workload.dataset is required")
        concurrency = _positive_int(data.get("concurrency"), "workload.concurrency")
        requests = _positive_int(data.get("requests"), "workload.requests")
        if concurrency > requests:
            raise ScenarioError("workload.concurrency cannot exceed workload.requests")
        return cls(
            input_tokens=_positive_int(
                data.get("input_tokens"), "workload.input_tokens"
            ),
            output_tokens=_positive_int(
                data.get("output_tokens"), "workload.output_tokens"
            ),
            concurrency=concurrency,
            requests=requests,
            dataset=dataset,
            seed=int(data.get("seed", 1)),
        )


@dataclass(frozen=True)
class SLO:
    ttft_p95_ms: float | None = None
    tpot_p95_ms: float | None = None

    @classmethod
    def from_mapping(cls, value: Any) -> SLO:
        if value is None:
            return cls()
        data = _mapping(value, "slo")
        ttft = float(data["ttft_p95_ms"]) if "ttft_p95_ms" in data else None
        tpot = float(data["tpot_p95_ms"]) if "tpot_p95_ms" in data else None
        if ttft is not None and ttft <= 0 or tpot is not None and tpot <= 0:
            raise ScenarioError("SLO targets must be positive")
        return cls(ttft_p95_ms=ttft, tpot_p95_ms=tpot)


@dataclass(frozen=True)
class Scenario:
    schema_version: int
    name: str
    target: Target
    deployment: Deployment
    workload: Workload
    measurement_tool: str
    publishable: bool
    cache_state: str
    routing_policy: str
    slo: SLO = field(default_factory=SLO)
    experiment: dict[str, Any] = field(default_factory=dict)
    metadata: dict[str, str] = field(default_factory=dict)
    source_path: str = ""

    @classmethod
    def from_mapping(cls, value: Any, source_path: str = "") -> Scenario:
        data = _mapping(value, "scenario")
        if data.get("schema_version") != 1:
            raise ScenarioError("schema_version must be 1")
        name = str(data.get("name", ""))
        if not name:
            raise ScenarioError("name is required")
        cache_state = str(data.get("cache_state", ""))
        if cache_state not in {"remote-cold", "cache-warm", "process-warm"}:
            raise ScenarioError(
                "cache_state must be remote-cold, cache-warm, or process-warm"
            )
        routing = _mapping(data.get("routing", {}), "routing")
        policy = str(routing.get("policy", ""))
        if policy not in {"round-robin", "load-aware", "prefix-aware"}:
            raise ScenarioError(
                "routing.policy must be round-robin, load-aware, or prefix-aware"
            )
        metadata = _mapping(data.get("metadata", {}), "metadata")
        experiment = _mapping(data.get("experiment", {}), "experiment")
        measurement_tool = str(data.get("measurement_tool", ""))
        if measurement_tool not in MEASUREMENT_TOOLS:
            raise ScenarioError(
                "measurement_tool must be guidellm-0.7.0 or inferscale-native-http-v1"
            )
        publishable = data.get("publishable")
        if not isinstance(publishable, bool):
            raise ScenarioError("publishable must be explicitly true or false")
        workload = Workload.from_mapping(data.get("workload"))
        if measurement_tool == GUIDELLM_TOOL:
            if workload.dataset != "synthetic":
                raise ScenarioError(
                    "GuideLLM scenarios require workload.dataset=synthetic"
                )
        elif publishable:
            raise ScenarioError(
                "native HTTP scenarios are behavioral evidence and cannot be publishable"
            )
        return cls(
            schema_version=1,
            name=name,
            target=Target.from_mapping(data.get("target")),
            deployment=Deployment.from_mapping(data.get("deployment")),
            workload=workload,
            measurement_tool=measurement_tool,
            publishable=publishable,
            cache_state=cache_state,
            routing_policy=policy,
            slo=SLO.from_mapping(data.get("slo")),
            experiment=experiment,
            metadata={str(k): str(v) for k, v in metadata.items()},
            source_path=source_path,
        )

    def to_dict(self) -> dict[str, Any]:
        return asdict(self)

    def selection_digest(self) -> str:
        """Hash the standardized workload independently of host file paths."""
        dataset = Path(self.workload.dataset)
        dataset_identity = (
            "guidellm:synthetic_text:exact"
            if self.measurement_tool == GUIDELLM_TOOL
            else hashlib.sha256(dataset.read_bytes()).hexdigest()
        )
        contract = {
            "schema_version": self.schema_version,
            "measurement_tool": self.measurement_tool,
            "publishable": self.publishable,
            "workload": {
                "input_tokens": self.workload.input_tokens,
                "output_tokens": self.workload.output_tokens,
                "concurrency": self.workload.concurrency,
                "requests": self.workload.requests,
                "seed": self.workload.seed,
                "dataset_identity": dataset_identity,
            },
            "cache_state": self.cache_state,
            "routing_policy": self.routing_policy,
            "experiment": self.experiment,
        }
        encoded = json.dumps(contract, sort_keys=True, separators=(",", ":")).encode()
        return hashlib.sha256(encoded).hexdigest()


def load_scenario(path: str | Path) -> Scenario:
    scenario_path = Path(path).resolve()
    try:
        raw = yaml.safe_load(scenario_path.read_text(encoding="utf-8"))
    except (OSError, yaml.YAMLError) as exc:
        raise ScenarioError(f"cannot load {scenario_path}: {exc}") from exc
    scenario = Scenario.from_mapping(raw, str(scenario_path))
    dataset = Path(scenario.workload.dataset)
    if scenario.measurement_tool == GUIDELLM_TOOL:
        dataset = Path("synthetic")
    else:
        if not dataset.is_absolute():
            dataset = (scenario_path.parent / dataset).resolve()
        if not dataset.is_file():
            raise ScenarioError(f"workload dataset does not exist: {dataset}")
    return Scenario(
        **{
            **scenario.__dict__,
            "workload": Workload(
                **{**scenario.workload.__dict__, "dataset": str(dataset)}
            ),
        }
    )
