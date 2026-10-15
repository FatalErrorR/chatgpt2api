"""批量协议登录。

输入每行一个账号：`邮箱----密码----TOTP密钥`（TOTP 密钥可省略，账号没开二步验证时不需要）。
后端直接走 OpenAI 的授权协议登录（不开浏览器），登录成功后写入号池并刷新额度。
已存在同邮箱的账号会被原地更新 token，不会重复添加。
"""
from __future__ import annotations

import threading
import time
import uuid
from concurrent.futures import ThreadPoolExecutor
from typing import Any, Callable

from services.account_service import account_service
from utils.helper import anonymize_token
from utils.totp import normalize_totp_secret

SEPARATOR = "----"
MAX_BATCH = 200
CONCURRENCY = 3
_MAX_PROGRESS = 20

_ERROR_TEXT = {
    "invalid_password": "密码错误",
    "need_totp_secret": "账号开启了二步验证，需要提供 TOTP 密钥",
    "invalid_totp_secret": "TOTP 密钥格式无效",
    "need_verification_code": "需要邮箱验证码，协议登录暂不支持",
    "mfa_factor_not_found": "没有找到二步验证方式",
    "mfa_no_auth_code": "二步验证通过后没有拿到授权码",
    "rate_limit_exceeded": "触发频率限制，请稍后再试",
    "unsupported_country_region_territory": "当前出口地区不受支持，请换代理",
    "no_auth_code": "登录后没有拿到授权码",
    "token_exchange_failed": "授权码换取 token 失败",
}


