#!/usr/bin/env python3
"""Contract check between the Python version (main) and the Go port (go-v2).

Runs the same ordered scenarios against a server and records, per scenario, the status code and
the *shape* of the JSON response (field names and value types). Scenarios marked "values": true
also keep the body, minus the keys listed in "ignore". Two recordings are then compared.

    python3 tools/contract/contract.py record --base http://127.0.0.1:8200 --admin-key K --out /tmp/py
    python3 tools/contract/contract.py record --base http://127.0.0.1:3100 --admin-key K --out /tmp/go
    python3 tools/contract/contract.py diff /tmp/py /tmp/go

Scenarios live in scenarios.json next to this file. Fields:
  name      unique id, also the file name of the recording
  method    GET / POST / DELETE …
  path      may contain {var} placeholders filled from earlier responses (see "save")
  auth      "admin" (default), "user", "none" or "bad" (a wrong key)
  body      JSON body
  values    true to compare the body itself, not only its shape
  ignore    keys removed anywhere in the body before comparing values
  save      {"var": "json.path"} stores a value from this response for later paths/bodies
  wait      seconds to sleep before the request (for background jobs)
Only the standard library is used.
"""
from __future__ import annotations

import argparse
import json
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

HERE = Path(__file__).resolve().parent


def shape(value):
    if isinstance(value, dict):
        return {key: shape(value[key]) for key in sorted(value)}
    if isinstance(value, list):
        shapes = []
        for item in value:
            item_shape = shape(item)
            if item_shape not in shapes:
                shapes.append(item_shape)
        merged = merge_shapes(shapes)
        return [merged] if merged is not None else []
    if isinstance(value, bool):
        return "bool"
    if isinstance(value, (int, float)):
        return "num"
    if value is None:
        return "null"
    return "str"


def merge_shapes(shapes):
    """List items of one type merge into one shape: dicts take the union of keys."""
    if not shapes:
        return None
    if all(isinstance(item, dict) for item in shapes):
        merged: dict = {}
        for item in shapes:
            for key, value in item.items():
                if key not in merged or merged[key] == "null":
                    merged[key] = value
        return {key: merged[key] for key in sorted(merged)}
    unique = []
    for item in shapes:
        if item not in unique:
            unique.append(item)
    return unique[0] if len(unique) == 1 else {"oneof": sorted(unique, key=json.dumps)}


def strip(value, ignore: set[str]):
    if isinstance(value, dict):
        return {key: strip(item, ignore) for key, item in value.items() if key not in ignore}
    if isinstance(value, list):
        return [strip(item, ignore) for item in value]
    return value


def lookup(data, path: str):
    for part in path.split("."):
        if isinstance(data, list):
            data = data[int(part)]
        else:
            data = data[part]
    return data


def fill(value, saved: dict):
    if isinstance(value, str):
        for key, item in saved.items():
            value = value.replace("{" + key + "}", str(item))
        return value
    if isinstance(value, dict):
        return {key: fill(item, saved) for key, item in value.items()}
    if isinstance(value, list):
        return [fill(item, saved) for item in value]
    return value


def request(base: str, method: str, path: str, token: str | None, body):
    headers = {"Accept": "application/json"}
    data = None
    if token:
        headers["Authorization"] = "Bearer " + token
    if body is not None:
        data = json.dumps(body).encode()
        headers["Content-Type"] = "application/json"
    path = urllib.parse.quote(path, safe="/?=&%:,+")
    req = urllib.request.Request(base.rstrip("/") + path, data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=180) as response:
            return response.status, response.headers.get("Content-Type", ""), response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.headers.get("Content-Type", ""), error.read()


