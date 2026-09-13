"""Compatibility CLI for `python -m benchmarks.runner.cli`."""

from __future__ import annotations

import sys

from inferscale_bench.cli import main


def normalized_args(args: list[str]) -> list[str]:
    if len(args) >= 2 and args[0] == "run" and not args[1].startswith("-"):
        return ["run", "--scenario", args[1], *args[2:]]
    return args


if __name__ == "__main__":
    raise SystemExit(main(normalized_args(sys.argv[1:])))
