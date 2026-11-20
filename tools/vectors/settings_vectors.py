#!/usr/bin/env python3
"""Generate golden vectors for internal/settings from the Python ConfigStore on main.

    PYTHONPATH=<main checkout> <main>/.venv/bin/python tools/vectors/settings_vectors.py > internal/settings/testdata/python_vectors.json
"""
import json, os, sys, tempfile
from pathlib import Path

os.environ["CHATGPT2API_AUTH_KEY"] = "vector-key"
for name in ("CHATGPT2API_BASE_URL", "CHATGPT2API_SENTINEL_SOLVER_URL", "CHATGPT2API_SENTINEL_SOLVER_KEY"):
    os.environ.pop(name, None)
from services.config import ConfigStore  # noqa: E402

ODD = ["abc", "12", " 7 ", "1.5", 7.9, True, False, None, -5, 0, 99999, "", "  ", "on", "off", "yes", "0", [], {}]
INT_KEYS = ["refresh_account_interval_minute", "image_retention_days", "image_poll_timeout_secs", "proxy_max_accounts",
            "image_failover_secs", "image_account_concurrency", "image_ref_fit_budget_bytes", "image_ref_fit_max_edge",
            "image_req_fit_budget_bytes", "image_upscale_target_edge"]
FLOAT_KEYS = ["image_poll_interval_secs", "image_poll_initial_wait_secs", "image_settle_secs"]
BOOL_KEYS = ["image_failover_enabled", "image_parallel_generation", "image_remove_conversation_after_result",
             "image_remove_conversation_always", "image_ref_fit_enabled", "image_req_fit_enabled", "image_upscale_enabled",
             "image_exact_size_enabled", "image_edit_upload_cooldown_enabled", "auto_remove_invalid_accounts",
             "auto_remove_rate_limited_accounts", "auto_relogin_after_refresh", "image_settle_enabled",
             "image_check_before_hit_enabled"]
STR_KEYS = ["image_upscale_method", "global_system_prompt", "default_upstream_model_name", "default_thinking_effort",
            "sentinel_solver_url", "base_url"]


def store(data):
    path = Path(tempfile.mkdtemp()) / "config.json"
    path.write_text(json.dumps(data))
    return ConfigStore(path), path


cases = [{"name": "empty", "config": {}}]
for key in INT_KEYS + FLOAT_KEYS + BOOL_KEYS + STR_KEYS:
    for i, value in enumerate(ODD):
        cases.append({"name": f"{key}#{i}", "config": {key: value}})
for i, value in enumerate(["MAX", "Extended", "lanczos", "MODEL", "x", "  auto  "]):
    cases.append({"name": f"effort#{i}", "config": {"default_thinking_effort": value, "image_upscale_method": value}})
cases += [
    {"name": "lists", "config": {"log_levels": ["INFO", " debug ", "trace", None, 3], "sensitive_words": [" a ", "", None, "b", 0]}},
    {"name": "lists-bad", "config": {"log_levels": "info", "sensitive_words": {"a": 1}}},
    {"name": "ai-review", "config": {"ai_review": {"enabled": True, "model": "m"}}},
    {"name": "ai-review-bad", "config": {"ai_review": ["x"]}},
    {"name": "raw-keys-kept", "config": {"auth-key": "secret", "sentinel_solver_key": "s", "custom": {"x": 1}, "proxy": " http://p "}},
    {"name": "backup", "config": {"backup": {"enabled": "yes", "prefix": "/a/b/", "interval_minutes": "0", "rotation_keep": -1,
                                              "include": {"images": "on", "logs": "off", "config": None}, "account_id": " id "}}},
    {"name": "backup-bad", "config": {"backup": "x"}},
    {"name": "storage", "config": {"image_storage": {"enabled": True, "mode": "BOTH", "webdav_url": "https://d/ ", "webdav_root_path": "//r//"}}},
    {"name": "storage-disabled", "config": {"image_storage": {"enabled": False, "mode": "webdav"}}},
    {"name": "storage-bad-mode", "config": {"image_storage": {"enabled": "1", "mode": "ftp", "webdav_root_path": "/"}}},
    {"name": "cache", "config": {"chat_completion_cache": {"enabled": "off", "ttl_seconds": "-3", "max_entries": 0, "stream_cache": "maybe"}}},
    {"name": "runtime", "config": {"proxy_runtime": {"enabled": "true", "egress_mode": "SINGLE_PROXY", "proxy_url": " u ",
        "reset_session_status_codes": [403, "429", 99, 600, True, "x", 403, 502.7],
        "clearance": {"mode": "FlareSolverr", "cf_cookies": "a=b", "cf_clearance": "", "user_agent": " ", "timeout_sec": 0, "refresh_interval": 10}}}},
    {"name": "runtime-empty-codes", "config": {"proxy_runtime": {"reset_session_status_codes": [], "egress_mode": "tunnel", "clearance": "x"}}},
    {"name": "apps", "config": {"third_party_apps": {"infinite_canvas": {"enabled": "on", "url": ""}}}},
    {"name": "apps-bad", "config": {"third_party_apps": []}},
]

updates = [
    {"name": "update-merge", "config": {"a": 1, "image_retention_days": 3}, "update": {"image_retention_days": 15, "b": "x", "backup_state": {"x": 1}}},
    {"name": "update-storage-invalid", "config": {}, "update": {"image_storage": {"enabled": True, "webdav_url": ""}}},
    {"name": "update-storage-no-password", "config": {}, "update": {"image_storage": {"enabled": True, "webdav_url": "https://d"}}},
    {"name": "update-storage-ok", "config": {}, "update": {"image_storage": {"enabled": True, "webdav_url": "https://d", "webdav_password": "p"}}},
    {"name": "update-runtime-keeps-cookies", "config": {"proxy_runtime": {"clearance": {"cf_cookies": "old=1", "cf_clearance": "cc"}}},
     "update": {"proxy_runtime": {"enabled": True, "clearance": {"cf_cookies": "", "has_cf_cookies": True, "cf_clearance": "", "has_cf_clearance": False}}}},
    {"name": "update-runtime-not-dict", "config": {"proxy_runtime": {"clearance": {"cf_cookies": "old=1"}}}, "update": {"proxy_runtime": "x"}},
    {"name": "update-sections", "config": {}, "update": {"backup": {}, "chat_completion_cache": None, "third_party_apps": {"infinite_canvas": {"enabled": True}}}},
]

out = []
for case in cases:
    s, _ = store(case["config"])
    out.append({**case, "get": s.get(), "keep_from_2026_11_20": s.image_retention_keep_from(__import__("datetime").date(2026, 11, 20))})
for case in updates:
    s, path = store(case["config"])
    try:
        result = s.update(case["update"])
        out.append({**case, "result": result, "saved": json.loads(path.read_text())})
    except ValueError as exc:
        out.append({**case, "error": str(exc)})
json.dump(out, sys.stdout, ensure_ascii=False, indent=1, sort_keys=True)
print()
