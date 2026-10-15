import json
import random
import re
import time
from datetime import datetime, timedelta, timezone
from html.parser import HTMLParser
from typing import Any, Sequence

import pybase64

DEFAULT_POW_SCRIPT = "https://chatgpt.com/backend-api/sentinel/sdk.js"
from utils.helper import new_uuid


CORES = [8, 16, 24, 32]
DOCUMENT_KEYS = ["__reactContainer$fzelfjyxej8", "_reactListening5dehydibo78", "location"]
SCREEN_RESOLUTIONS = [[1920, 1080], [1440, 900], [2560, 1440], [3840, 2160]]
CHINA_TZ = timezone(timedelta(hours=8))
NAVIGATOR_KEYS = [
    "registerProtocolHandler−function registerProtocolHandler() { [native code] }",
    "storage−[object StorageManager]",
    "locks−[object LockManager]",
    "appCodeName−Mozilla",
    "permissions−[object Permissions]",
    "share−function share() { [native code] }",
    "webdriver−false",
    "managed−[object NavigatorManagedData]",
    "canShare−function canShare() { [native code] }",
    "vendor−Google Inc.",
    "mediaDevices−[object MediaDevices]",
    "vibrate−function vibrate() { [native code] }",
    "storageBuckets−[object StorageBucketManager]",
    "mediaCapabilities−[object MediaCapabilities]",
    "cookieEnabled−true",
    "virtualKeyboard−[object VirtualKeyboard]",
    "product−Gecko",
    "presentation−[object Presentation]",
    "onLine−true",
    "mimeTypes−[object MimeTypeArray]",
    "credentials−[object CredentialsContainer]",
    "serviceWorker−[object ServiceWorkerContainer]",
    "keyboard−[object Keyboard]",
    "gpu−[object GPU]",
    "doNotTrack",
    "serial−[object Serial]",
    "pdfViewerEnabled−true",
    "language−zh-CN",
    "geolocation−[object Geolocation]",
    "userAgentData−[object NavigatorUAData]",
    "getUserMedia−function getUserMedia() { [native code] }",
    "sendBeacon−function sendBeacon() { [native code] }",
    "hardwareConcurrency−32",
    "windowControlsOverlay−[object WindowControlsOverlay]",
]
WINDOW_KEYS = [
    "0",
    "window",
    "self",
    "document",
    "name",
    "location",
    "customElements",
    "history",
    "navigation",
    "innerWidth",
    "innerHeight",
    "scrollX",
    "scrollY",
    "visualViewport",
    "screenX",
    "screenY",
    "outerWidth",
    "outerHeight",
    "devicePixelRatio",
    "screen",
    "chrome",
    "navigator",
    "onresize",
    "performance",
    "crypto",
    "indexedDB",
    "sessionStorage",
    "localStorage",
    "scheduler",
    "alert",
    "atob",
    "btoa",
    "fetch",
    "matchMedia",
    "postMessage",
    "queueMicrotask",
    "requestAnimationFrame",
    "setInterval",
    "setTimeout",
    "caches",
    "__NEXT_DATA__",
    "__BUILD_MANIFEST",
    "__NEXT_PRELOADREADY",
]


class ScriptSrcParser(HTMLParser):
    def __init__(self) -> None:
        super().__init__()
        self.script_sources: list[str] = []
        self.data_build = ""

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        if tag != "script":
            return
        attrs_dict = dict(attrs)
        src = attrs_dict.get("src")
        if not src:
            return
        self.script_sources.append(src)
        match = re.search(r"c/[^/]*/_", src)
        if match:
            self.data_build = match.group(0)


def parse_pow_resources(html_content: str) -> tuple[list[str], str]:
    parser = ScriptSrcParser()
    parser.feed(html_content)
    script_sources = parser.script_sources or [DEFAULT_POW_SCRIPT]
    data_build = parser.data_build
    if not data_build:
        match = re.search(r'<html[^>]*data-build="([^"]*)"', html_content)
        if match:
            data_build = match.group(1)
    return script_sources, data_build


def _local_date_string() -> str:
    """按 zh-CN 浏览器 `"" + new Date` 的输出格式生成本地时间串。"""
    now = datetime.now(CHINA_TZ)
    return now.strftime("%a %b %d %Y %H:%M:%S") + " GMT+0800 (中国标准时间)"