def record(args) -> int:
    scenarios = json.loads((HERE / "scenarios.json").read_text())
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    keys = {"admin": args.admin_key, "user": args.user_key, "none": None, "bad": "definitely-not-a-valid-key"}
    saved: dict = {}
    names = set()
    for scenario in scenarios:
        name = scenario["name"]
        if name in names:
            raise SystemExit(f"duplicate scenario name {name}")
        names.add(name)
        if scenario.get("wait"):
            time.sleep(float(scenario["wait"]))
        auth = scenario.get("auth", "admin")
        token = saved.get("user_key") if auth == "user" and not keys["user"] else keys[auth]
        path = fill(scenario["path"], saved)
        body = fill(scenario.get("body"), saved)
        status, content_type, raw = request(args.base, scenario["method"], path, token, body)
        entry: dict = {"name": name, "method": scenario["method"], "path": scenario["path"], "status": status}
        try:
            parsed = json.loads(raw) if raw else None
            entry["shape"] = shape(parsed)
            if scenario.get("values"):
                entry["body"] = strip(parsed, set(scenario.get("ignore", [])))
        except ValueError:
            parsed = None
            entry["content_type"] = content_type.split(";")[0]
            entry["bytes"] = "empty" if not raw else "non-empty"
        for var, json_path in (scenario.get("save") or {}).items():
            try:
                saved[var] = lookup(parsed, json_path)
            except (KeyError, IndexError, TypeError, ValueError):
                print(f"  ! {name}: could not save {var} from {json_path}", file=sys.stderr)
        (out / f"{name}.json").write_text(json.dumps(entry, ensure_ascii=False, indent=1, sort_keys=True))
        print(f"{status} {scenario['method']:6} {path}  [{name}]")
    return 0


def diff_shapes(left, right, where: str, problems: list[str]) -> None:
    if isinstance(left, dict) and isinstance(right, dict):
        for key in sorted(set(left) | set(right)):
            if key not in right:
                problems.append(f"{where}.{key}: only in first")
            elif key not in left:
                problems.append(f"{where}.{key}: only in second")
            else:
                diff_shapes(left[key], right[key], f"{where}.{key}", problems)
    elif isinstance(left, list) and isinstance(right, list):
        if left and right:
            diff_shapes(left[0], right[0], where + "[]", problems)
    elif left != right and "null" not in (left, right):
        problems.append(f"{where}: {json.dumps(left)} vs {json.dumps(right)}")


def diff(args) -> int:
    first, second = Path(args.first), Path(args.second)
    failed = 0
    for path in sorted(first.glob("*.json")):
        other = second / path.name
        a = json.loads(path.read_text())
        if not other.exists():
            print(f"MISSING  {a['name']}")
            failed += 1
            continue
        b = json.loads(other.read_text())
        problems: list[str] = []
        if a["status"] != b["status"]:
            problems.append(f"status {a['status']} vs {b['status']}")
        if "shape" in a or "shape" in b:
            diff_shapes(a.get("shape"), b.get("shape"), "$", problems)
        else:
            if a.get("content_type") != b.get("content_type"):
                problems.append(f"content-type {a.get('content_type')} vs {b.get('content_type')}")
        if "body" in a and a.get("body") != b.get("body"):
            problems.append("body differs:\n      " + json.dumps(a.get("body"), ensure_ascii=False)[:300]
                            + "\n      " + json.dumps(b.get("body"), ensure_ascii=False)[:300])
        if problems:
            failed += 1
            print(f"FAIL     {a['name']} ({a['method']} {a['path']})")
            for problem in problems[:12]:
                print("    " + problem)
        else:
            print(f"ok       {a['name']}")
    print(f"\n{failed} of {len(list(first.glob('*.json')))} scenarios differ")
    return 1 if failed else 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)
    rec = sub.add_parser("record")
    rec.add_argument("--base", required=True)
    rec.add_argument("--admin-key", required=True)
    rec.add_argument("--user-key", default="")
    rec.add_argument("--out", required=True)
    dif = sub.add_parser("diff")
    dif.add_argument("first")
    dif.add_argument("second")
    args = parser.parse_args()
    return record(args) if args.command == "record" else diff(args)


if __name__ == "__main__":
    sys.exit(main())
