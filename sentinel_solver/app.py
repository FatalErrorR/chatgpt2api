"""Sentinel solver service: keeps a headless Chromium on chatgpt.com and computes Turnstile tokens.

Since 2026-10-14 the chatgpt.com conversation endpoint only accepts a Turnstile
token that was produced inside a real chatgpt.com page; a pure Python/Node
emulation is rejected. The main service does not bundle a browser, so it calls
this one over HTTP.

`vm.js` is a port of the two isomorphic VMs in the chatgpt.com frontend bundle
(the challenge program is served as an obfuscated instruction list and has to run
against the real `window`/`document`). It follows the implementation in
MEIZU16/chatgpt2api's `sentinel_solver/` (MIT), kept byte-identical so it can be
re-synced against the frontend bundle when OpenAI changes the instruction set.

Environment:
- SENTINEL_SOLVER_KEY: when set, requests must send `Authorization: Bearer <key>`
- SENTINEL_SOLVER_PROXY: browser proxy for requests that do not name one. The main
  service sends the proxy it uses itself with every request, so both share one egress IP.
- SENTINEL_SOLVER_USER_AGENT: optional, defaults to a Linux Chrome UA built from the bundled Chromium
- SENTINEL_SOLVER_TIMEZONE: optional page timezone (e.g. Asia/Shanghai), should match the egress IP
- SENTINEL_SOLVER_REFRESH_SECONDS: how often the page is reopened, default 1800
- SENTINEL_SOLVER_HEADLESS: default true, set false to debug
- PORT: listen port, default 8080
"""
from __future__ import annotations

import asyncio
import hmac
import os
import time
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any
from urllib.parse import unquote, urlsplit

import uvicorn
from fastapi import FastAPI, Header, HTTPException
from playwright.async_api import Browser, BrowserContext, Page, Playwright, async_playwright
from playwright_stealth import Stealth
from pydantic import BaseModel

CHATGPT_URL = "https://chatgpt.com/"
CHALLENGE_TITLES = ("请稍候", "Just a moment", "Attention Required")
VM_SCRIPT = (Path(__file__).parent / "vm.js").read_text(encoding="utf-8")
BASE_INIT_SCRIPT = """
Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
window.chrome = window.chrome || { runtime: {} };
"""

# Preferred path: load OpenAI's own turnstile chunk from this very page and let it
# compute the token against the real environment. A hand-ported VM has to track
# whatever the frontend does next; calling the shipped module does not.
#
# The chunk is content-hashed and changes on every deploy, so it is discovered at
# solve time instead of being pinned. See `_DISCOVER_TURNSTILE_ASSET`.
_PINNED_ASSET = "/unauth-mweb/assets/turnstile-DykPLDEM.js"
_DISCOVER_TURNSTILE_ASSET = """
() => {
  const pattern = /\\/turnstile-[^/"']+\\.js/;
  const seen = [];
  const push = (url) => { if (url && pattern.test(url) && !seen.includes(url)) seen.push(url); };
  try { performance.getEntriesByType('resource').forEach((e) => push(e.name)); } catch (err) {}
  try { Array.from(document.scripts).forEach((s) => push(s.src)); } catch (err) {}
  return seen;
}
"""
_SCAN_ASSET_IN_SCRIPTS = """
async (pattern) => {
  const re = new RegExp(pattern);
  for (const script of Array.from(document.scripts)) {
    if (!script.src) continue;
    try {
      const text = await (await fetch(script.src)).text();
      const hit = text.match(re);
      if (hit) return hit[0];
    } catch (err) {}
  }
  return null;
}
"""
_SOLVE_VIA_SDK = """
async ([requirements, key, asset]) => {
  try {
    const mod = await import(asset);
    const turnstile = mod.turnstile || (mod.default && mod.default.turnstile);
    if (!turnstile || typeof turnstile.getEnforcementToken !== 'function') {
      return 'ERR:getEnforcementToken missing';
    }
    // The SDK looks its seed up in a WeakMap keyed by the requirements object;
    // hand it the p token instead. Its own tokenCache is a WeakMap too: hooking
    // that one makes it return p as the token without running the VM.
    const cache = turnstile.tokenCache;
    const original = WeakMap.prototype.get;
    WeakMap.prototype.get = function (target) {
      if (this === cache) return original.call(this, target);
      return target === requirements ? key : original.call(this, target);
    };
    let token = '';
    try {
      token = String(await turnstile.getEnforcementToken(requirements) || '');
    } finally {
      WeakMap.prototype.get = original;
    }
    if (!token) return 'ERR:turnstile returned an empty token';
    if (token === key) return 'ERR:turnstile VM did not run (p echoed back)';
    // Over its 500ms budget the VM returns only the step count.
    if (/^\\d+$/.test(token)) return 'ERR:turnstile VM timed out after ' + token + ' steps';
    // A VM exception comes back as btoa("<steps>: <error>").
    let decoded = '';
    try { decoded = atob(token); } catch (error) {}
    if (/^\\d+: /.test(decoded)) return 'ERR:turnstile VM error: ' + decoded.slice(0, 160);
    return token;
  } catch (error) {
    return 'ERR:' + String(error).slice(0, 300);
  }
}
"""
_SOLVE_VIA_VM = "([dx, p]) => window.__sentinelVM.turnstile(dx, p)"

