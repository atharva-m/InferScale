#!/usr/bin/env python3
"""Render measured InferScale result JSON files into a Markdown comparison table."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any


def value(payload: dict[str, Any], *path: str) -> Any:
    current: Any = payload
    for part in path:
        current = current.get(part) if isinstance(current, dict) else None
    return current


def display(number: Any, digits: int = 2) -> str:
    return "n/a" if number is None else f"{float(number):.{digits}f}"


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("results", nargs="+", help="benchmark result JSON files")
    parser.add_argument("--output", required=True, help="Markdown output path")
    args = parser.parse_args()
    rows = []
    for path_text in args.results:
        path = Path(path_text)
        payload = json.loads(path.read_text(encoding="utf-8"))
        if payload.get("schema_version") != 1:
            raise ValueError(f"unsupported result schema in {path}")
        rows.append(
            [
                str(value(payload, "scenario", "name")),
                str(value(payload, "scenario", "deployment", "backend")),
                display(value(payload, "aggregate", "ttft_ms", "p95")),
                display(value(payload, "aggregate", "tpot_ms", "p95")),
                display(value(payload, "aggregate", "output_tokens_per_s")),
                display(
                    value(payload, "aggregate", "slo_attainment") * 100
                    if value(payload, "aggregate", "slo_attainment") is not None
                    else None,
                    1,
                ),
                display(value(payload, "cost", "cost_per_1m_output_tokens"), 4),
            ]
        )
    lines = [
        "# InferScale measured benchmark comparison",
        "",
        "All values below were loaded from runner-generated result artifacts; no placeholder performance numbers are emitted.",
        "",
        "| Scenario | Backend | P95 TTFT (ms) | P95 TPOT (ms) | Output tok/s | SLO attainment (%) | Cost / 1M output tokens |",
        "|---|---:|---:|---:|---:|---:|---:|",
    ]
    lines.extend("| " + " | ".join(row) + " |" for row in rows)
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text("\n".join(lines) + "\n", encoding="utf-8")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
