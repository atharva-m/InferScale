#!/usr/bin/env python3
"""Render measured InferScale result artifacts as Markdown comparisons."""

from __future__ import annotations

import argparse
from pathlib import Path
from typing import Any

from comparison import (
    ResultArtifact,
    group_compatible,
    load_results,
    scaling_comparisons,
)


def display(number: Any, digits: int = 2) -> str:
    return "n/a" if number is None else f"{float(number):.{digits}f}"


def cell(value: Any) -> str:
    return (
        str(value if value not in (None, "") else "n/a")
        .replace("\\", "\\\\")
        .replace("|", "\\|")
        .replace("\n", "<br>")
    )


def backend_label(result: ResultArtifact) -> str:
    deployment, provenance = result.deployment, result.provenance
    runtime = provenance.get("runtime_version") or ""
    digest = (
        provenance.get("runtime_image_digest")
        or provenance.get("container_image_digest")
        or ""
    )
    identity = " ".join(
        part for part in (str(runtime), str(digest)[:19] if digest else "") if part
    )
    backend = f"{deployment.get('backend', 'n/a')} {deployment.get('backend_version', '')}".strip()
    return f"{backend} / {identity}" if identity else backend


def with_baseline(
    results: list[ResultArtifact], baseline: str | None
) -> list[ResultArtifact]:
    if not baseline or any(result.source == Path(baseline) for result in results):
        return results
    return [*results, *load_results([baseline])]


def default_report(results: list[ResultArtifact]) -> list[str]:
    lines = [
        "# InferScale measured benchmark comparison",
        "",
        "| Scenario | Backend | P95 TTFT (ms) | P95 TPOT (ms) | Output tok/s | SLO attainment (%) | Cost / 1M output tokens |",
        "|---|---:|---:|---:|---:|---:|---:|",
    ]
    for result in results:
        attainment = result.slo_attainment
        scenario = result.scenario
        lines.append(
            "| "
            + " | ".join(
                [
                    cell(scenario.get("name")),
                    cell(backend_label(result)),
                    display(result.p95_ttft_ms),
                    display(result.p95_tpot_ms),
                    display(result.output_tokens_per_second),
                    display(attainment * 100 if attainment is not None else None, 1),
                    display(result.cost_per_million_output_tokens, 4),
                ]
            )
            + " |"
        )
    return lines


def rich_report(
    results: list[ResultArtifact], axis: str, baseline: str | None
) -> list[str]:
    lines = [f"# InferScale matched {axis} benchmark comparison", ""]
    for group_index, group in enumerate(group_compatible(results, axis), 1):
        first = group[0]
        lines.extend(
            [
                f"## Compatibility group {group_index}: "
                f"{cell(first.deployment.get('model'))} / "
                f"{cell(first.deployment.get('gpu_type'))}",
                "",
            ]
        )
        headings = [
            "Source", "Scenario", "Backend/runtime", "GPU / TP", "Concurrency",
            "P95 TTFT ms", "P95 TPOT ms", "Output tok/s", "SLO %", "GPU-hours",
            "Input cost / 1M", "Output cost / 1M", "Commit", "Provider", "GPU $/hour",
        ]
        comparisons = scaling_comparisons(group, baseline) if axis == "gpu_scaling" else []
        by_run = {id(item.run): item for item in comparisons}
        if axis == "gpu_scaling":
            headings.extend(["Baseline source", "Speedup", "Efficiency %"])
        lines.extend(["| " + " | ".join(headings) + " |", "|" + "---|" * len(headings)])
        for result in group:
            deployment = result.deployment
            workload = result.workload
            provenance = result.provenance
            attainment = result.slo_attainment
            row = [
                cell(result.source), cell(result.scenario.get("name")), cell(backend_label(result)),
                cell(
                    f"{deployment.get('gpu_count', 'n/a')} / "
                    f"{deployment.get('tensor_parallelism', 'n/a')}"
                ),
                cell(workload.get("concurrency")), display(result.p95_ttft_ms),
                display(result.p95_tpot_ms), display(result.output_tokens_per_second),
                display(attainment * 100 if attainment is not None else None, 1),
                display(result.gpu_hours, 4),
                display(result.cost_per_million_input_tokens, 4),
                display(result.cost_per_million_output_tokens, 4),
                cell(provenance.get("git_commit")),
                cell(provenance.get("provider")), display(provenance.get("gpu_hourly_price"), 4),
            ]
            if axis == "gpu_scaling":
                comparison = by_run[id(result)]
                row.extend(
                    [
                        cell(comparison.baseline.source if comparison.baseline else None),
                        display(comparison.throughput_speedup),
                        display(
                            comparison.scaling_efficiency * 100
                            if comparison.scaling_efficiency is not None
                            else None,
                            1,
                        ),
                    ]
                )
            lines.append("| " + " | ".join(row) + " |")
        lines.append("")
    return lines


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("results", nargs="+", help="benchmark result JSON files")
    parser.add_argument("--output", required=True, help="Markdown output path")
    parser.add_argument(
        "--comparison", choices=("backend", "gpu_scaling", "concurrency")
    )
    parser.add_argument(
        "--baseline", help="explicit 1-GPU reference artifact for gpu_scaling"
    )
    args = parser.parse_args()
    if args.baseline and args.comparison != "gpu_scaling":
        parser.error("--baseline requires --comparison gpu_scaling")
    results = with_baseline(load_results(args.results), args.baseline)
    lines = (
        rich_report(results, args.comparison, args.baseline)
        if args.comparison
        else default_report(results)
    )
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