SOLVER_KEY = os.getenv("SENTINEL_SOLVER_KEY", "").strip()
PROXY = os.getenv("SENTINEL_SOLVER_PROXY", "").strip()
USER_AGENT = os.getenv("SENTINEL_SOLVER_USER_AGENT", "").strip()
TIMEZONE = os.getenv("SENTINEL_SOLVER_TIMEZONE", "").strip()
REFRESH_SECONDS = max(60, int(os.getenv("SENTINEL_SOLVER_REFRESH_SECONDS", "1800")))
HEADLESS = os.getenv("SENTINEL_SOLVER_HEADLESS", "true").strip().lower() not in {"0", "false", "no"}
# Turnstile runs on a 500ms budget in the frontend and reports the step count on
# timeout, so a slow solve is not an error there. SO gets a real deadline.
# The ported VM inside vm.js keeps the frontend's own 500ms turnstile budget and
# 60s SO budget. The page's own module may need to fetch/parse on first use, so it
# gets room here.
SDK_SOLVE_TIMEOUT_MS = 120000
VM_TIMEOUT_ATTEMPTS = 3
READY_TIMEOUT_SECONDS = 60
# One page per egress proxy the main service uses (e.g. per-account proxies).
MAX_PROXY_PAGES = 4


def playwright_proxy(url: str) -> dict[str, str] | None:
    """Chromium ignores user:pass inside the proxy URL; Playwright takes them separately."""
    if not url:
        return None
    parts = urlsplit(url if "://" in url else f"http://{url}")
    server = f"{parts.scheme}://{parts.hostname}" + (f":{parts.port}" if parts.port else "")
    proxy = {"server": server}
    if parts.username:
        proxy["username"] = unquote(parts.username)
    if parts.password:
        proxy["password"] = unquote(parts.password)
    return proxy


def redact_proxy(url: str) -> str:
    if not url:
        return "direct"
    parts = urlsplit(url if "://" in url else f"http://{url}")
    return f"{parts.scheme}://{parts.hostname}" + (f":{parts.port}" if parts.port else "")


