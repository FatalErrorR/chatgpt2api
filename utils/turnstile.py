"""sentinel dx（turnstile）求解器。

旧版手写 VM 已跟不上上游 SDK 的混淆方式，这里改为在进程内嵌 V8
（py-mini-racer）中加载真实 sentinel SDK 来求解。

- `assets/sentinel/sdk.js`：上游 sentinel SDK（打补丁暴露内部求解函数）
- `assets/sentinel/shims.js`：浏览器环境 shim，可用指纹覆盖其中的环境信号

V8 上下文较重，按指纹缓存复用；上下文不可跨线程并发使用，求解全程持有全局锁。
"""
from __future__ import annotations

import json
import threading
from pathlib import Path
from typing import Any, Dict, Optional

from py_mini_racer import MiniRacer

from utils.log import logger

_ASSETS_DIR = Path(__file__).resolve().parents[1] / "assets" / "sentinel"
_SDK_PATCH_MARKER = "t.token=ye,t}({});"
_SDK_PATCH_REPLACEMENT = "t.token=ye,t.__solve_dx=_n,t.__reg=D,t}({});"
_CTX_CACHE_LIMIT = 4
_EVAL_TIMEOUT_MS = 30000

_cache: Dict[str, MiniRacer] = {}
_lock = threading.Lock()


def _read_asset(name: str) -> str:
    return (_ASSETS_DIR / name).read_text(encoding="utf-8")


def _build_ctx(fingerprint: Dict[str, Any] | None) -> MiniRacer:
    sdk = _read_asset("sdk.js")
    patched = sdk.replace(_SDK_PATCH_MARKER, _SDK_PATCH_REPLACEMENT)
    if patched == sdk:
        raise RuntimeError("sentinel sdk patch marker not found, assets/sentinel/sdk.js may be outdated")
    ctx = MiniRacer()
    ctx.eval(
        "globalThis.setTimeout=globalThis.setTimeout||function(f,m){return 0};"
        "globalThis.clearTimeout=globalThis.clearTimeout||function(){};",
        timeout=_EVAL_TIMEOUT_MS,
    )
    if fingerprint:
        ctx.eval("globalThis.__SHIM_FP = %s;" % json.dumps(fingerprint, ensure_ascii=False), timeout=_EVAL_TIMEOUT_MS)
    ctx.eval(_read_asset("shims.js"), timeout=_EVAL_TIMEOUT_MS)
    ctx.eval(patched, timeout=_EVAL_TIMEOUT_MS)
    return ctx


def _get_ctx(fingerprint: Dict[str, Any] | None) -> MiniRacer:
    key = json.dumps(fingerprint or {}, sort_keys=True, ensure_ascii=False)
    ctx = _cache.get(key)
    if ctx is None:
        ctx = _build_ctx(fingerprint)
        while len(_cache) >= _CTX_CACHE_LIMIT:
            _cache.popitem()
        _cache[key] = ctx
    return ctx


def _solve(ctx: MiniRacer, dx: str, p_token: str) -> str:
    ctx.eval("globalThis.__req = {};", timeout=_EVAL_TIMEOUT_MS)
    ctx.eval("SentinelSDK.__reg(__req, %s);" % json.dumps(p_token), timeout=_EVAL_TIMEOUT_MS)
    ctx.eval("globalThis.__token = undefined; globalThis.__err = undefined;", timeout=_EVAL_TIMEOUT_MS)
    ctx.eval(
        "SentinelSDK.__solve_dx(__req, %s)"
        ".then(function(v){globalThis.__token=v},function(e){globalThis.__err=String(e&&e.stack||e)});"
        % json.dumps(dx),
        timeout=_EVAL_TIMEOUT_MS,
    )
    token = ctx.eval("globalThis.__token", timeout=_EVAL_TIMEOUT_MS)
    error = ctx.eval("globalThis.__err", timeout=_EVAL_TIMEOUT_MS)
    if error:
        raise RuntimeError(f"sentinel solve failed: {error}")
    if not token:
        raise RuntimeError("sentinel solve returned no token")
    return str(token)


def solve_turnstile_token(
    dx: str,
    p: str,
    fingerprint: Optional[Dict[str, Any]] = None,
) -> Optional[str]:
    """求解 sentinel dx 挑战，返回 turnstile token；失败返回 None。

    参数：
    - `dx`：prepare 响应中的 turnstile.dx
    - `p`：本次 prepare 使用的 requirements token（dx 外层程序用它做 XOR 解码）
    - `fingerprint`：浏览器环境信号覆盖（userAgent/languages/oaiDid 等），
      应与 HTTP 请求头保持一致
    """
    if not dx:
        return None
    try:
        with _lock:
            ctx = _get_ctx(fingerprint)
            return _solve(ctx, dx, p)
    except Exception as exc:
        logger.warning({"event": "solve_turnstile_failed", "error": str(exc)})
        return None
