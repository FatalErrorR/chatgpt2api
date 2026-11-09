"""代理清单：给账号上的代理地址起名字、记录健康状况，导入时把新账号平均分到选中的代理。

账号仍然只认自己的 proxy 字符串，这份清单不参与选出口。健康状况只放内存，重启清零。
"""

from __future__ import annotations

import json
import time
from collections.abc import Callable
from pathlib import Path
from threading import Lock

from services.config import DATA_DIR
from services.proxy_service import normalize_proxy_url

# 连续这么多次请求失败（重试后仍是 Cloudflare 挑战页，或连接出错）就标记异常；成功一次即恢复
FAILURE_THRESHOLD = 3


def mask_proxy_url(url: str) -> str:
    """隐藏代理密码：scheme://user:pass@host:port -> scheme://user:***@host:port。"""
    if "@" not in url:
        return url
    credentials, host = url.rsplit("@", 1)
    scheme, sep, userinfo = credentials.rpartition("://")
    user = userinfo.split(":", 1)[0]
    return f"{scheme}{sep}{user}:***@{host}"


def proxy_key(url: str) -> str:
    """同一代理不同写法的统一识别值：与实际请求一样归一化（socks5 -> socks5h、host:port 补 http），
    scheme 和主机名不分大小写，去掉末尾斜杠。只用于比对，不写回账号。"""
    url = normalize_proxy_url(url).rstrip("/")
    scheme, sep, rest = url.partition("://")
    if not sep:
        return url
    userinfo, at, host = rest.rpartition("@")
    return f"{scheme.lower()}://{userinfo}{at}{host.lower()}"


def _counts_by_key(account_counts: dict[str, int]) -> dict[str, int]:
    counts: dict[str, int] = {}
    for url, count in account_counts.items():
        if url:
            counts[proxy_key(url)] = counts.get(proxy_key(url), 0) + int(count)
    return counts