class SolverPage:
    """Keeps one Cloudflare-cleared chatgpt.com page alive behind one egress proxy."""

    def __init__(self, browser: Browser, user_agent: str, proxy: str = "") -> None:
        self._browser = browser
        self._context: BrowserContext | None = None
        self._page: Page | None = None
        self._opened_at = 0.0
        self._lock = asyncio.Lock()
        self._turnstile_asset = ""
        self.user_agent = user_agent
        self.proxy = proxy
        self.last_error = ""
        self._closed = False

    async def stop(self) -> None:
        # An evicted page must not reopen a context nobody will close.
        self._closed = True
        if self._context is not None:
            try:
                await self._context.close()
            except Exception:  # noqa: BLE001
                pass

    async def page(self) -> Page:
        async with self._lock:
            if self._closed:
                raise RuntimeError(f"solver page for {redact_proxy(self.proxy)} was evicted")
            stale = self._page is None or time.monotonic() - self._opened_at > REFRESH_SECONDS
            if not stale and not await self._is_ready(self._page):
                stale = True
            if stale:
                await self._reopen()
            return self._page

    async def invalidate(self) -> None:
        async with self._lock:
            self._opened_at = 0.0

    async def _reopen(self) -> None:
        if self._context is not None:
            try:
                await self._context.close()
            except Exception:  # noqa: BLE001
                pass
        self._page = None
        context_kwargs: dict[str, object] = {
            "locale": "zh-CN",
            "viewport": {"width": 1280, "height": 800},
            "user_agent": self.user_agent,
        }
        if TIMEZONE:
            context_kwargs["timezone_id"] = TIMEZONE
        if self.proxy:
            context_kwargs["proxy"] = playwright_proxy(self.proxy)
        context = await self._browser.new_context(**context_kwargs)
        # page.evaluate has no per-call timeout, so widen the context default; the
        # page's own module can take a moment on first use.
        context.set_default_timeout(SDK_SOLVE_TIMEOUT_MS)
        await context.add_init_script(BASE_INIT_SCRIPT)
        await context.add_init_script(VM_SCRIPT)
        await Stealth().apply_stealth_async(context)
        page = await context.new_page()
        await page.goto(CHATGPT_URL, wait_until="domcontentloaded", timeout=60000)
        deadline = time.monotonic() + READY_TIMEOUT_SECONDS
        while not await self._is_ready(page):
            if time.monotonic() > deadline:
                title = await page.title()
                await context.close()
                raise RuntimeError(
                    f"chatgpt.com page not ready (title={title!r}), likely stuck on a Cloudflare challenge"
                )
            await asyncio.sleep(1)
        self._context = context
        self._page = page
        self._opened_at = time.monotonic()
        self.last_error = ""
        print(f"[sentinel-solver] chatgpt.com page ready via {redact_proxy(self.proxy)}: {await page.title()!r}", flush=True)

    @staticmethod
    async def _is_ready(page: Page | None) -> bool:
        if page is None or page.is_closed():
            return False
        try:
            title = await page.title()
            if any(marker in title for marker in CHALLENGE_TITLES):
                return False
            return await page.evaluate(
                "location.hostname === 'chatgpt.com' && typeof window.__sentinelVM === 'object'"
            )
        except Exception:  # noqa: BLE001
            return False

    async def turnstile_asset(self) -> str:
        """Locate chatgpt.com's own turnstile chunk.

        The file name is content-hashed and changes on every deploy, so look for
        the URL that is actually loaded on this page first, then scan the page's
        own scripts for the literal path. The pinned path is only a last resort.
        """
        if self._turnstile_asset:
            return self._turnstile_asset
        try:
            loaded = await self.evaluate(_DISCOVER_TURNSTILE_ASSET, None)
            if loaded:
                self._turnstile_asset = str(loaded[0])
                print(f"[sentinel-solver] turnstile asset (loaded): {self._turnstile_asset}", flush=True)
                return self._turnstile_asset
            scanned = await self.evaluate(_SCAN_ASSET_IN_SCRIPTS, r"/unauth-mweb/assets/turnstile-[A-Za-z0-9_-]+\.js")
            if scanned:
                self._turnstile_asset = str(scanned)
                print(f"[sentinel-solver] turnstile asset (scanned): {self._turnstile_asset}", flush=True)
                return self._turnstile_asset
        except Exception as exc:  # noqa: BLE001
            self.last_error = str(exc)
        print(f"[sentinel-solver] turnstile asset discovery fell back to {_PINNED_ASSET}", flush=True)
        self._turnstile_asset = _PINNED_ASSET
        return self._turnstile_asset

    async def solve_turnstile(self, requirements: dict[str, Any] | None, dx: str, p: str) -> tuple[bool, str]:
        """Solve via the page's own turnstile module, falling back to the ported VM."""
        if requirements:
            asset = await self.turnstile_asset()
            for _ in range(VM_TIMEOUT_ATTEMPTS):
                try:
                    result = str(await self.evaluate(_SOLVE_VIA_SDK, [requirements, p, asset]) or "")
                except Exception as exc:  # noqa: BLE001
                    result = f"ERR:{exc}"
                if result and not result.startswith("ERR:"):
                    return True, result
                # A cold page often runs over the VM's 500ms budget; the page is fine, so retry.
                if "turnstile VM timed out" not in result:
                    break
            self.last_error = result[:300]
            print(f"[sentinel-solver] SDK solve failed via {asset}: {result[:300]}", flush=True)
            # Drop the cached asset and page so the next attempt re-discovers both.
            self._turnstile_asset = ""
            await self.invalidate()
        if not dx:
            return False, self.last_error or "no dx and no requirements supplied"
        result = str(await self.evaluate(_SOLVE_VIA_VM, [dx, p]) or "")
        if result.startswith("ERR:"):
            return False, result[4:]
        return bool(result), result

    async def evaluate(self, expression: str, arg: object) -> object:
        """Run in the page, reopening once if the page turned out to be dead."""
        for attempt in range(2):
            page = await self.page()
            try:
                return await page.evaluate(expression, arg)
            except Exception as exc:  # noqa: BLE001
                self.last_error = str(exc)
                if attempt:
                    raise
                await self.invalidate()
        raise RuntimeError("unreachable")

    def status(self) -> dict[str, object]:
        return {
            "ready": self._page is not None and not self._page.is_closed(),
            "page_age_seconds": int(time.monotonic() - self._opened_at) if self._opened_at else None,
            "user_agent": self.user_agent,
            "timezone": TIMEZONE or None,
            "proxy": redact_proxy(self.proxy),
            "last_error": self.last_error or None,
        }