def _b64_json(config: list[Any]) -> str:
    raw = json.dumps(config, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    return pybase64.b64encode(raw).decode("ascii")


def build_pow_config(
    user_agent: str,
    script_sources: Sequence[str] | None = None,
    data_build: str = "",
    base: list[Any] | None = None,
) -> list[Any]:
    """构造 sentinel PoW 配置（对齐 SDK 的 getConfig 输出，25 个字段）。

    传入 `base`（同一次「页面加载」内首次生成的配置）时，沿用其中的固定字段
    （屏幕尺寸、核心数、sid、timeOrigin 等），仅刷新随时间变化的字段，
    与浏览器 SDK 在同一页面会话内多次调用 getConfig 的行为保持一致。
    """
    script_source = random.choice(list(script_sources)) if script_sources else DEFAULT_POW_SCRIPT
    if base is not None:
        config = list(base)
        config[1] = _local_date_string()
        config[5] = script_source
        config[10] = random.choice(NAVIGATOR_KEYS)
        config[11] = random.choice(DOCUMENT_KEYS)
        config[12] = random.choice(WINDOW_KEYS)
        config[13] = round(time.time() * 1000 - float(config[17]), 3)
        return config
    elapsed_ms = random.uniform(800.0, 4000.0)
    return [
        sum(random.choice(SCREEN_RESOLUTIONS)),
        _local_date_string(),
        4294705152,
        1,
        user_agent,
        script_source,
        data_build,
        "zh-CN",
        "zh-CN,zh",
        0,
        random.choice(NAVIGATOR_KEYS),
        random.choice(DOCUMENT_KEYS),
        random.choice(WINDOW_KEYS),
        round(elapsed_ms, 3),
        new_uuid(),
        "",
        random.choice(CORES),
        round(time.time() * 1000 - elapsed_ms, 3),
        0, 0, 0, 0, 0, 0,
        0,  # 0 = edge/chrome, 1 = firefox
    ]


def _pow_hash(text: str) -> str:
    """sentinel SDK 内部使用的 FNV-1a 变体哈希，输出 8 位十六进制字符串。"""
    value = 2166136261
    for char in text:
        value ^= ord(char)
        value = (value * 16777619) & 0xFFFFFFFF
    value ^= value >> 16
    value = (value * 2246822507) & 0xFFFFFFFF
    value ^= value >> 13
    value = (value * 3266489909) & 0xFFFFFFFF
    value ^= value >> 16
    return format(value & 0xFFFFFFFF, "08x")


def _pow_generate(seed: str, difficulty: str, config: list[Any], limit: int = 500000) -> tuple[str, bool]:
    """按 sentinel SDK 的算法求解 proof-of-work。

    成功时返回 `(base64 配置 + "~S", True)`；`config[3]` 记录尝试序号、
    `config[9]` 记录求解耗时（毫秒），两者都会参与哈希输入。
    """
    difficulty_len = len(difficulty)
    start = time.perf_counter()
    for counter in range(limit):
        config[3] = counter
        config[9] = round((time.perf_counter() - start) * 1000)
        encoded = _b64_json(config)
        if _pow_hash(seed + encoded)[:difficulty_len] <= difficulty:
            return encoded + "~S", True
    return "", False


def build_legacy_requirements_token(
    user_agent: str,
    script_sources: Sequence[str] | None = None,
    data_build: str = "",
    base: list[Any] | None = None,
) -> str:
    config = build_pow_config(user_agent, script_sources=script_sources, data_build=data_build, base=base)
    config[3] = 1
    config[9] = round(random.uniform(0.1, 2.0), 1)
    return "gAAAAAC" + _b64_json(config)


def build_proof_token(
    seed: str,
    difficulty: str,
    user_agent: str,
    script_sources: Sequence[str] | None = None,
    data_build: str = "",
    base: list[Any] | None = None,
) -> str:
    config = build_pow_config(user_agent, script_sources=script_sources, data_build=data_build, base=base)
    answer, solved = _pow_generate(seed, difficulty, config)
    if not solved:
        raise RuntimeError(f"failed to solve proof token: difficulty={difficulty}")
    return "gAAAAAB" + answer
