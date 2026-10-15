"""RFC 6238 TOTP（标准库实现，不引入额外依赖）。

用于批量协议登录时自动填写二步验证码。密钥是 Authenticator 里的 base32 字符串。
"""
from __future__ import annotations

import base64
import binascii
import hashlib
import hmac
import struct
import time

TOTP_PERIOD = 30
TOTP_DIGITS = 6


def normalize_totp_secret(secret: str) -> str:
    """去掉空格/连字符并转大写；非法 base32 返回空串。"""
    cleaned = "".join(str(secret or "").split()).replace("-", "").upper()
    if not cleaned:
        return ""
    try:
        base64.b32decode(cleaned + "=" * (-len(cleaned) % 8), casefold=True)
    except (binascii.Error, ValueError):
        return ""
    return cleaned


def totp_code(secret: str, now: float | None = None, *, period: int = TOTP_PERIOD, digits: int = TOTP_DIGITS) -> str:
    """生成当前时间窗口的验证码。secret 非法时抛出 ValueError。"""
    cleaned = normalize_totp_secret(secret)
    if not cleaned:
        raise ValueError("invalid totp secret")
    key = base64.b32decode(cleaned + "=" * (-len(cleaned) % 8), casefold=True)
    counter = int((time.time() if now is None else now) // period)
    digest = hmac.new(key, struct.pack(">Q", counter), hashlib.sha1).digest()
    offset = digest[-1] & 0x0F
    value = (struct.unpack(">I", digest[offset:offset + 4])[0] & 0x7FFFFFFF) % (10 ** digits)
    return str(value).zfill(digits)


def seconds_left_in_period(now: float | None = None, *, period: int = TOTP_PERIOD) -> float:
    """当前验证码窗口还剩多少秒，用于避免在窗口边界提交过期验证码。"""
    current = time.time() if now is None else now
    return period - (current % period)
