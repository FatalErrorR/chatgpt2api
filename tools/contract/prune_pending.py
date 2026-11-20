#!/usr/bin/env python3
"""Drop the routes TestPythonRoutesAreServed reports as "served now" from docs/contract/pending-routes.txt.

    go test ./internal/app -run TestPythonRoutesAreServed 2>&1 | python3 tools/contract/prune_pending.py
"""
import re
import sys
from pathlib import Path

PENDING = Path(__file__).resolve().parents[2] / "docs" / "contract" / "pending-routes.txt"


def norm(line: str) -> str:
    method, path = line.strip().split(" ", 1)
    path = re.sub(r"\{([a-zA-Z_]+)(:path)?\}", lambda m: "*" if m.group(2) else ":", path)
    return f"{method} {path}"


served, in_block = set(), False
for raw in sys.stdin:
    if "served now" in raw:
        in_block = True
        continue
    if in_block:
        text = raw.strip()
        if re.match(r"^(GET|POST|PUT|PATCH|DELETE) /", text):
            served.add(text)
        else:
            in_block = False
kept, dropped = [], []
for line in PENDING.read_text().splitlines():
    if line.strip() and not line.startswith("#") and norm(line) in served:
        dropped.append(line)
    else:
        kept.append(line)
PENDING.write_text("\n".join(kept) + "\n")
print(f"dropped {len(dropped)}:", *dropped, sep="\n  ")