class ProxyList:
    def __init__(self, store_file: Path):
        self._store_file = store_file
        self._lock = Lock()
        self._entries: list[dict[str, str]] = self._load()
        self._health: dict[str, dict] = {}  # 按 proxy_key
        self._reserved: dict[str, int] = {}  # 已分配、账号还没入库（协议登录进行中）的数量，按 proxy_key

    def _load(self) -> list[dict[str, str]]:
        try:
            raw = json.loads(self._store_file.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return []
        entries: dict[str, dict[str, str]] = {}
        for item in raw if isinstance(raw, list) else []:
            url = str(item.get("url") or "").strip() if isinstance(item, dict) else ""
            if url:
                entries.setdefault(proxy_key(url), {"url": url, "name": str(item.get("name") or "").strip()})
        return list(entries.values())

    def _save_locked(self) -> None:
        self._store_file.parent.mkdir(parents=True, exist_ok=True)
        self._store_file.write_text(json.dumps(self._entries, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")

    def _find_locked(self, url: str) -> dict[str, str] | None:
        key = proxy_key(url)
        return next((entry for entry in self._entries if proxy_key(entry["url"]) == key), None)

    # ── 清单 ──────────────────────────────────────────────
    def list_entries(self, account_counts: dict[str, int]) -> list[dict]:
        """返回清单；账号上用到但清单里没有的代理自动补进来（名字留空）。"""
        counts = _counts_by_key(account_counts)
        with self._lock:
            missing = [url for url in account_counts if url and self._find_locked(url) is None]
            for url in missing:
                if self._find_locked(url) is None:  # 同一代理的多种写法只补一条
                    self._entries.append({"url": url, "name": ""})
            if missing:
                self._save_locked()
            return [
                {
                    **entry,
                    "key": proxy_key(entry["url"]),
                    "masked_url": mask_proxy_url(entry["url"]),
                    "account_count": counts.get(proxy_key(entry["url"]), 0),
                    "health": self._health_view_locked(entry["url"]),
                }
                for entry in self._entries
            ]

    def add(self, url: str, name: str = "") -> None:
        url = url.strip()
        if not url:
            raise ValueError("请填写代理地址")
        with self._lock:
            if self._find_locked(url) is not None:
                raise ValueError("这个代理已经在清单里")
            self._entries.append({"url": url, "name": name.strip()})
            self._save_locked()

    def rename(self, url: str, name: str) -> None:
        with self._lock:
            entry = self._find_locked(url.strip())
            if entry is None:
                raise ValueError("代理不存在")
            entry["name"] = name.strip()
            self._save_locked()

    def delete(self, url: str, account_counts: dict[str, int]) -> None:
        account_count = _counts_by_key(account_counts).get(proxy_key(url), 0)
        if account_count > 0:
            raise ValueError(f"还有 {account_count} 个账号在用这个代理，先把它们改到别的代理")
        with self._lock:
            entry = self._find_locked(url.strip())
            if entry is None:
                raise ValueError("代理不存在")
            self._entries.remove(entry)
            self._health.pop(proxy_key(entry["url"]), None)
            self._save_locked()

    # ── 健康 ──────────────────────────────────────────────
    def record_success(self, url: str) -> None:
        if not url:
            return
        with self._lock:
            health = self._health.setdefault(proxy_key(url), {})
            health["successes"] = health.get("successes", 0) + 1
            health["consecutive_failures"] = 0
            health["last_success_at"] = time.time()

    def record_failure(self, url: str, error: str) -> None:
        if not url:
            return
        with self._lock:
            health = self._health.setdefault(proxy_key(url), {})
            health["failures"] = health.get("failures", 0) + 1
            health["consecutive_failures"] = health.get("consecutive_failures", 0) + 1
            health["last_error"] = error[:300]
            health["last_failure_at"] = time.time()

    def is_abnormal(self, url: str) -> bool:
        with self._lock:
            return self._is_abnormal_locked(url)

    def _is_abnormal_locked(self, url: str) -> bool:
        return self._health.get(proxy_key(url), {}).get("consecutive_failures", 0) >= FAILURE_THRESHOLD

    def _health_view_locked(self, url: str) -> dict:
        health = self._health.get(proxy_key(url), {})
        return {
            "abnormal": health.get("consecutive_failures", 0) >= FAILURE_THRESHOLD,
            "successes": health.get("successes", 0),
            "failures": health.get("failures", 0),
            "consecutive_failures": health.get("consecutive_failures", 0),
            "last_error": health.get("last_error", ""),
            "last_failure_at": health.get("last_failure_at"),
            "last_success_at": health.get("last_success_at"),
        }

    # ── 平均分配 ──────────────────────────────────────────
    def reserve(self, candidates: list[str], count: int, count_accounts: Callable[[], dict[str, int]]) -> list[str]:
        """把 count 个新账号分到候选代理并预留名额：每次给“已入库账号 + 其它批次预留 + 本批已分配”最少的，
        并列按候选顺序。异常代理先跳过；候选全部异常时仍在它们之间分配，不退回直连。

        返回候选里的原始写法。账号入库（或登录失败）后必须对每个结果调用 release，否则名额一直占着。
        count_accounts 在锁内调用：入库后再 release，最坏只是短暂多算，不会少算。
        """
        unique: dict[str, str] = {}
        for url in candidates:
            if url.strip():
                unique.setdefault(proxy_key(url), url.strip())
        options = list(unique.values())
        if not options:
            raise ValueError("平均分配至少要选一个代理")
        with self._lock:
            usable = [url for url in options if not self._is_abnormal_locked(url)] or options
            counts = _counts_by_key(count_accounts())
            loads = {url: counts.get(proxy_key(url), 0) + self._reserved.get(proxy_key(url), 0) for url in usable}
            assigned = []
            for _ in range(count):
                pick = min(usable, key=loads.__getitem__)
                loads[pick] += 1
                assigned.append(pick)
                self._reserved[proxy_key(pick)] = self._reserved.get(proxy_key(pick), 0) + 1
            return assigned

    def release(self, urls: list[str]) -> None:
        with self._lock:
            for url in urls:
                key = proxy_key(url)
                left = self._reserved.get(key, 0) - 1
                if left > 0:
                    self._reserved[key] = left
                else:
                    self._reserved.pop(key, None)

proxy_list = ProxyList(DATA_DIR / "proxies.json")
