"""Production-shaped data for the frontend walkthrough (Python data directory format).

Shapes taken from production (no values copied): about 130 accounts, most of them limited or
abnormal; 100 unnamed proxies on one host (closed local ports, so every attempt fails at once on any machine), two disabled; hundreds of images over
three weeks with a few tags; thousands of log lines. Deterministic (fixed seed) so the Go and
Python instances see the same data.

    python3 mockdata.py <data dir>
"""
from __future__ import annotations

import hashlib
import json
import random
import struct
import sys
import uuid
import zlib
from datetime import datetime, timedelta
from pathlib import Path

rnd = random.Random(20261120)
NOW = datetime(2026, 11, 20, 18, 0, 0)


def png(seed: int, w: int = 48, h: int = 32) -> bytes:
    raw = b"".join(b"\x00" + b"".join(bytes(((x * 5 + seed) % 256, (y * 7 + seed) % 256, (seed * 3) % 256)) for x in range(w)) for y in range(h))
    chunk = lambda t, d: struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d))
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0)) + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b"")


def main(target: Path) -> None:
    data = target
    (data / "images").mkdir(parents=True, exist_ok=True)

    proxies = [{"url": f"http://user{i:02d}:pw@127.0.0.1:{10000 + i}", "name": "", "disabled": i % 10 == 3} for i in range(100)]
    (data / "proxies.json").write_text(json.dumps(proxies, ensure_ascii=False, indent=2))

    statuses = ["正常"] * 18 + ["限流"] * 34 + ["异常"] * 66 + ["禁用"] * 12
    errors = ["Cloudflare 403", "token_invalidated", "account_deactivated", "refresh_token 过期", "代理连接超时", ""]
    accounts = []
    for i, status in enumerate(statuses):
        kind = rnd.choice(["free", "free", "Plus", "Team", "Pro"])
        account = {
            "access_token": "tok-" + hashlib.sha1(f"acct{i}".encode()).hexdigest(),
            "type": kind, "status": status, "quota": rnd.randint(0, 40) if status == "正常" else 0,
            "email": f"user{i:03d}@example-mail.com", "user_id": f"user-{i:04d}", "proxy": proxies[i % 100]["url"],
            "success": rnd.randint(0, 900), "fail": rnd.randint(0, 120), "created_at": (NOW - timedelta(days=rnd.randint(1, 60))).strftime("%Y-%m-%d %H:%M:%S"),
            "last_used_at": (NOW - timedelta(minutes=rnd.randint(1, 5000))).strftime("%Y-%m-%d %H:%M:%S"),
            "restore_at": (NOW + timedelta(hours=rnd.randint(1, 30))).strftime("%Y-%m-%dT%H:%M:%SZ") if status == "限流" else None,
            "default_model_slug": "auto", "edit_upload_blocked_until": None, "invalid_count": 0 if status != "异常" else rnd.randint(1, 5),
            "last_invalid_at": None, "last_refresh_error": rnd.choice(errors) if status == "异常" else None, "last_refresh_error_at": None,
            "last_token_refresh_at": None, "last_token_refresh_error": None, "last_token_refresh_error_at": None,
            "limits_progress": [{"feature_name": "image_gen", "remaining": 0, "reset_after": "2026-11-21T03:00:00Z"}] if status == "限流" else [],
            "source_type": rnd.choice(["web", "codex", "password", "oauth_login"]),
        }
        if status == "异常" and i % 3 == 0:
            account["password"] = "pw"
        accounts.append(account)
    (data / "accounts.json").write_text(json.dumps(accounts, ensure_ascii=False, indent=2))
    (data / ".cumulative_total").write_text(str(len(accounts) + 47))

    keys = [{"id": f"key{i}", "name": f"用户{i}", "role": "user", "key_hash": hashlib.sha256(f"user-key-{i}".encode()).hexdigest(),
             "enabled": i != 2, "created_at": "2026-10-0%dT08:00:00+00:00" % (i + 1), "last_used_at": None} for i in range(3)]
    (data / "auth_keys.json").write_text(json.dumps({"items": keys}, ensure_ascii=False, indent=2))

    items, tags = {}, {}
    for i in range(5200):
        day = NOW - timedelta(days=rnd.randint(0, 20), minutes=rnd.randint(0, 1400))
        payload = png(i)
        rel = f"{day:%Y/%m/%d}/{int(day.timestamp())}_{hashlib.md5(payload).hexdigest()}.png"
        path = data / "images" / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(payload)
        items[rel] = {"rel": rel, "path": rel, "name": rel.rsplit("/", 1)[-1], "date": f"{day:%Y-%m-%d}", "size": len(payload),
                      "created_at": f"{day:%Y-%m-%d %H:%M:%S}", "storage": "local", "local": True, "webdav": False, "remote_url": "",
                      "width": 48, "height": 32}
        if i % 97 == 0:
            tags[rel] = rnd.sample(["人像", "风景", "产品图", "海报"], rnd.randint(1, 2))
    (data / "image_index.json").write_text(json.dumps({"items": items}, ensure_ascii=False, indent=2))
    (data / "image_tags.json").write_text(json.dumps(tags, ensure_ascii=False, indent=2))

    lines, days = [], {}
    for i in range(4000):
        moment = NOW - timedelta(minutes=rnd.randint(0, 60 * 24 * 40))
        if rnd.random() < 0.7:
            endpoint = rnd.choice(["/v1/images/generations", "/v1/images/edits", "/v1/chat/completions"])
            ok = rnd.random() < 0.8
            urls = [f"http://localhost/images/x{i}.png"] if ok and "images" in endpoint else []
            detail = {"key_id": "admin", "key_name": "管理员", "role": "admin", "endpoint": endpoint, "model": "gpt-image-2",
                      "started_at": f"{moment:%Y-%m-%d %H:%M:%S}", "ended_at": f"{moment:%Y-%m-%d %H:%M:%S}", "duration_ms": rnd.randint(2000, 90000),
                      "status": "success" if ok else "failed", "request_text": "一只猫" * rnd.randint(1, 30),
                      "account_email": accounts[rnd.randrange(len(accounts))]["email"]}
            if urls:
                detail["urls"] = urls
                cst = moment + timedelta(hours=0)
                bucket = days.setdefault(f"{cst:%Y-%m-%d}", {"edits": 0, "generations": 0})
                bucket["edits" if endpoint.endswith("edits") else "generations"] += 1
            if not ok:
                detail["error"] = rnd.choice(["Cloudflare 403", "图片生成超时，未拿到结果", "no available image quota"])
            summary = {"/v1/images/generations": "文生图", "/v1/images/edits": "图生图", "/v1/chat/completions": "文本生成"}[endpoint] + ("调用完成" if ok else "调用失败")
            lines.append((moment, {"id": uuid.UUID(int=rnd.getrandbits(128)).hex, "time": f"{moment:%Y-%m-%d %H:%M:%S}", "type": "call", "summary": summary, "detail": detail}))
        else:
            lines.append((moment, {"id": uuid.UUID(int=rnd.getrandbits(128)).hex, "time": f"{moment:%Y-%m-%d %H:%M:%S}", "type": "account",
                                   "summary": "更新账号", "detail": {"source": "refresh", "status": "成功"}}))
    lines.sort(key=lambda item: item[0])
    (data / "logs.jsonl").write_text("".join(json.dumps(item, ensure_ascii=False, separators=(",", ":")) + "\n" for _, item in lines))
    (data / "image_output_counts.json").write_text(json.dumps({"days": days}, separators=(",", ":")))
    print(f"{len(accounts)} accounts, {len(proxies)} proxies, {len(items)} images, {len(lines)} log lines")


if __name__ == "__main__":
    main(Path(sys.argv[1]))