def parse_account_lines(text: str) -> tuple[list[dict[str, Any]], list[dict[str, Any]]]:
    """解析批量文本，返回 (有效条目, 无效行)。同一邮箱只保留第一条。"""
    entries: list[dict[str, Any]] = []
    invalid: list[dict[str, Any]] = []
    seen: set[str] = set()
    for line_no, raw in enumerate(str(text or "").splitlines(), start=1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        parts = [part.strip() for part in line.split(SEPARATOR)]
        if len(parts) < 2:
            invalid.append({"line": line_no, "error": "格式应为 邮箱----密码----TOTP密钥"})
            continue
        email = parts[0]
        if len(parts) >= 3:
            totp_raw, password = parts[-1], SEPARATOR.join(parts[1:-1])
        else:
            totp_raw, password = "", parts[1]
        if "@" not in email or not password:
            invalid.append({"line": line_no, "email": email, "error": "邮箱或密码为空"})
            continue
        totp_secret = ""
        if totp_raw:
            totp_secret = normalize_totp_secret(totp_raw)
            if not totp_secret:
                invalid.append({"line": line_no, "email": email, "error": "TOTP 密钥格式无效"})
                continue
        key = email.lower()
        if key in seen:
            invalid.append({"line": line_no, "email": email, "error": "邮箱重复"})
            continue
        seen.add(key)
        entries.append({"line": line_no, "email": email, "password": password, "totp_secret": totp_secret})
    return entries, invalid


def describe_login_error(result: dict[str, Any]) -> str:
    """把登录失败结果翻译成给人看的短句，不包含密码等敏感内容。"""
    code = str(result.get("error") or "unknown")
    detail = result.get("detail")
    if code == "password_verify_failed_403" and isinstance(detail, dict):
        inner = detail.get("error")
        if isinstance(inner, dict) and inner.get("code") == "account_deactivated":
            return "账号已被停用"
    if code.startswith("mfa_verify_failed_"):
        return f"二步验证码被拒绝（{code.rsplit('_', 1)[-1]}）"
    return _ERROR_TEXT.get(code, code)


class ProtocolLoginService:
    def __init__(self, login: Callable[..., dict] | None = None) -> None:
        self._login = login
        self._lock = threading.Lock()
        self._progress: dict[str, dict[str, Any]] = {}

    def _do_login(self, email: str, password: str, totp_secret: str) -> dict:
        login = self._login or account_service._login_with_password
        return login(email, password, totp_secret)

    # ── 进度 ──────────────────────────────────────────────
    def _purge_locked(self) -> None:
        if len(self._progress) <= _MAX_PROGRESS:
            return
        ordered = sorted(self._progress.items(), key=lambda kv: kv[1]["created_at"])
        for pid, _ in ordered[: len(self._progress) - _MAX_PROGRESS]:
            self._progress.pop(pid, None)

    def _set_item(self, pid: str, index: int, **fields: Any) -> None:
        with self._lock:
            progress = self._progress.get(pid)
            if progress is None:
                return
            progress["items"][index].update(fields)
            items = progress["items"]
            progress["completed"] = sum(1 for item in items if item["status"] in {"成功", "失败"})
            progress["success"] = sum(1 for item in items if item["status"] == "成功")
            progress["failed"] = sum(1 for item in items if item["status"] == "失败")

    def get_progress(self, progress_id: str) -> dict[str, Any] | None:
        with self._lock:
            progress = self._progress.get(progress_id)
            if progress is None:
                return None
            return {**progress, "items": [dict(item) for item in progress["items"]]}

    # ── 执行 ──────────────────────────────────────────────
    def start(self, text: str, proxy: str = "", *, run_async: bool = True) -> dict[str, Any]:
        entries, invalid = parse_account_lines(text)
        if len(entries) > MAX_BATCH:
            raise ValueError(f"一次最多 {MAX_BATCH} 个账号")
        progress_id = str(uuid.uuid4())
        with self._lock:
            self._progress[progress_id] = {
                "id": progress_id,
                "status": "done" if not entries else "running",
                "created_at": time.time(),
                "total": len(entries),
                "completed": 0,
                "success": 0,
                "failed": 0,
                "invalid": invalid,
                "items": [{"email": e["email"], "status": "等待", "error": ""} for e in entries],
            }
            self._purge_locked()
        if entries:
            if run_async:
                threading.Thread(target=self._run, args=(progress_id, entries, proxy.strip()), daemon=True).start()
            else:
                self._run(progress_id, entries, proxy.strip())
        return {"progress_id": progress_id, "total": len(entries), "invalid": invalid}

    def _run(self, progress_id: str, entries: list[dict[str, Any]], proxy: str) -> None:
        try:
            with ThreadPoolExecutor(max_workers=CONCURRENCY) as pool:
                futures = [pool.submit(self._login_one, progress_id, i, entry, proxy) for i, entry in enumerate(entries)]
                for future in futures:
                    future.result()
        finally:
            with self._lock:
                progress = self._progress.get(progress_id)
                if progress is not None:
                    progress["status"] = "done"

    def _login_one(self, progress_id: str, index: int, entry: dict[str, Any], proxy: str) -> None:
        self._set_item(progress_id, index, status="登录中")
        try:
            result = self._do_login(entry["email"], entry["password"], entry["totp_secret"])
            if not result.get("ok"):
                self._set_item(progress_id, index, status="失败", error=describe_login_error(result))
                return
            token = self._save_account(entry, result, proxy)
            self._set_item(progress_id, index, status="成功", error="", token=anonymize_token(token))
        except Exception as exc:  # 单个账号失败不影响其它账号
            self._set_item(progress_id, index, status="失败", error=f"{type(exc).__name__}: {str(exc)[:120]}")

    @staticmethod
    def _save_account(entry: dict[str, Any], result: dict[str, Any], proxy: str) -> str:
        """登录成功后入库：同邮箱已存在则原地换 token，否则新增；随后刷新额度。"""
        email = str(result.get("email") or entry["email"]).strip()
        extra: dict[str, Any] = {
            "email": email,
            "password": entry["password"],
            "totp_secret": entry["totp_secret"],
            "source_type": result.get("source_type", "password"),
            "status": "正常",
        }
        if result.get("account_id"):
            extra["account_id"] = result["account_id"]
        if proxy:
            extra["proxy"] = proxy
        token_data = {
            "access_token": result["access_token"],
            "refresh_token": result.get("refresh_token", ""),
            "id_token": result.get("id_token", ""),
        }
        existing = next(
            (a for a in account_service.list_accounts() if str(a.get("email") or "").strip().lower() == email.lower()),
            None,
        )
        if existing:
            token = account_service._apply_refreshed_tokens(existing["access_token"], token_data, "protocol_login")
            account_service.update_account(token, extra, quiet=True)
        else:
            token = result["access_token"]
            account_service.add_account_items([{**token_data, **extra}])
        account_service.refresh_accounts([token])
        return token


protocol_login_service = ProtocolLoginService()
