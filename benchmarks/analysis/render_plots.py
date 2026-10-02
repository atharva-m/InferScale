#!/usr/bin/env python3
"""Export matched v1 benchmark comparisons as headless Matplotlib figures."""

from __future__ import annotations

import argparse
import math
from pathlib import Path
from typing import Callable

from comparison import (
    ResultArtifact,
    group_compatible,
    load_results,
    scaling_comparisons,
)


def backend_label(result: ResultArtifact) -> str:
    deployment, provenance = result.deployment, result.provenance
    backend = (
        f"{deployment.get('backend', 'n/a')} "
        f"{deployment.get('backend_version', '')}"
    ).strip()
    runtime = provenance.get("runtime_version") or ""
    digest = (
        provenance.get("runtime_image_digest")
        or provenance.get("container_image_digest")
        or ""
    )
    identity = " ".join(
        part for part in (str(runtime), str(digest)[:19] if digest else "") if part
    )
    return f"{backend} / {identity}" if identity else backend


def include_baseline(
    results: list[ResultArtifact], baseline: str | None
) -> list[ResultArtifact]:
    if not baseline or any(result.source == Path(baseline) for result in results):
        return results
    return [*results, *load_results([baseline])]


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("results", nargs="+", help="benchmark result JSON files")
    parser.add_argument(
        "--comparison",
        required=True,
        choices=("backend", "gpu_scaling", "concurrency"),
    )
    parser.add_argument("--output-dir", required=True, help="directory for exported figures")
    parser.add_argument(
        "--baseline", help="explicit 1-GPU reference artifact for gpu_scaling"
    )
    parser.add_argument("--format", choices=("svg", "png"), default="svg")
    args = parser.parse_args()
    if args.baseline and args.comparison != "gpu_scaling":
        parser.error("--baseline requires --comparison gpu_scaling")

    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    results = include_baseline(load_results(args.results), args.baseline)
    groups = group_compatible(results, args.comparison)
    output_dir = Path(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)

    metrics: list[tuple[str, Callable[[ResultArtifact], float | None]]] = [
        ("P95 TTFT (ms)", lambda result: result.p95_ttft_ms),
        ("P95 TPOT (ms)", lambda result: result.p95_tpot_ms),
        (
            "Output throughput (tokens/s)",
            lambda result: result.output_tokens_per_second,
        ),
        (
            "Input token cost / 1M",
            lambda result: result.cost_per_million_input_tokens,
        ),
        (
            "Output token cost / 1M",
            lambda result: result.cost_per_million_output_tokens,
        ),
    ]
    if args.comparison == "gpu_scaling":
        metrics.append(("GPU scaling efficiency (%)", lambda _result: None))

    for group_index, group in enumerate(groups, 1):
        scaling = (
            scaling_comparisons(group, args.baseline)
            if args.comparison == "gpu_scaling"
            else []
        )
        efficiencies = {id(item.run): item.scaling_efficiency for item in scaling}
        series: list[tuple[str, list[tuple[ResultArtifact, float]]]] = []
        for label, getter in metrics:
            points = []
            for result in group:
                value = efficiencies.get(id(result))
                if label.endswith("(%)"):
                    value = value * 100 if value is not None else None
                else:
                    value = getter(result)
                if value is not None and math.isfinite(value):
                    points.append((result, value))
            if points:
                series.append((label, points))
        if not series:
            continue

        columns = 2
        rows = math.ceil(len(series) / columns)
        figure, axes = plt.subplots(
            rows, columns, figsize=(12, 4 * rows), squeeze=False
        )
        backend_positions: dict[str, int] = {}
        for metric_index, (label, points) in enumerate(series):
            axis = axes[metric_index // columns][metric_index % columns]
            for repeat, (result, value) in enumerate(points):
                if args.comparison == "gpu_scaling":
                    x = result.deployment.get("gpu_count")
                elif args.comparison == "concurrency":
                    x = result.workload.get("concurrency")
                else:
                    identity = backend_label(result)
                    x = backend_positions.setdefault(identity, len(backend_positions))
                if not isinstance(x, (int, float)) or isinstance(x, bool):
                    continue
                axis.scatter(x, value, s=42)
                source = (
                    result.source.name
                    if result.source
                    else str(result.scenario.get("name", "run"))
                )
                axis.annotate(
                    source,
                    (x, value),
                    xytext=(0, 7 + (repeat % 3) * 7),
                    textcoords="offset points",
                    fontsize=7,
                    ha="center",
                )
            axis.set_title(label)
            axis.grid(True, alpha=0.25)
            if args.comparison == "gpu_scaling":
                axis.set_xlabel("GPU count")
            elif args.comparison == "concurrency":
                axis.set_xlabel("Concurrency")
            else:
                axis.set_xlabel("Backend / runtime")
                axis.set_xticks(
                    list(backend_positions.values()),
                    list(backend_positions.keys()),
                    rotation=25,
                    ha="right",
                )
        for unused in range(len(series), rows * columns):
            figure.delaxes(axes[unused // columns][unused % columns])
        figure.suptitle(f"Matched {args.comparison} comparison · group {group_index}")
        figure.tight_layout()
        figure.savefig(
            output_dir / f"group-{group_index:03d}.{args.format}",
            format=args.format,
            bbox_inches="tight",
        )
        plt.close(figure)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
