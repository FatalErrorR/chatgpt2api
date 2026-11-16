"""代理运维：检测代理、把账号从失败代理迁走并停用代理、按需重新分配账号的代理。

迁移规则和导入时的平均分配一致：每个账号给当前账号最少的可用代理（并列按清单顺序），每个代理不超过
「单代理最多账号数」。可用代理 = 未停用、未被排除；没指定目标时还要跳过健康异常的代理。
所有操作都先算方案（dry_run 只返回方案），执行时在服务端重新算一遍再落盘，不信任前端传回的方案。
"""

from __future__ import annotations

from collections import Counter
from concurrent.futures import ThreadPoolExecutor

from services.account_service import account_service
from services.config import config
from services.log_service import LOG_TYPE_ACCOUNT, log_service
from services.proxy_list_service import CapacityError, pick_least_loaded, proxy_key, proxy_list
from services.proxy_service import test_proxy

CHECK_WORKERS = 16
# 每条日志最多记多少条迁移明细
LOG_MOVES_LIMIT = 300


class ReassignError(ValueError):
    pass


def _label(entry: dict) -> str:
    """代理的显示名：有名字用名字，否则用 主机:端口（不带账号密码）。"""
    if str(entry.get("name") or "").strip():
        return str(entry["name"]).strip()
    masked = str(entry.get("masked_url") or "")
    rest = masked.split("://", 1)[-1]
    return rest.rsplit("@", 1)[-1].rstrip("/") or masked


def _snapshot() -> tuple[list[dict], list[dict]]:
    entries = proxy_list.list_entries(account_service.proxy_account_counts())
    return entries, account_service.list_accounts()


def _plan(move_accounts: list[dict], entries: list[dict], target_keys: set[str] | None, exclude_keys: set[str]) -> dict:
    """算出 move_accounts 各自去哪个代理。target_keys 为 None 表示「所有可用代理」（跳过异常的）。"""
    by_key = {entry["key"]: entry for entry in entries}
    candidates = [
        entry
        for entry in entries
        if not entry["disabled"] and entry["key"] not in exclude_keys and (target_keys is None or entry["key"] in target_keys)
    ]
    if target_keys is None:
        candidates = [entry for entry in candidates if not entry["health"]["abnormal"]]
    if move_accounts and not candidates:
        raise ReassignError("没有可用的代理接收这些账号（代理都已停用、异常或被排除），先添加或启用代理")

    leaving = Counter(account.get("proxy_key") for account in move_accounts if account.get("proxy_key"))
    reserved = proxy_list.reserved_counts()
    loads = {
        entry["url"]: entry["account_count"] - leaving.get(entry["key"], 0) + reserved.get(entry["key"], 0)
        for entry in candidates
    }
    max_per = config.proxy_max_accounts
    picks = pick_least_loaded([entry["url"] for entry in candidates], loads, len(move_accounts), max_per)

    key_by_url = {entry["url"]: entry["key"] for entry in candidates}
    moves: list[dict] = []
    unchanged = 0
    received: Counter[str] = Counter()
    for account, url in zip(move_accounts, picks):
        target_key = key_by_url[url]
        if account.get("proxy_key") == target_key:
            unchanged += 1
            continue
        received[target_key] += 1
        source = by_key.get(account.get("proxy_key") or "")
        moves.append({
            "token": account["access_token"],
            "email": account.get("email") or "",
            "from": _label(source) if source else ("未指定" if not account.get("proxy") else "清单外代理"),
            "to": _label(by_key[target_key]),
            "to_url": url,
        })
    targets = [
        {
            "key": key,
            "label": _label(by_key[key]),
            "before": by_key[key]["account_count"],
            "after": by_key[key]["account_count"] - leaving.get(key, 0) + received[key],
        }
        for key in received
    ]
    targets.sort(key=lambda item: item["label"])
    return {"moves": moves, "unchanged": unchanged, "targets": targets, "max_per_proxy": max_per}


def _public(plan: dict) -> dict:
    """给前端的方案：不带 token 和代理原始地址。"""
    return {**plan, "moves": [{key: move[key] for key in ("email", "from", "to")} for move in plan["moves"]]}


def _apply(plan: dict, reason: str) -> int:
    changed = account_service.set_account_proxies({move["token"]: move["to_url"] for move in plan["moves"]})
    if changed:
        log_service.add(
            LOG_TYPE_ACCOUNT,
            f"{reason}：迁移 {len(changed)} 个账号的代理",
            {
                "reason": reason,
                "moved": len(changed),
                "moves": [{key: move[key] for key in ("email", "from", "to")} for move in plan["moves"][:LOG_MOVES_LIMIT]],
            },
        )
    return len(changed)


