import json
from pathlib import Path

import pytest
from inferscale_bench.workload import load_messages


def test_workload_cycles_deterministically(tmp_path: Path) -> None:
    path = tmp_path / "workload.jsonl"
    rows = [
        {"messages": [{"role": "user", "content": "a"}]},
        {"messages": [{"role": "user", "content": "b"}]},
    ]
    path.write_text("\n".join(json.dumps(row) for row in rows), encoding="utf-8")
    first = load_messages(str(path), 5, seed=4)
    second = load_messages(str(path), 5, seed=4)
    assert first == second
    assert len(first) == 5


def test_workload_rejects_invalid_roles(tmp_path: Path) -> None:
    path = tmp_path / "workload.jsonl"
    path.write_text('{"messages":[{"role":"tool","content":"no"}]}\n', encoding="utf-8")
    with pytest.raises(ValueError, match="role"):
        load_messages(str(path), 1, seed=1)
