from __future__ import annotations

from fastapi import APIRouter, Header, HTTPException
from fastapi.concurrency import run_in_threadpool
from pydantic import BaseModel

from api.support import require_admin
from services.account_service import account_service
from services.proxy_list_service import proxy_list


class ProxyEntryRequest(BaseModel):
    url: str = ""
    name: str = ""


def _list() -> dict:
    return {"items": proxy_list.list_entries(account_service.proxy_account_counts())}


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