class SolverPool:
    """One shared Chromium, with a page per egress proxy so the token is solved from the same IP."""

    def __init__(self) -> None:
        self._playwright: Playwright | None = None
        self._browser: Browser | None = None
        self._pages: dict[str, SolverPage] = {}
        self._lock = asyncio.Lock()
        self.user_agent = USER_AGENT

    async def start(self) -> None:
        self._playwright = await async_playwright().start()
        self._browser = await self._playwright.chromium.launch(
            headless=HEADLESS,
            # channel="chromium" uses full Chromium's new headless mode; the default
            # headless shell cannot clear the Cloudflare challenge.
            channel="chromium",
            args=["--disable-blink-features=AutomationControlled", "--no-sandbox", "--disable-dev-shm-usage"],
        )
        if not self.user_agent:
            major = self._browser.version.split(".")[0]
            self.user_agent = (
                "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
                f"(KHTML, like Gecko) Chrome/{major}.0.0.0 Safari/537.36"
            )
        solver = await self.get(None)
        try:
            await solver.page()
        except Exception as exc:  # noqa: BLE001
            # A page that cannot open yet is not fatal; the first request retries.
            solver.last_error = str(exc)
            print(f"[sentinel-solver] initial page open failed: {exc}", flush=True)

    async def stop(self) -> None:
        for solver in self._pages.values():
            await solver.stop()
        if self._browser is not None:
            await self._browser.close()
        if self._playwright is not None:
            await self._playwright.stop()

    async def get(self, proxy: str | None) -> SolverPage:
        """Page for this egress; `None` (old clients) means SENTINEL_SOLVER_PROXY, "" means direct."""
        key = PROXY if proxy is None else proxy.strip()
        async with self._lock:
            solver = self._pages.pop(key, None)
            if solver is None:
                solver = SolverPage(self._browser, self.user_agent, key)
                while len(self._pages) >= MAX_PROXY_PAGES:
                    oldest = next(iter(self._pages))
                    await self._pages.pop(oldest).stop()
            # Most recently used last, so eviction drops the stalest egress.
            self._pages[key] = solver
            return solver

    def status(self) -> dict[str, object]:
        default = self._pages.get(PROXY)
        status = default.status() if default else {"ready": False, "user_agent": self.user_agent}
        status["browser_version"] = self._browser.version if self._browser else None
        status["pages"] = [solver.status() for solver in self._pages.values()]
        return status


pool = SolverPool()


@asynccontextmanager
async def lifespan(_: FastAPI):
    await pool.start()
    yield
    await pool.stop()


app = FastAPI(title="chatgpt2api sentinel solver", lifespan=lifespan)


class TurnstileRequest(BaseModel):
    p: str
    dx: str = ""
    # The full `/chat-requirements/prepare` response: pure challenge material
    # (prepare token, dx, proof-of-work seed, SO handle). No account token,
    # cookie or user prompt is ever sent here.
    requirements: dict[str, Any] | None = None
    # The proxy the main service sends this account's requests through. The token
    # is only accepted from the same egress IP. Omitted by older clients.
    proxy: str | None = None


class SORequest(BaseModel):
    collector_dx: str
    snapshot_dx: str
    p: str
    proxy: str | None = None


def require_key(authorization: str | None) -> None:
    if not SOLVER_KEY:
        return
    supplied = (authorization or "").removeprefix("Bearer ").strip()
    if not hmac.compare_digest(supplied, SOLVER_KEY):
        raise HTTPException(status_code=401, detail="invalid solver key")


@app.get("/health")
async def health() -> dict[str, object]:
    return pool.status()


@app.post("/v1/turnstile")
async def solve_turnstile(body: TurnstileRequest, authorization: str | None = Header(default=None)) -> dict[str, str]:
    require_key(authorization)
    try:
        solver = await pool.get(body.proxy)
        ok, token = await solver.solve_turnstile(body.requirements, body.dx, body.p)
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=503, detail=f"turnstile solve failed: {exc}") from exc
    if not ok:
        raise HTTPException(status_code=503, detail=f"turnstile solve failed: {token[:300]}")
    return {"token": token}


@app.post("/v1/so")
async def solve_so(body: SORequest, authorization: str | None = Header(default=None)) -> dict[str, str]:
    require_key(authorization)
    try:
        solver = await pool.get(body.proxy)
        token = await solver.evaluate(
            "([c, s, p]) => window.__sentinelVM.so(c, s, p)", [body.collector_dx, body.snapshot_dx, body.p]
        )
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=503, detail=f"so solve failed: {exc}") from exc
    return {"token": str(token or "")}


if __name__ == "__main__":
    uvicorn.run(app, host="0.0.0.0", port=int(os.getenv("PORT", "8080")), access_log=False)
