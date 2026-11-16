from __future__ import annotations

from concurrent.futures import ThreadPoolExecutor

from fastapi import APIRouter, Header, HTTPException
from fastapi.concurrency import run_in_threadpool
from pydantic import BaseModel, Field

from api.support import require_admin
from services.account_service import account_service
from services.proxy_list_service import proxy_list
from services.proxy_reassign_service import ReassignError, check_proxies, enable_proxies, retire_proxies
from services.proxy_service import test_proxy


class ProxyEntryRequest(BaseModel):
    url: str = ""
    name: str = ""


class ProxyBatchAddRequest(BaseModel):
    # 每行一个代理：host:port:账号:密码、host:port 或完整地址
    text: str = ""
    # 填了就依次命名「厂商-N」，接着这个厂商已有的最大序号
    vendor: str = ""


class ProxyRetireRequest(BaseModel):
    # 要停用的代理（地址或 key）；上面的账号先迁到其它可用代理
    urls: list[str] = Field(default_factory=list)
    # True 只返回迁移方案，不改任何东西
    dry_run: bool = True


class ProxyEnableRequest(BaseModel):
    urls: list[str] = Field(default_factory=list)


class ProxyBatchTestRequest(BaseModel):
    # 为空时测试清单里的全部代理
    urls: list[str] = Field(default_factory=list)


def _list() -> dict:
    return {"items": proxy_list.list_entries(account_service.proxy_account_counts())}


def _test_proxies(urls: list[str]) -> dict:
    """并发测试代理连 chatgpt.com，结果记进代理健康：只有 2xx/3xx 算正常，被挑战（403）或连不上算失败。"""
    entries = proxy_list.list_entries(account_service.proxy_account_counts())
    wanted = {url.strip() for url in urls if url.strip()}
    targets = [entry for entry in entries if not wanted or entry["url"] in wanted]
    with ThreadPoolExecutor(max_workers=8) as pool:
        results = list(pool.map(lambda entry: test_proxy(entry["url"]), targets))
    for entry, result in zip(targets, results):
        if 200 <= int(result.get("status") or 0) < 400:
            proxy_list.record_success(entry["url"])
        else:
            proxy_list.record_failure(entry["url"], str(result.get("error") or f"HTTP {result.get('status')}"))
    return {"results": {entry["key"]: result for entry, result in zip(targets, results)}, **_list()}


def create_router() -> APIRouter:
    router = APIRouter()

    @router.get("/api/proxies")
    async def list_proxies(authorization: str | None = Header(default=None)):
        require_admin(authorization)
        return await run_in_threadpool(_list)

    @router.post("/api/proxies")
    async def add_proxy(body: ProxyEntryRequest, authorization: str | None = Header(default=None)):
        require_admin(authorization)
        try:
            proxy_list.add(body.url, body.name)
        except ValueError as exc:
            raise HTTPException(status_code=400, detail={"error": str(exc)}) from exc
        return await run_in_threadpool(_list)

    @router.post("/api/proxies/batch")
    async def add_proxies(body: ProxyBatchAddRequest, authorization: str | None = Header(default=None)):
        require_admin(authorization)
        try:
            result = proxy_list.add_batch(body.text, body.vendor)
        except ValueError as exc:
            raise HTTPException(status_code=400, detail={"error": str(exc)}) from exc
        return {**result, **await run_in_threadpool(_list)}

    @router.post("/api/proxies/test")
    async def test_proxies(body: ProxyBatchTestRequest, authorization: str | None = Header(default=None)):
        require_admin(authorization)
        return await run_in_threadpool(_test_proxies, body.urls)

    @router.post("/api/proxies/check")
    async def check_all_proxies(authorization: str | None = Header(default=None)):
        """检测所有启用的代理一轮，失败的附上停用并迁移账号的方案（不执行）。"""
        require_admin(authorization)
        result = await run_in_threadpool(check_proxies)
        return {**result, **await run_in_threadpool(_list)}

    @router.post("/api/proxies/retire")
    async def retire(body: ProxyRetireRequest, authorization: str | None = Header(default=None)):
        """把代理上的账号迁走并停用代理；dry_run 只返回方案。"""
        require_admin(authorization)
        try:
            result = await run_in_threadpool(retire_proxies, body.urls, body.dry_run)
        except ValueError as exc:
            raise HTTPException(status_code=400, detail={"error": str(exc)}) from exc
        return {**result, **await run_in_threadpool(_list)}

    @router.post("/api/proxies/enable")
    async def enable(body: ProxyEnableRequest, authorization: str | None = Header(default=None)):
        require_admin(authorization)
        changed = await run_in_threadpool(enable_proxies, body.urls)
        return {"enabled": changed, **await run_in_threadpool(_list)}

    @router.post("/api/proxies/update")
    async def rename_proxy(body: ProxyEntryRequest, authorization: str | None = Header(default=None)):
        require_admin(authorization)
        try:
            proxy_list.rename(body.url, body.name)
        except ValueError as exc:
            raise HTTPException(status_code=400, detail={"error": str(exc)}) from exc
        return await run_in_threadpool(_list)

    @router.post("/api/proxies/delete")
    async def delete_proxy(body: ProxyEntryRequest, authorization: str | None = Header(default=None)):
        require_admin(authorization)
        try:
            proxy_list.delete(body.url, account_service.proxy_account_counts())
        except ValueError as exc:
            raise HTTPException(status_code=400, detail={"error": str(exc)}) from exc
        return await run_in_threadpool(_list)

    return router
