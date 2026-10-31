from __future__ import annotations

import gzip
import mimetypes
from contextlib import asynccontextmanager
from pathlib import Path
from threading import Event, Thread

from fastapi import FastAPI, HTTPException, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.middleware.gzip import GZipMiddleware
from fastapi.responses import FileResponse, Response

from api import accounts, ai, image_tasks, system
from api.errors import install_exception_handlers
from api.support import resolve_web_asset, start_limited_account_watcher
from services.backup_service import backup_service
from services.config import config
from services.image_service import start_image_cleanup_scheduler
from services.log_service import log_service


def create_app() -> FastAPI:
    app_version = config.app_version

    @asynccontextmanager
    async def lifespan(_: FastAPI):
        stop_event = Event()
        thread = start_limited_account_watcher(stop_event)
        cleanup_thread = start_image_cleanup_scheduler(stop_event)
        backup_service.start()
        config.cleanup_old_images()
        Thread(target=log_service.image_output_counts, name="image-count-rollup", daemon=True).start()
        try:
            yield
        finally:
            stop_event.set()
            thread.join(timeout=1)
            cleanup_thread.join(timeout=1)
            backup_service.stop()

    app = FastAPI(title="chatgpt2api", version=app_version, lifespan=lifespan)
    install_exception_handlers(app)
    app.add_middleware(
        CORSMiddleware,
        allow_origins=["*"],
        allow_credentials=False,
        allow_methods=["*"],
        allow_headers=["*"],
    )
    # text/event-stream is excluded by Starlette. Static files use pathsend and
    # skip this middleware, so serve_web compresses those itself.
    app.add_middleware(GZipMiddleware, minimum_size=800, compresslevel=6)
    app.include_router(ai.create_router())
    app.include_router(accounts.create_router())
    app.include_router(image_tasks.create_router())
    app.include_router(system.create_router(app_version))

    @app.api_route("/{full_path:path}", methods=["GET", "HEAD"], include_in_schema=False)
    async def serve_web(full_path: str, request: Request):
        asset = resolve_web_asset(full_path)
        if asset is not None:
            return _asset_response(asset, request)
        if full_path.strip("/").startswith("_next/"):
            raise HTTPException(status_code=404, detail="Not Found")
        fallback = resolve_web_asset("")
        if fallback is None:
            raise HTTPException(status_code=404, detail="Not Found")
        return _asset_response(fallback, request)

    return app


_GZIP_CACHE: dict[tuple[str, int], bytes] = {}


def _cached_gzip(asset: Path) -> bytes:
    key = (str(asset), asset.stat().st_mtime_ns)
    cached = _GZIP_CACHE.get(key)
    if cached is None:
        cached = gzip.compress(asset.read_bytes(), compresslevel=6)
        if len(_GZIP_CACHE) > 80:
            _GZIP_CACHE.clear()
        _GZIP_CACHE[key] = cached
    return cached


def _asset_response(asset: Path, request: Request) -> Response:
    headers: dict[str, str] = {}
    if "_next/static/" in asset.as_posix():
        headers["Cache-Control"] = "public, max-age=31536000, immutable"
    if (
        request.method == "GET"
        and "gzip" in request.headers.get("accept-encoding", "")
        and asset.suffix in {".js", ".css"}
    ):
        body = _cached_gzip(asset)
        headers["Content-Encoding"] = "gzip"
        headers["Vary"] = "Accept-Encoding"
        media_type = mimetypes.guess_type(asset.name)[0] or "application/octet-stream"
        return Response(body, media_type=media_type, headers=headers)
    return FileResponse(asset, headers=headers)