def _plan_or_shortfall(plan_fn) -> dict:
    """dry_run 时把名额不足、无可用代理也当成方案结果返回，前端好提示。"""
    try:
        return _public(plan_fn())
    except CapacityError as exc:
        return {"moves": [], "unchanged": 0, "targets": [], "max_per_proxy": config.proxy_max_accounts, "error": str(exc), "shortfall": exc.shortfall}
    except ReassignError as exc:
        return {"moves": [], "unchanged": 0, "targets": [], "max_per_proxy": config.proxy_max_accounts, "error": str(exc)}


def retire_proxies(urls: list[str], dry_run: bool = True) -> dict:
    """把这些代理上的账号迁到其它可用代理，然后停用这些代理。"""
    keys = {proxy_key(url) for url in urls if str(url).strip()}
    if not keys:
        raise ReassignError("请选择要停用的代理")
    entries, accounts = _snapshot()
    known = {entry["key"]: entry for entry in entries}
    missing = keys - set(known)
    if missing:
        raise ReassignError(f"有 {len(missing)} 个代理不在清单里，刷新后再试")
    on_retiring = [account for account in accounts if account.get("proxy_key") in keys]

    def plan() -> dict:
        return _plan(on_retiring, entries, None, keys)

    retiring = [{"key": key, "label": _label(known[key]), "account_count": known[key]["account_count"]} for key in keys]
    retiring.sort(key=lambda item: item["label"])
    if dry_run:
        return {**_plan_or_shortfall(plan), "retiring": retiring}
    result = plan()  # 名额不足等问题直接抛给调用方，不做部分迁移
    moved = _apply(result, f"停用 {len(keys)} 个代理")
    disabled = proxy_list.set_disabled([known[key]["url"] for key in keys], True)
    log_service.add(LOG_TYPE_ACCOUNT, f"停用 {disabled} 个代理", {"proxies": [item["label"] for item in retiring]})
    return {**_public(result), "retiring": retiring, "moved": moved, "disabled": disabled}


def enable_proxies(urls: list[str]) -> int:
    changed = proxy_list.set_disabled(urls, False)
    if changed:
        entries = {entry["key"]: entry for entry in proxy_list.list_entries(account_service.proxy_account_counts())}
        labels = [_label(entries[proxy_key(url)]) for url in urls if proxy_key(url) in entries]
        log_service.add(LOG_TYPE_ACCOUNT, f"启用 {changed} 个代理", {"proxies": labels})
    return changed


def reassign_accounts(tokens: list[str], target_urls: list[str], dry_run: bool = True) -> dict:
    """把选中账号重新分到 target_urls（为空表示所有可用代理）。"""
    wanted = {token for token in tokens if token}
    if not wanted:
        raise ReassignError("请选择要重新分配代理的账号")
    entries, accounts = _snapshot()
    move_accounts = [account for account in accounts if account["access_token"] in wanted]
    if not move_accounts:
        raise ReassignError("选中的账号已不存在，刷新后再试")
    target_keys = {proxy_key(url) for url in target_urls if str(url).strip()} or None

    def plan() -> dict:
        return _plan(move_accounts, entries, target_keys, set())

    if dry_run:
        return _plan_or_shortfall(plan)
    result = plan()
    return {**_public(result), "moved": _apply(result, "重新分配代理")}


def is_provider_outage(tested: int, failures: list[dict]) -> bool:
    """大面积失败多半是代理商的问题（欠费 402、整体故障），这时停用代理也没有好代理可以接手。"""
    if not failures or tested < 2:
        return False
    if len(failures) * 2 > tested:
        return True
    return len(failures) >= 3 and all("402" in str(item.get("error") or "") for item in failures)


def check_proxies() -> dict:
    """对所有启用的代理测一轮（连 chatgpt.com，只有 2xx/3xx 算通过），结果记进健康；
    失败的代理附上「停用并迁移账号」的预演方案。大面积失败时判定为代理商故障，不给方案。"""
    entries, _ = _snapshot()
    enabled = [entry for entry in entries if not entry["disabled"]]
    with ThreadPoolExecutor(max_workers=CHECK_WORKERS) as pool:
        results = list(pool.map(lambda entry: test_proxy(entry["url"]), enabled))
    failures: list[dict] = []
    for entry, result in zip(enabled, results):
        status = int(result.get("status") or 0)
        if 200 <= status < 400:
            proxy_list.record_success(entry["url"])
            continue
        error = str(result.get("error") or f"HTTP {status}")
        proxy_list.record_failure(entry["url"], error)
        failures.append({
            "key": entry["key"],
            "url": entry["url"],
            "label": _label(entry),
            "status": status,
            "error": error,
            "account_count": entry["account_count"],
        })
    outage = is_provider_outage(len(enabled), failures)
    plan = None
    if failures and not outage:
        plan = retire_proxies([item["url"] for item in failures], dry_run=True)
    return {
        "tested": len(enabled),
        "passed": len(enabled) - len(failures),
        "failures": [{key: value for key, value in item.items() if key != "url"} for item in failures],
        "outage": outage,
        "plan": plan,
        "results": {entry["key"]: result for entry, result in zip(enabled, results)},
    }
