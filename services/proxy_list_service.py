"""代理清单：给账号上的代理地址起名字、记录健康状况，导入时把新账号平均分到选中的代理。

账号仍然只认自己的 proxy 字符串，这份清单不参与选出口。健康状况只放内存，重启清零。
"""

from __future__ import annotations

import json
import time
from pathlib import Path
from threading import Lock

from services.config import DATA_DIR

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


class ProxyList:
    def __init__(self, store_file: Path):
        self._store_file = store_file
        self._lock = Lock()
        self._entries: list[dict[str, str]] = self._load()
        self._health: dict[str, dict] = {}

    def _load(self) -> list[dict[str, str]]:
        try:
            raw = json.loads(self._store_file.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return []
        entries: dict[str, dict[str, str]] = {}
        for item in raw if isinstance(raw, list) else []:
            url = str(item.get("url") or "").strip() if isinstance(item, dict) else ""
            if url:
                entries[url] = {"url": url, "name": str(item.get("name") or "").strip()}
        return list(entries.values())

    def _save_locked(self) -> None:
        self._store_file.parent.mkdir(parents=True, exist_ok=True)
        self._store_file.write_text(json.dumps(self._entries, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")

    def _find_locked(self, url: str) -> dict[str, str] | None:
        return next((entry for entry in self._entries if entry["url"] == url), None)

    # ── 清单 ──────────────────────────────────────────────
    def list_entries(self, account_counts: dict[str, int]) -> list[dict]:
        """返回清单；账号上用到但清单里没有的代理自动补进来（名字留空）。"""
        with self._lock:
            missing = [url for url in account_counts if url and self._find_locked(url) is None]
            if missing:
                self._entries.extend({"url": url, "name": ""} for url in missing)
                self._save_locked()
            return [
                {
                    **entry,
                    "masked_url": mask_proxy_url(entry["url"]),
                    "account_count": int(account_counts.get(entry["url"], 0)),
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

    def delete(self, url: str, account_count: int) -> None:
        if account_count > 0:
            raise ValueError(f"还有 {account_count} 个账号在用这个代理，先把它们改到别的代理")
        with self._lock:
            entry = self._find_locked(url.strip())
            if entry is None:
                raise ValueError("代理不存在")
            self._entries.remove(entry)
            self._health.pop(entry["url"], None)
            self._save_locked()

    # ── 健康 ──────────────────────────────────────────────
    def record_success(self, url: str) -> None:
        if not url:
            return
        with self._lock:
            health = self._health.setdefault(url, {})
            health["successes"] = health.get("successes", 0) + 1
            health["consecutive_failures"] = 0
            health["last_success_at"] = time.time()

    def record_failure(self, url: str, error: str) -> None:
        if not url:
            return
        with self._lock:
            health = self._health.setdefault(url, {})
            health["failures"] = health.get("failures", 0) + 1
            health["consecutive_failures"] = health.get("consecutive_failures", 0) + 1
            health["last_error"] = error[:300]
            health["last_failure_at"] = time.time()

    def is_abnormal(self, url: str) -> bool:
        with self._lock:
            return self._health.get(url, {}).get("consecutive_failures", 0) >= FAILURE_THRESHOLD

    def _health_view_locked(self, url: str) -> dict:
        health = self._health.get(url, {})
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
    def distribute(self, candidates: list[str], count: int, account_counts: dict[str, int]) -> list[str]:
        """把 count 个新账号分到候选代理：每次给“已有账号数 + 本批已分配数”最少的，并列按清单顺序。

        异常代理先跳过；候选全部异常时仍在它们之间分配，不退回直连。
        """
        options = [url for url in dict.fromkeys(item.strip() for item in candidates) if url]
        if not options:
            raise ValueError("平均分配至少要选一个代理")
        usable = [url for url in options if not self.is_abnormal(url)] or options
        loads = {url: int(account_counts.get(url, 0)) for url in usable}
        assigned = []
        for _ in range(count):
            pick = min(usable, key=loads.__getitem__)
            loads[pick] += 1
            assigned.append(pick)
        return assigned


proxy_list = ProxyList(DATA_DIR / "proxies.json")
