from __future__ import annotations

import json
import random
from pathlib import Path
from typing import Any


def load_messages(path: str, count: int, seed: int) -> list[list[dict[str, str]]]:
    records: list[list[dict[str, str]]] = []
    with Path(path).open(encoding="utf-8") as handle:
        for line_number, line in enumerate(handle, 1):
            if not line.strip():
                continue
            try:
                raw: Any = json.loads(line)
            except json.JSONDecodeError as exc:
                raise ValueError(f"invalid JSONL at line {line_number}: {exc}") from exc
            messages = raw.get("messages") if isinstance(raw, dict) else None
            if not isinstance(messages, list) or not messages:
                raise ValueError(
                    f"line {line_number} requires a non-empty messages array"
                )
            normalized: list[dict[str, str]] = []
            for message in messages:
                if not isinstance(message, dict) or message.get("role") not in {
                    "system",
                    "user",
                    "assistant",
                }:
                    raise ValueError(f"line {line_number} has an invalid message role")
                content = message.get("content")
                if not isinstance(content, str) or not content:
                    raise ValueError(f"line {line_number} has empty message content")
                normalized.append({"role": str(message["role"]), "content": content})
            records.append(normalized)
    if not records:
        raise ValueError("workload contains no requests")
    rng = random.Random(seed)
    rng.shuffle(records)
    return [records[index % len(records)] for index in range(count)]
