from __future__ import annotations

import base64
import json
import re
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field, replace
from datetime import datetime, timezone
from typing import Any, Iterable, Iterator

import tiktoken

from services.account_service import account_service, is_edit_upload_throttled
from services.config import config
from services.image_storage_service import image_storage_service
from services.openai_backend_api import (
    ImageContentPolicyError,
    ImagePollTimeoutError,
    ImageUpstreamTaskError,
    OpenAIBackendAPI,
)
from utils.helper import (
    IMAGE_MODELS,
    UpstreamHTTPError,
    anonymize_token,
    extract_image_from_message_content,
    gif_frame_count,
    gif_frame_duration_ms,
    is_codex_image_model,
    is_gif_bytes,
    is_supported_image_model,
    split_image_model,
)
from utils.image_refusal import is_policy_refusal
from utils.image_tokens import count_image_content_tokens
from utils.image_upscale import fit_to_size, requested_dimensions, requested_long_edge, upscale_to_edge
from utils.log import logger


IMAGE_MODEL_NOT_FOUND_COOLDOWN_SECS = 10 * 60
IMAGE_FORBIDDEN_COOLDOWN_SECS = 10 * 60
# HTTP/2 流被中途重置（curl 92）：换号重试，每张图最多 2 次。
# 熔断：近 5 分钟全站断流超过 10 次多半是上游大面积故障，此时不再重试，免得白耗额度。
MAX_STREAM_RESET_RETRIES = 2
STREAM_RESET_BREAKER_WINDOW_SECS = 5 * 60
STREAM_RESET_BREAKER_LIMIT = 10
_stream_reset_times: list[float] = []
_stream_reset_lock = threading.Lock()


def is_stream_reset_error(message: str) -> bool:
    """HTTP/2 流没有正常结束就被掐断（curl 92，如 PROTOCOL_ERROR / INTERNAL_ERROR）。"""
    text = str(message or "").lower()
    return "curl: (92)" in text or "was not closed cleanly" in text


def _note_stream_reset() -> bool:
    """记一次断流；返回熔断是否已打开（近窗口内断流次数超过上限）。"""
    now = time.monotonic()
    with _stream_reset_lock:
        _stream_reset_times[:] = [t for t in _stream_reset_times if now - t < STREAM_RESET_BREAKER_WINDOW_SECS]
        _stream_reset_times.append(now)
        return len(_stream_reset_times) > STREAM_RESET_BREAKER_LIMIT


def is_upstream_forbidden_error(error: Exception | str) -> bool:
    """上游 403，多为 Cloudflare 按出口 IP 下发的人机验证。"""
    return isinstance(error, UpstreamHTTPError) and error.status_code == 403


def is_image_model_not_found_error(error: Exception | str) -> bool:
    """仅匹配图片对话接口返回的精确 Model not found 404。"""
    if not isinstance(error, UpstreamHTTPError):
        return False
    if error.status_code != 404:
        return False
    if error.context != "/backend-api/f/conversation":
        return False
    body = error.body
    if not isinstance(body, dict):
        return False
    return body.get("detail") == "Model not found"


class ImageGenerationError(Exception):
    def __init__(
        self,
        message: str,
        status_code: int = 502,
        error_type: str = "server_error",
        code: str | None = "upstream_error",
        param: str | None = None,
        account_email: str = "",
        conversation_id: str = "",
    ) -> None:
        super().__init__(message)
        self.status_code = status_code
        self.error_type = error_type
        self.code = code
        self.param = param
        self.account_email = account_email
        self.conversation_id = conversation_id

    def to_openai_error(self) -> dict[str, Any]:
        error_dict = {
            "error": {
                "message": public_image_error_message(str(self)),
                "type": self.error_type,
                "param": self.param,
                "code": self.code,
            }
        }
        if self.account_email:
            error_dict["error"]["account_email"] = self.account_email
        return error_dict


def public_image_error_message(message: str) -> str:
    text = str(message or "").strip()
    lower = text.lower()
    if any(item in lower for item in ("backend-api/", "status=", "body=", "chatgpt.com", "upstreamhttperror")):
        return "The image generation request failed. Please try again later."
    return text or "The image generation request failed. Please try again later."


def is_token_invalid_error(message: str) -> bool:
    text = str(message or "").lower()
    return (
        "token_invalidated" in text
        or "token_revoked" in text
        or "authentication token has been invalidated" in text
        or "invalidated oauth token" in text
    )


def is_tls_connection_error(message: str) -> bool:
    """检测 TLS/SSL 连接错误，这类错误通常可以通过重试解决。"""
    text = str(message or "").lower()
    return (
        "curl: (35)" in text
        or "tls connect error" in text
        or "openssl_internal" in text
        or "ssl: wrong_version_number" in text
        or "ssl: certificate_verify_failed" in text
        or "connection aborted" in text
        or "remote disconnected" in text
        or "connection reset by peer" in text
    )


def is_connection_timeout_error(message: str) -> bool:
    """检测连接超时错误（如 curl 28），这类错误可通过同账号短等待重试解决。"""
    text = str(message or "").lower()
    return (
        "curl: (28)" in text
        or "operation timed out" in text
        or "connection timed out" in text
        or "read timed out" in text
        or "connect timeout" in text
    )


def image_stream_error_message(message: str) -> str:
    text = str(message or "")
    if is_token_invalid_error(text):
        return "image generation failed"
    if is_tls_connection_error(text):
        return "upstream image connection failed, please retry later"
    if is_connection_timeout_error(text):
        return "upstream connection timed out, please retry later"
    if is_stream_reset_error(text):
        return "upstream connection was interrupted, please retry later"
    return text or "image generation failed"


REFERENCED_IMAGE_IDS_RE = re.compile(r'"referenced_image_ids"\s*:\s*\[([^\]]+)\]')
# 检测模型返回的部分工具调用 JSON（如 {"size":"1920x1088","n":1}）
# 这些 JSON 包含图片生成工具的参数，但没有实际生成图片
TOOL_PARAMS_JSON_RE = re.compile(
    r'\{\s*"size"\s*:\s*"\d+x\d+"\s*,\s*"n"\s*:\s*\d+\s*\}'
)


def is_model_text_reply_instead_of_image(message: str) -> bool:
    """检测模型是否返回了文本回复（包含工具调用 JSON）而非实际生成图片。

    当上游 ChatGPT 未能触发图片生成工具时，会返回一段描述性文本，
    其中可能包含 JSON 参数（如 prompt、referenced_image_ids、size/n 等）。
    这种情况应被视为「上游未生成图片」而非「内容策略违规」。

    检测两种模式：
    1. 完整的工具调用 JSON（含 referenced_image_ids）
    2. 部分的工具参数 JSON（如 {"size":"1920x1088","n":1}）
    """
    if not message:
        return False
    if REFERENCED_IMAGE_IDS_RE.search(message):
        return True
    # 检测部分工具参数 JSON（模型返回了工具参数但未触发工具）
    if TOOL_PARAMS_JSON_RE.search(message):
        return True
    return False


def encode_images(images: Iterable[tuple[bytes, str, str]]) -> list[str]:
    return [base64.b64encode(data).decode("ascii") for data, _, _ in images if data]


def save_image_bytes(image_data: bytes, base_url: str | None = None) -> str:
    return image_storage_service.save(image_data, base_url).url


def message_text(content: Any) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = []
        for item in content:
            if isinstance(item, str):
                parts.append(item)
            elif isinstance(item, dict) and str(item.get("type") or "") in {"text", "input_text", "output_text"}:
                parts.append(str(item.get("text") or ""))
        return "".join(parts)
    return ""


def normalize_messages(messages: object, system: Any = None) -> list[dict[str, Any]]:
    normalized = []
    if config.global_system_prompt:
        normalized.append({"role": "system", "content": config.global_system_prompt})
    system_text = message_text(system)
    if system_text:
        normalized.append({"role": "system", "content": system_text})
    if isinstance(messages, list):
        for message in messages:
            if not isinstance(message, dict):
                continue
            role = message.get("role", "user")
            content = message.get("content", "")
            text = message_text(content)
            images: list[tuple[bytes, str]] = []
            if role == "user":
                images.extend(extract_image_from_message_content(content))
                if isinstance(content, list):
                    for part in content:
                        if not isinstance(part, dict) or part.get("type") != "image":
                            continue
                        data = part.get("data")
                        if isinstance(data, (bytes, bytearray)) and all(existing[0] != bytes(data) for existing in images):
                            images.append((bytes(data), str(part.get("mime") or "image/png")))
            if images:
                parts: list[Any] = []
                if text:
                    parts.append({"type": "text", "text": text})
                for data, mime in images:
                    parts.append({"type": "image", "data": data, "mime": mime})
                normalized.append({"role": role, "content": parts})
            else:
                normalized.append({"role": role, "content": text})
    return normalized


def prompt_with_global_system(prompt: str) -> str:
    return f"{config.global_system_prompt}\n\n{prompt}" if config.global_system_prompt else prompt


def assistant_history_text(messages: list[dict[str, Any]]) -> str:
    return "".join(str(item.get("content") or "") for item in messages if item.get("role") == "assistant")


def assistant_history_messages(messages: list[dict[str, Any]]) -> list[str]:
    return [str(item.get("content") or "") for item in messages if item.get("role") == "assistant" and item.get("content")]


def build_image_prompt(
    prompt: str,
    size: str | None,
    quality: str = "auto",
    output_format: str = "png",
    frame_count: object = None,
    frame_duration_ms: object = None,
) -> str:
    hints = []
    if size:
        hints.append(f"输出图片尺寸为 {size}。")
    if quality:
        hints.append(f"输出图片质量为 {quality}。")
    # ponytail: web picture_v2 has no output_format; this sentence is the only GIF signal until a captured 2.6 payload exists
    if str(output_format or "").lower() == "gif":
        hints.append(
            f"输出动画 GIF，{gif_frame_count(frame_count)} 帧，每帧 {gif_frame_duration_ms(frame_duration_ms)} 毫秒。"
        )
    return f"{prompt.strip()}\n\n{''.join(hints)}" if hints else prompt


def encoding_for_model(model: str):
    try:
        return tiktoken.encoding_for_model(model)
    except KeyError:
        try:
            return tiktoken.get_encoding("o200k_base")
        except KeyError:
            return tiktoken.get_encoding("cl100k_base")


def count_message_image_tokens(messages: list[dict[str, Any]], model: str) -> int:
    return sum(count_image_content_tokens(message.get("content"), model) for message in messages)


def count_message_text_tokens(messages: list[dict[str, Any]], model: str) -> int:
    encoding = encoding_for_model(model)
    total = 0
    for message in messages:
        total += 3
        for key, value in message.items():
            if key == "content" and isinstance(value, list):
                total += len(encoding.encode(message_text(value)))
            elif isinstance(value, str):
                total += len(encoding.encode(value))
            else:
                continue
            if key == "name":
                total += 1
    return total + 3


def count_message_tokens(messages: list[dict[str, Any]], model: str) -> int:
    return count_message_text_tokens(messages, model) + count_message_image_tokens(messages, model)


def count_text_tokens(text: str, model: str) -> int:
    return len(encoding_for_model(model).encode(text))


def maybe_upscale_image(image_data: bytes, size: str | None = None) -> bytes:
    """按请求的尺寸处理返回的图；任何失败都返回原图。

    开启 image_exact_size_enabled 且请求里明确写了 宽x高（如 1024x1024）：裁剪到该比例
    并缩放到精确尺寸，过大缩小、过小放大（放大受 image_upscale_enabled 控制）。
    其余情况（含 auto、2K 这类写法）：开启放大时按设置的目标长边放大。
    """
    try:
        dimensions = requested_dimensions(size) if config.image_exact_size_enabled else None
        if dimensions:
            return fit_to_size(
                image_data, *dimensions,
                allow_upscale=config.image_upscale_enabled, method=config.image_upscale_method,
            )
        if not config.image_upscale_enabled:
            return image_data
        target = config.image_upscale_target_edge
        requested = requested_long_edge(size)
        if requested:
            target = min(target, requested)
        return upscale_to_edge(image_data, target, config.image_upscale_method)
    except Exception as exc:
        logger.warning({"event": "image_upscale_failed", "error": repr(exc)[:300]})
        return image_data


def format_image_result(
    items: list[dict[str, Any]],
    prompt: str,
    response_format: str,
    base_url: str | None = None,
    created: int | None = None,
    message: str = "",
    size: str | None = None,
) -> dict[str, Any]:
    data: list[dict[str, Any]] = []
    for item in items:
        b64_json = str(item.get("b64_json") or "").strip()
        if not b64_json:
            continue
        revised_prompt = str(item.get("revised_prompt") or prompt).strip() or prompt
        image_bytes = base64.b64decode(b64_json)
        upscaled = image_bytes if is_gif_bytes(image_bytes) else maybe_upscale_image(image_bytes, size)
        if upscaled is not image_bytes:
            image_bytes = upscaled
            b64_json = base64.b64encode(image_bytes).decode("ascii")
        if response_format == "b64_json":
            data.append({
                "b64_json": b64_json,
                "url": save_image_bytes(image_bytes, base_url),
                "revised_prompt": revised_prompt,
            })
        else:
            data.append({
                "url": save_image_bytes(image_bytes, base_url),
                "revised_prompt": revised_prompt,
            })
    result: dict[str, Any] = {"created": created or int(time.time()), "data": data}
    if message and not data:
        result["message"] = message
    return result


@dataclass
class ConversationRequest:
    model: str = "auto"
    prompt: str = ""
    messages: list[dict[str, Any]] | None = None
    thinking_effort: str = ""
    images: list[str] | None = None
    n: int = 1
    size: str | None = None
    quality: str = "auto"
    output_format: str = "png"
    frame_count: int | None = None
    frame_duration_ms: int | None = None
    response_format: str = "b64_json"
    base_url: str | None = None
    message_as_error: bool = False
    poll_timeout_secs: int | None = None
    # 换号生成：这次尝试最多等多少秒出图，超时就抛 ImagePollTimeoutError 让上层换号（由 _generate_single_image 设置）
    failover_timeout_secs: int | None = None
    # 确认上游已经开画时，在原账号上最多等多久（换号生成剩下的总时限，由 _generate_single_image 设置）
    task_poll_timeout_secs: int | None = None
    progress_callback: Any = None  # Callable[[str], None] | None


@dataclass
class ConversationState:
    text: str = ""
    raw_text: str = ""
    conversation_id: str = ""
    file_ids: list[str] = field(default_factory=list)
    sediment_ids: list[str] = field(default_factory=list)
    blocked: bool = False
    tool_invoked: bool | None = None
    turn_use_case: str = ""
    # 上游在 SSE 里给的结构化错误，如 {"error": "你提交的消息过长…", "error_code": "input_too_large"}
    error_code: str = ""
    error_message: str = ""
    # SSE 里出现了生图工具的消息：上游已经开画
    image_tool_called: bool = False


@dataclass
class ImageOutput:
    kind: str
    model: str
    index: int
    total: int
    created: int = field(default_factory=lambda: int(time.time()))
    text: str = ""
    upstream_event_type: str = ""
    data: list[dict[str, Any]] = field(default_factory=list)
    account_email: str = ""
    conversation_id: str = ""

    def to_chunk(self) -> dict[str, Any]:
        chunk: dict[str, Any] = {
            "object": "image.generation.chunk",
            "created": self.created,
            "model": self.model,
            "index": self.index,
            "total": self.total,
            "progress_text": self.text,
            "upstream_event_type": self.upstream_event_type,
            "data": [],
        }
        if self.account_email:
            chunk["_account_email"] = self.account_email
        if self.conversation_id:
            chunk["_conversation_id"] = self.conversation_id
        if self.kind == "message":
            chunk.update({
                "object": "image.generation.message",
                "message": self.text,
            })
            chunk.pop("progress_text", None)
            chunk.pop("upstream_event_type", None)
        elif self.kind == "result":
            chunk.update({
                "object": "image.generation.result",
                "data": self.data,
            })
            chunk.pop("progress_text", None)
            chunk.pop("upstream_event_type", None)
        return chunk


def assistant_message_text(message: dict[str, Any]) -> str:
    content = message.get("content") or {}
    parts = content.get("parts") or []
    if isinstance(parts, list) and parts:
        text = "".join(part for part in parts if isinstance(part, str))
        if text:
            return text
    # Fallback: content_type "code" stores text in the "text" field instead of "parts"
    text_field = str(content.get("text") or "")
    if text_field:
        return text_field
    return ""


def is_visible_assistant_message(message: dict[str, Any]) -> bool:
    """Return whether an upstream assistant message is intended for the user."""
    author = message.get("author")
    if not isinstance(author, dict):
        return False
    role = str(author.get("role") or "").strip().lower()
    if role != "assistant":
        return False

    metadata = message.get("metadata") or {}
    if isinstance(metadata, dict) and metadata.get("is_visually_hidden_from_conversation") is True:
        return False

    # Tool commands are emitted as assistant messages addressed to recipients
    # such as "web". Only messages addressed to everyone are user-visible.
    recipient = str(message.get("recipient") or "").strip().lower()
    if recipient and recipient != "all":
        return False

    # Reasoning and other internal channels must not leak into API text output.
    channel = str(message.get("channel") or "").strip().lower()
    if channel and channel != "final":
        return False

    return True


def strip_history(text: str, history_text: str = "") -> str:
    text = str(text or "")
    history_text = str(history_text or "")
    while history_text and text.startswith(history_text):
        text = text[len(history_text):]
    return text


def sanitize_output_text(text: str) -> str:
    text = str(text or "")

    def is_internal_annotation_part(part: str) -> bool:
        value = part.strip()
        if not value:
            return True
        lower = value.lower()
        return bool(
            re.fullmatch(r"turn\d+[a-z]*\d*", lower)
            or re.fullmatch(r"turn\d+\w*", lower)
            or lower.startswith(("turn", "source", "sources"))
        )

    def readable_annotation_part(parts: list[str]) -> str:
        for part in parts:
            value = part.strip()
            if value and not is_internal_annotation_part(value):
                return value
        return ""

    def annotation_text(payload: str) -> str:
        parts = [part.strip() for part in payload.split("\ue202")]
        kind = (parts[0] if parts else "").lower()
        data = parts[1:]
        if kind == "url":
            label = data[0] if data else ""
            url = data[1] if len(data) > 1 else ""
            if label and url.startswith(("http://", "https://")):
                return f"{label} ({url})"
            return label or url
        if kind == "cite":
            return readable_annotation_part(data)
        return readable_annotation_part(data)

    def replace_annotation(match: re.Match[str]) -> str:
        return annotation_text(match.group(1))

    def replace_annotation_before_punctuation(match: re.Match[str]) -> str:
        leading_space = match.group(1)
        replacement = annotation_text(match.group(2))
        return f"{leading_space}{replacement}" if replacement else ""

    # ChatGPT web sometimes returns rich annotation markers using private-use
    # characters. API clients cannot render those. Preserve readable labels
    # from entity/link annotations, while removing internal citation pointers.
    text = re.sub(r"(\s*)\ue200([^\ue201]*)\ue201(?=[.,;:!?])", replace_annotation_before_punctuation, text)
    text = re.sub(r"\ue200([^\ue201]*)\ue201", replace_annotation, text)
    text = re.sub(r"\ue200[^\ue201]*$", "", text)
    return text


def assistant_raw_text(event: dict[str, Any], current_text: str = "", history_text: str = "") -> str:
    for candidate in (event, event.get("v")):
        if not isinstance(candidate, dict):
            continue
        message = candidate.get("message")
        if not isinstance(message, dict) or not is_visible_assistant_message(message):
            continue
        text = assistant_message_text(message)
        if text:
            return strip_history(text, history_text)
    return apply_text_patch(event, current_text, history_text)


def assistant_text(event: dict[str, Any], current_text: str = "", history_text: str = "") -> str:
    return sanitize_output_text(assistant_raw_text(event, current_text, history_text))


def event_assistant_text(event: dict[str, Any], history_text: str = "") -> str:
    for candidate in (event, event.get("v")):
        if not isinstance(candidate, dict):
            continue
        message = candidate.get("message")
        if isinstance(message, dict) and is_visible_assistant_message(message):
            return strip_history(assistant_message_text(message), history_text)
    return ""


def apply_text_patch(event: dict[str, Any], current_text: str = "", history_text: str = "") -> str:
    if event.get("p") == "/message/content/parts/0":
        return apply_patch_op(event, current_text, history_text)

    operations = event.get("v")
    if isinstance(operations, str) and current_text and not event.get("p") and not event.get("o"):
        return current_text + operations

    if event.get("o") == "patch" and isinstance(operations, list):
        text = current_text
        for item in operations:
            if isinstance(item, dict):
                text = apply_text_patch(item, text, history_text)
        return text

    if not isinstance(operations, list):
        return current_text

    text = current_text
    for item in operations:
        if isinstance(item, dict):
            text = apply_text_patch(item, text, history_text)
    return text


def apply_patch_op(operation: dict[str, Any], current_text: str, history_text: str = "") -> str:
    op = operation.get("o")
    value = str(operation.get("v") or "")
    if op == "append":
        return current_text + value
    if op == "replace":
        return strip_history(value, history_text)
    return current_text


def add_unique(values: list[str], candidates: list[str]) -> None:
    for candidate in candidates:
        if candidate and candidate not in values:
            values.append(candidate)


FILE_SERVICE_ID_RE = re.compile(r"file-service://([A-Za-z0-9_-]+)")
FILE_ID_RE = re.compile(r"\b(file[-_](?!service\b)[A-Za-z0-9_-]+)\b")
# 真正的图片文件 ID 格式：file_00000000 + 24位十六进制字符（共32字符）
# 用于过滤非图片文件 ID（如 file_upload_business_upsell）
REAL_IMAGE_FILE_ID_RE = re.compile(r"\bfile_00000000[a-f0-9]{24}\b")
SEDIMENT_ID_RE = re.compile(r"sediment://([A-Za-z0-9_-]+)")


def extract_conversation_ids(payload: str) -> tuple[str, list[str], list[str]]:
    conversation_match = re.search(r'"conversation_id"\s*:\s*"([^"]+)"', payload)
    conversation_id = conversation_match.group(1) if conversation_match else ""
    file_ids: list[str] = []
    # Negative lookahead excludes "file-service" (URI prefix, not a real id).
    add_unique(file_ids, FILE_SERVICE_ID_RE.findall(payload))
    # 只提取真正的图片文件 ID（file_00000000... 格式），过滤非图片文件 ID（如 file_upload_business_upsell）
    add_unique(file_ids, REAL_IMAGE_FILE_ID_RE.findall(payload))
    sediment_ids = SEDIMENT_ID_RE.findall(payload)
    return conversation_id, file_ids, sediment_ids


def is_image_tool_event(event: dict[str, Any]) -> bool:
    value = event.get("v")
    message = event.get("message") or (value.get("message") if isinstance(value, dict) else None)
    if not isinstance(message, dict):
        return False
    metadata = message.get("metadata") or {}
    author = message.get("author") or {}
    content = message.get("content") or {}
    if author.get("role") != "tool":
        return False
    if metadata.get("async_task_type") == "image_gen":
        return True
    if content.get("content_type") != "multimodal_text":
        return False
    return any(
        isinstance(part, dict) and (
                part.get("content_type") == "image_asset_pointer"
                or str(part.get("asset_pointer") or "").startswith(("file-service://", "sediment://"))
        )
        for part in content.get("parts") or []
    )


def _is_user_message_event(event: dict[str, Any]) -> bool:
    """检查事件是否来自 user 角色消息。"""
    value = event.get("v")
    message = event.get("message") or (value.get("message") if isinstance(value, dict) else None)
    if isinstance(message, dict):
        author = message.get("author") or {}
        if str(author.get("role") or "").strip().lower() == "user":
            return True
    return False


def _is_image_tool_call_message(message: dict[str, Any]) -> bool:
    """会话文档里助手发给工具的生图调用：recipient 不是 all，内容是带 prompt 的参数 JSON。
    （代码执行等其它工具也会 recipient 不是 all，所以还要看内容。）"""
    author = message.get("author") or {}
    recipient = str(message.get("recipient") or "").strip()
    if str(author.get("role") or "") != "assistant" or recipient in {"", "all"}:
        return False
    content = message.get("content") or {}
    parts = [content.get("text")] + list(content.get("parts") or [])
    return any(isinstance(part, str) and '"prompt"' in part for part in parts)


def update_conversation_state(state: ConversationState, payload: str, event: dict[str, Any] | None = None) -> None:
    conversation_id, file_ids, sediment_ids = extract_conversation_ids(payload)
    if conversation_id and not state.conversation_id:
        state.conversation_id = conversation_id
    # Accept file_id / sediment_id when any of:
    #   1) event is a complete image_gen tool message
    #   2) prior server_ste_metadata already flipped tool_invoked True (in an image_gen turn),
    #      BUT only for non-user messages — user messages contain the uploaded input image
    #      which must NOT be treated as a generated output.
    #   3) patch event whose payload references asset_pointer / file-service://,
    #      BUT only when the event is not a user message.
    is_patch_event = isinstance(event, dict) and event.get("o") == "patch"
    is_user_msg = isinstance(event, dict) and _is_user_message_event(event)
    image_context = (
        (isinstance(event, dict) and is_image_tool_event(event))
        or (state.tool_invoked is True and not is_user_msg)
        or (is_patch_event and not is_user_msg and ("asset_pointer" in payload or "file-service://" in payload))
    )
    if image_context:
        add_unique(state.file_ids, file_ids)
        add_unique(state.sediment_ids, sediment_ids)
    if not isinstance(event, dict):
        return
    state.conversation_id = str(event.get("conversation_id") or state.conversation_id)
    value = event.get("v")
    if isinstance(value, dict):
        state.conversation_id = str(value.get("conversation_id") or state.conversation_id)
    if isinstance(event.get("error_code"), str) and event["error_code"].strip():
        state.error_code = event["error_code"].strip()
    if isinstance(event.get("error"), str) and event["error"].strip():
        state.error_message = event["error"].strip()
    if is_image_tool_event(event):
        state.image_tool_called = True
    if event.get("type") == "moderation":
        moderation = event.get("moderation_response")
        if isinstance(moderation, dict) and moderation.get("blocked") is True:
            state.blocked = True
    if event.get("type") == "server_ste_metadata":
        metadata = event.get("metadata")
        if isinstance(metadata, dict):
            if isinstance(metadata.get("tool_invoked"), bool):
                state.tool_invoked = metadata["tool_invoked"]
            state.turn_use_case = str(metadata.get("turn_use_case") or state.turn_use_case)


def conversation_base_event(event_type: str, state: ConversationState, **extra: Any) -> dict[str, Any]:
    return {
        "type": event_type,
        "text": state.text,
        "conversation_id": state.conversation_id,
        "file_ids": list(state.file_ids),
        "sediment_ids": list(state.sediment_ids),
        "blocked": state.blocked,
        "tool_invoked": state.tool_invoked,
        "turn_use_case": state.turn_use_case,
        "error_code": state.error_code,
        "error_message": state.error_message,
        "image_tool_called": state.image_tool_called,
        **extra,
    }


def iter_conversation_payloads(payloads: Iterator[str], history_text: str = "",
                               history_messages: list[str] | None = None) -> Iterator[dict[str, Any]]:
    state = ConversationState()
    history_messages = history_messages or []
    history_index = 0
    for payload in payloads:
        # print(f"[upstream_sse] {payload}", flush=True)
        if not payload:
            continue
        if payload == "[DONE]":
            yield conversation_base_event("conversation.done", state, done=True)
            break
        try:
            event = json.loads(payload)
        except json.JSONDecodeError:
            update_conversation_state(state, payload)
            yield conversation_base_event("conversation.raw", state, payload=payload)
            continue
        if not isinstance(event, dict):
            yield conversation_base_event("conversation.event", state, raw=event)
            continue
        update_conversation_state(state, payload, event)
        if history_index < len(history_messages) and event_assistant_text(event, history_text) == history_messages[history_index]:
            history_index += 1
            state.raw_text = ""
            state.text = ""
            continue
        next_raw_text = assistant_raw_text(event, state.raw_text, history_text)
        next_text = sanitize_output_text(next_raw_text)
        state.raw_text = next_raw_text
        if next_text != state.text:
            delta = next_text[len(state.text):] if next_text.startswith(state.text) else next_text
            state.text = next_text
            yield conversation_base_event("conversation.delta", state, raw=event, delta=delta)
            continue
        yield conversation_base_event("conversation.event", state, raw=event)


def conversation_events(
    backend: OpenAIBackendAPI,
    messages: list[dict[str, Any]] | None = None,
    model: str = "auto",
    prompt: str = "",
    images: list[str] | None = None,
    size: str | None = None,
    quality: str = "auto",
    output_format: str = "png",
    frame_count: object = None,
    frame_duration_ms: object = None,
    thinking_effort: str = "",
) -> Iterator[dict[str, Any]]:
    normalized = normalize_messages(messages or ([{"role": "user", "content": prompt}] if prompt else []))
    image_model = is_supported_image_model(model)
    history_text = "" if image_model else assistant_history_text(normalized)
    history_messages = [] if image_model else assistant_history_messages(normalized)
    final_prompt = prompt_with_global_system(build_image_prompt(
        prompt, size, quality, output_format, frame_count, frame_duration_ms,
    )) if image_model else prompt
    payloads = backend.stream_conversation(
        messages=normalized,
        model=model,
        prompt=final_prompt,
        images=images if image_model else None,
        system_hints=["picture_v2"] if image_model else None,
        thinking_effort=thinking_effort if not image_model else "",
        size=size if image_model else None,
    )
    yield from iter_conversation_payloads(payloads, history_text, history_messages)


def text_backend(model: str = "auto") -> OpenAIBackendAPI:
    return OpenAIBackendAPI(access_token=account_service.get_text_access_token(model=model))


def stream_text_deltas(backend: OpenAIBackendAPI, request: ConversationRequest) -> Iterator[str]:
    attempted_tokens: set[str] = set()
    token = getattr(backend, "access_token", "")
    emitted = False
    while True:
        if token and token in attempted_tokens:
            raise RuntimeError("no available text account")
        if token:
            attempted_tokens.add(token)
        active_backend = None
        try:
            active_backend = OpenAIBackendAPI(access_token=token)
            for event in conversation_events(
                active_backend,
                messages=request.messages,
                model=request.model,
                prompt=request.prompt,
                thinking_effort=request.thinking_effort,
            ):
                if event.get("type") != "conversation.delta":
                    continue
                delta = str(event.get("delta") or "")
                if delta:
                    emitted = True
                    yield delta
            account_service.mark_text_used(token)
            return
        except Exception as exc:
            error_message = str(exc)
            if token and not emitted and is_token_invalid_error(error_message):
                refreshed_token = account_service.refresh_access_token(token, force=True, event="text_stream")
                if refreshed_token and refreshed_token != token and refreshed_token not in attempted_tokens:
                    token = refreshed_token
                else:
                    account_service.remove_invalid_token(token, "text_stream", error=error_message)
                    token = account_service.get_text_access_token(
                        excluded_tokens=set(attempted_tokens),
                        model=request.model,
                    )
                if token:
                    continue
            raise
        finally:
            if active_backend is not None:
                active_backend.close()


def collect_text(backend: OpenAIBackendAPI, request: ConversationRequest) -> str:
    return "".join(stream_text_deltas(backend, request))


def _get_detailed_error_from_tasks(
    backend: OpenAIBackendAPI,
    conversation_id: str,
    timeout_secs: float = 10.0,
    wait_secs: float = 2.0,
) -> str:
    """从 /backend-api/tasks/ 接口获取结构化错误信息。

    当 SSE 流检测到 moderation 拦截时，轮询 tasks 接口获取详细错误文本。
    使用结构化字段（metadata.is_error, author.role, content.content_type）判断，
    而非依赖易变的文本匹配。

    参数：
    - `backend`：OpenAIBackendAPI 实例。
    - `conversation_id`：会话 ID。
    - `timeout_secs`：请求超时秒数。
    - `wait_secs`：等待任务创建的秒数。设为 0 可跳过等待。

    返回：
    - 详细错误信息文本，如果未找到则返回空字符串。
    """
    import time as _time
    try:
        if wait_secs > 0:
            _time.sleep(wait_secs)
        tasks = backend._query_backend_tasks(conversation_id=conversation_id, timeout_secs=timeout_secs)
        if not tasks:
            return ""

        for task in tasks:
            is_error, error_msg, metadata = backend.check_task_error(task)
            if is_error and error_msg:
                logger.info({
                    "event": "image_task_structured_error",
                    "conversation_id": conversation_id,
                    "error_msg": error_msg,
                    "metadata": metadata,
                })
                return error_msg
        return ""
    except Exception as exc:
        logger.warning({
            "event": "image_task_error_query_failed",
            "conversation_id": conversation_id,
            "error": str(exc),
        })
        return ""


def _remove_image_conversation_later(
        backend: OpenAIBackendAPI,
        conversation_id: str,
        *,
        success: bool,
) -> None:
    if not conversation_id:
        return
    if not (config.image_remove_conversation_always or (success and config.image_remove_conversation_after_result)):
        return

    def _run() -> None:
        try:
            backend.delete_conversation(conversation_id)
            logger.info({"event": "image_conversation_removed", "conversation_id": conversation_id})
        except Exception as exc:
            logger.warning({
                "event": "image_conversation_remove_failed",
                "conversation_id": conversation_id,
                "error": str(exc),
            })

    threading.Thread(target=_run, name=f"remove-image-conversation-{conversation_id}", daemon=True).start()


# ── 上游这一轮有没有开画 ───────────────────────────────────────
# 先判断开没开画，再决定等图、换号还是报错：没开画就不空等，开了画就不换号（换号会重复开画）。
INPUT_TOO_LARGE_CODE = "input_too_large"
# tool_invoked=false 后查一次会话和任务前先等一下：SSE 关闭后会话文档要几百毫秒才落库
NOT_STARTED_PROBE_WAIT_SECS = 2.0
# 只回文案或空回复：本次请求最多换一个没试过的号
MAX_NOT_STARTED_SWITCHES = 1
# 账号自己的问题（此对话无法生图、生图额度用完）：最多换这么多个没试过的号
MAX_ACCOUNT_ISSUE_SWITCHES = 3
# 「此对话无法生图」：同一账号在这么多个不同请求里出现，才短暂停止给它派新的生图
CANNOT_GENERATE_REPEAT_LIMIT = 2
CANNOT_GENERATE_WINDOW_SECS = 6 * 60 * 60
CANNOT_GENERATE_COOLDOWN_SECS = 30 * 60
# 上游说生图额度用完：按它给的重置时间暂停派工；没给就用账号记录的重置时间，再没有就暂停 1 小时
QUOTA_EXHAUSTED_DEFAULT_SECS = 60 * 60
QUOTA_EXHAUSTED_MAX_SECS = 48 * 60 * 60

NOT_STARTED_REASONS = {
    "text_only": "只回了文字，没有调用生图工具",
    "empty": "没有任何回复",
    "cannot_generate": "账号回复此对话无法生成图片",
    "quota_exhausted": "账号的生图额度已用完",
}

_CANNOT_GENERATE_RE = re.compile(
    r"(无法|不能|没法|没办法|無法)[^。！？!?\n]{0,20}(此|这个|這個|这|這|本|当前|當前)[个個]?(对话|對話|聊天|会话|會話)"
    r"[^。！？!?\n]{0,12}(生成|创建|創建|编辑|編輯|绘制|繪製|制作|製作|画|畫)"
    r"|(can't|cannot|can not|unable to|not able to)\s+(directly\s+)?(generate|create|edit|make|produce|draw)"
    r"[^.!?\n]{0,30}\b(images?|pictures?)\b[^.!?\n]{0,20}\b(this|the current)\s+(chat|conversation)",
    re.IGNORECASE,
)
# 上游服务自己出错：「生图服务暂时不可用，请稍后再试」
_SERVICE_ERROR_RE = re.compile(
    r"暂时不可用|暫時不可用|服务(?:繁忙|异常|出错|故障|不可用)|服務(?:繁忙|異常|出錯|故障|不可用)"
    r"|稍后再试|稍後再試|出了点问题|出了點問題|temporarily unavailable|service unavailable|try again later"
    r"|something went wrong",
    re.IGNORECASE,
)
# 线上原话：「我目前无法直接在此对图片进行生成或编辑」
_CANNOT_GENERATE_HERE_RE = re.compile(
    r"(无法|不能|没法|没办法|無法)[^。！？!?\n]{0,6}在(此|这|這)[^。！？!?\n]{0,10}"
    r"(生成|创建|創建|编辑|編輯|绘制|繪製|制作|製作)")
_QUOTA_IMAGE_RE = re.compile(r"图片|图像|圖片|圖像|生图|生圖|\bimages?\b", re.IGNORECASE)
_QUOTA_LIMIT_RE = re.compile(r"上限|限额|限額|额度|額度|\blimit\b", re.IGNORECASE)
_QUOTA_EXHAUSTED_RE = re.compile(r"已达|已達|达到|達到|用完|用尽|用盡|重置|\bhit\b|\breached\b|\bused up\b|\bresets?\b", re.IGNORECASE)
_RESET_HOURS_RE = re.compile(r"(\d+)\s*(?:hours?|hrs?|小时|小時)", re.IGNORECASE)
_RESET_MINUTES_RE = re.compile(r"(\d+)\s*(?:minutes?|mins?|分钟|分鐘)", re.IGNORECASE)

_cannot_generate_seen: dict[str, list[tuple[float, int]]] = {}
_cannot_generate_lock = threading.Lock()


class ImageInputTooLargeError(ImageGenerationError):
    """上游明确回了 input_too_large，且这一轮没有图也没有生图任务：是请求太长，不是账号故障。"""

    def __init__(self, upstream_message: str = "", conversation_id: str = "") -> None:
        message = "输入过长，上游拒绝处理：请缩短提示词或减少参考图后重试。"
        if upstream_message:
            message += f"（上游：{upstream_message[:200]}）"
        super().__init__(message, status_code=400, error_type="invalid_request_error",
                         code=INPUT_TOO_LARGE_CODE, conversation_id=conversation_id)
        self.error_code = INPUT_TOO_LARGE_CODE
        self.tool_invoked = False
        self.task_state = "none"


class ImageNotStartedError(ImageGenerationError):
    """上游这一轮没有开画（tool_invoked=false，查过会话和任务也没有生图任务）。
    由 _generate_single_image 按 reason 决定换号还是结束。"""

    def __init__(self, reason: str, upstream_text: str = "", *, error_code: str = "", conversation_id: str = "") -> None:
        message = f"上游未启动生图：{NOT_STARTED_REASONS.get(reason, reason)}"
        if error_code:
            message += f"（上游错误码 {error_code}）"
        if upstream_text:
            message += f"。上游回复：{upstream_text[:200]}"
        super().__init__(message, status_code=502, error_type="server_error",
                         code="image_generation_not_started", conversation_id=conversation_id)
        self.reason = reason
        self.upstream_text = upstream_text
        self.error_code = error_code
        self.tool_invoked = False
        self.task_state = "none"


@dataclass
class ImageTaskProbe:
    # images：会话里已经有图；running：有生图任务（还没出图）；refused：任务列表明确是内容政策拒绝；
    # service_error：任务列表报了别的错（如服务暂时不可用）；none：会话和任务都查到了，确认没有；unknown：有一处没查到
    state: str
    file_ids: list[str] = field(default_factory=list)
    sediment_ids: list[str] = field(default_factory=list)
    task_error: str = ""


def classify_not_started_text(text: str) -> str:
    """没开画时上游回的文字属于哪一类：
    policy / cannot_generate / quota_exhausted / service_error / text_only / empty。
    政策拒绝要整句判断（utils.image_refusal），只提到「内容政策」「无法生成」不算。"""
    text = str(text or "").strip()
    if not text:
        return "empty"
    if is_policy_refusal(text):
        return "policy"
    if _CANNOT_GENERATE_RE.search(text) or _CANNOT_GENERATE_HERE_RE.search(text):
        return "cannot_generate"
    if _QUOTA_IMAGE_RE.search(text) and _QUOTA_LIMIT_RE.search(text) and _QUOTA_EXHAUSTED_RE.search(text):
        return "quota_exhausted"
    if _SERVICE_ERROR_RE.search(text):
        return "service_error"
    return "text_only"


def quota_reset_secs(text: str, account: dict | None = None) -> int:
    """额度用完后多久重置：先看上游文字里的「N 小时 M 分钟」，再看账号记录的 image_gen 重置时间。"""
    hours = _RESET_HOURS_RE.search(str(text or ""))
    minutes = _RESET_MINUTES_RE.search(str(text or ""))
    secs = (int(hours.group(1)) * 3600 if hours else 0) + (int(minutes.group(1)) * 60 if minutes else 0)
    if not secs:
        for item in (account or {}).get("limits_progress") or []:
            if isinstance(item, dict) and item.get("feature_name") == "image_gen" and item.get("reset_after"):
                try:
                    reset_at = datetime.fromisoformat(str(item["reset_after"]).replace("Z", "+00:00"))
                    secs = int((reset_at - datetime.now(timezone.utc)).total_seconds())
                except ValueError:
                    secs = 0
    if secs <= 0:
        secs = QUOTA_EXHAUSTED_DEFAULT_SECS
    return min(secs, QUOTA_EXHAUSTED_MAX_SECS)


def note_cannot_generate(account_key: str, request_key: int) -> bool:
    """记一次「此对话无法生图」；同一账号在窗口内出现在足够多的不同请求里时返回 True。"""
    now = time.monotonic()
    with _cannot_generate_lock:
        seen = [(at, key) for at, key in _cannot_generate_seen.get(account_key, []) if now - at < CANNOT_GENERATE_WINDOW_SECS]
        seen.append((now, request_key))
        _cannot_generate_seen[account_key] = seen
        return len({key for _, key in seen}) >= CANNOT_GENERATE_REPEAT_LIMIT


def probe_image_task(backend: OpenAIBackendAPI, conversation_id: str) -> ImageTaskProbe:
    """查一次会话文档和任务列表，确认上游到底有没有开画。

    任何一处查不到都是 unknown：不能当成没开画，免得把仍在跑的任务当成没开画、换号重画。
    """
    if not conversation_id:
        return ImageTaskProbe("unknown")
    time.sleep(NOT_STARTED_PROBE_WAIT_SECS)
    try:
        document = backend._get_conversation(conversation_id)
    except Exception as exc:
        logger.warning({"event": "image_task_probe_failed", "conversation_id": conversation_id,
                        "source": "conversation", "error": repr(exc)[:300]})
        return ImageTaskProbe("unknown")
    probe = ImageTaskProbe("none")
    running = False
    for record in backend._extract_image_tool_records(document):
        running = True
        add_unique(probe.file_ids, record.get("file_ids") or [])
        add_unique(probe.sediment_ids, record.get("sediment_ids") or [])
    for node in (document.get("mapping") or {}).values():
        message = (node or {}).get("message") or {}
        if _is_image_tool_call_message(message) or (message.get("metadata") or {}).get("async_task_type") == "image_gen":
            running = True
    if probe.file_ids or probe.sediment_ids:
        probe.state = "images"
        return probe
    try:
        tasks = backend._query_backend_tasks(conversation_id=conversation_id, timeout_secs=5.0)
    except Exception as exc:
        logger.warning({"event": "image_task_probe_failed", "conversation_id": conversation_id,
                        "source": "tasks", "error": repr(exc)[:300]})
        probe.state = "running" if running else "unknown"
        return probe
    for task in tasks:
        is_error, error_msg, _ = backend.check_task_error(task)
        if is_error and error_msg and not is_model_text_reply_instead_of_image(error_msg):
            probe.task_error = error_msg
        else:
            # 没报错的任务在跑；上游有时把生图参数 JSON 标成 is_error，图其实还在生成
            running = True
    if running:
        probe.state = "running"
    elif probe.task_error:
        probe.state = "refused" if is_policy_refusal(probe.task_error) else "service_error"
    return probe


def _log_turn_assessment(conversation_id: str, tool_invoked: bool | None, error_code: str, task_state: str,
                         text_kind: str, *, started: bool = False) -> None:
    """每次尝试读完上游后的判断。不写提示词和 token。"""
    logger.info({
        "event": "image_turn_assessed",
        "conversation_id": conversation_id,
        "tool_invoked": tool_invoked,
        "error_code": error_code,
        "task_state": task_state,
        "text_kind": text_kind,
        "next": "wait_same_account" if started else ("not_started" if task_state == "none" else "resolve"),
    })


def stream_image_outputs(
        backend: OpenAIBackendAPI,
        request: ConversationRequest,
        index: int = 1,
        total: int = 1,
) -> Iterator[ImageOutput]:
    last: dict[str, Any] = {}
    for event in conversation_events(
            backend,
            prompt=request.prompt,
            model=request.model,
            images=request.images or [],
            size=request.size,
            quality=request.quality,
            output_format=request.output_format,
            frame_count=request.frame_count,
            frame_duration_ms=request.frame_duration_ms,
    ):
        last = event
        if event.get("type") == "conversation.delta":
            yield ImageOutput(
                kind="progress",
                model=request.model,
                index=index,
                total=total,
                text=str(event.get("delta") or ""),
                upstream_event_type="conversation.delta",
                conversation_id=str(event.get("conversation_id") or ""),
            )
            continue
        if event.get("type") == "conversation.event":
            raw = event.get("raw")
            raw_type = str(raw.get("type") or "") if isinstance(raw, dict) else ""
            yield ImageOutput(
                kind="progress",
                model=request.model,
                index=index,
                total=total,
                upstream_event_type=raw_type,
                conversation_id=str(event.get("conversation_id") or ""),
            )

    conversation_id = str(last.get("conversation_id") or "")
    file_ids = [str(item) for item in last.get("file_ids") or []]
    sediment_ids = [str(item) for item in last.get("sediment_ids") or []]
    message = str(last.get("text") or "").strip()
    tool_invoked = last.get("tool_invoked")
    error_code = str(last.get("error_code") or "")
    # 已经开画：SSE 里有生图工具消息，或 tool_invoked=true 且这一轮是生图。
    # tool_invoked=true 但用途是 data analysis 等时调的可能是别的工具，不算开画，按原流程处理。
    started = bool(last.get("image_tool_called")) or (tool_invoked is True and last.get("turn_use_case") == "image gen")
    logger.info({
        "event": "image_stream_resolve_start",
        "conversation_id": conversation_id,
        "file_ids": file_ids,
        "sediment_ids": sediment_ids,
        "tool_invoked": tool_invoked,
        "turn_use_case": last.get("turn_use_case"),
        "error_code": error_code,
        "image_tool_called": bool(last.get("image_tool_called")),
    })
    if request.progress_callback:
        request.progress_callback("image_stream_resolve_start")
    if message and not file_ids and not sediment_ids and last.get("blocked"):
        # 尝试从 /backend-api/tasks/ 获取详细错误信息
        detailed_error = _get_detailed_error_from_tasks(backend, conversation_id)
        error_text = detailed_error or message or "Image generation was rejected by upstream policy."
        yield ImageOutput(kind="message", model=request.model, index=index, total=total, text=error_text, conversation_id=conversation_id)
        return

    if message and not file_ids and not sediment_ids and is_policy_refusal(message):
        # 串流里已经明确回了政策拒绝：不会有图了，直接结束（图生图也不再等图、换号）
        _log_turn_assessment(conversation_id, tool_invoked, error_code, "not_checked", "policy")
        yield ImageOutput(kind="message", model=request.model, index=index, total=total, text=message, conversation_id=conversation_id)
        return

    # 上游明确说没调生图工具（tool_invoked=false）：查一次会话和任务确认真的没开画，没开画就不等图。
    # tool_invoked 缺失或查不到时保持「未知」，走下面原来的等图流程，免得误判仍在跑的任务。
    task_state = "sse_images" if (file_ids or sediment_ids) else ("started" if started else "not_checked")
    text_kind = ""
    unknown_after_check = False
    # 上游给了明确错误码（如 input_too_large）但没带 tool_invoked 时也查一次
    if not file_ids and not sediment_ids and not started and (tool_invoked is False or error_code):
        text_kind = classify_not_started_text(message)
        if text_kind != "policy" or error_code:
            probe = probe_image_task(backend, conversation_id)
            task_state = probe.state
            if probe.state == "images":
                add_unique(file_ids, probe.file_ids)
                add_unique(sediment_ids, probe.sediment_ids)
            elif probe.state == "running":
                started = True
            elif probe.state == "unknown":
                # 查不到：先在原账号上按单号时限等一轮确认，不当成没开画立刻换号或报错
                unknown_after_check = True
            elif probe.state == "service_error":
                # 任务列表报的是服务错误（如服务暂时不可用）：回上游服务错误，不能当成内容违规
                _log_turn_assessment(conversation_id, tool_invoked, error_code, task_state, "task_service_error")
                raise ImageUpstreamTaskError(probe.task_error, conversation_id)
            elif probe.state == "refused":
                # 任务列表明确是内容政策拒绝：交给原有的拒绝处理，结束这次请求
                _log_turn_assessment(conversation_id, tool_invoked, error_code, task_state, "task_refused")
                yield ImageOutput(kind="message", model=request.model, index=index, total=total,
                                  text=probe.task_error, conversation_id=conversation_id)
                return
            elif probe.state == "none":
                _log_turn_assessment(conversation_id, tool_invoked, error_code, task_state,
                                     "input_too_large" if error_code == INPUT_TOO_LARGE_CODE else text_kind)
                if error_code == INPUT_TOO_LARGE_CODE:
                    raise ImageInputTooLargeError(str(last.get("error_message") or ""), conversation_id)
                if text_kind == "service_error":
                    # 上游说生图服务出错：回上游服务错误，不换号、不算账号故障
                    raise ImageUpstreamTaskError(message, conversation_id)
                if text_kind != "policy":
                    if request.message_as_error:
                        raise ImageNotStartedError(text_kind, message, error_code=error_code, conversation_id=conversation_id)
                    # 对话类接口：上游的文字可能就是回答，原样给出；空回复给一句明确的说明
                    yield ImageOutput(kind="message", model=request.model, index=index, total=total,
                                      text=message or f"上游未启动生图：{NOT_STARTED_REASONS['empty']}",
                                      conversation_id=conversation_id)
                    return
    _log_turn_assessment(conversation_id, tool_invoked, error_code, task_state, text_kind or "", started=started)

    should_poll_for_image = (started or unknown_after_check or bool(request.images)
                             or last.get("turn_use_case") == "image gen")
    if message and not file_ids and not sediment_ids and not should_poll_for_image:
        yield ImageOutput(kind="message", model=request.model, index=index, total=total, text=message, conversation_id=conversation_id)
        return

    # 检测模型是否返回了文本描述（含 referenced_image_ids）而非实际生成图片
    # 这说明模型已发起图片生成工具调用，但 SSE 在工具完成前断开，
    # 图片可能正在异步生成中。需要使用更积极的轮询策略来获取结果。
    is_text_reply = bool(message and is_model_text_reply_instead_of_image(message))
    if is_text_reply:
        logger.info({
            "event": "image_detected_text_reply_with_ids",
            "conversation_id": conversation_id,
            "message_preview": message[:200],
        })

    # 当检测到文本回复但 conversation_id 丢失时，尝试从最近对话列表中恢复
    # SSE 流太短时（模型返回文本而非触发图片工具），conversation_id 可能未被捕获，
    # 但图片已在上游异步生成。通过列出最近对话来恢复 conversation_id。
    if is_text_reply and not conversation_id:
        try:
            import time as _time
            recovered_id = backend.find_conversation_by_prompt(
                request.prompt, _time.time(), timeout_secs=5.0,
            )
            if recovered_id:
                conversation_id = recovered_id
                logger.info({
                    "event": "image_conversation_id_recovered",
                    "conversation_id": conversation_id,
                    "message_preview": message[:200],
                })
        except Exception as exc:
            logger.warning({
                "event": "image_conversation_id_recovery_failed",
                "error": repr(exc)[:300],
            })

    # 在轮询图片之前，先检查 /backend-api/tasks/ 是否有 moderation 拦截
    # 这样可以避免不必要的长时间轮询超时
    # 注意：当 should_poll_for_image 为 True 或检测到文本回复时，
    # 即使 tasks 报告了"错误"，也不能直接返回——因为上游可能将工具调用的 JSON 参数
    # （如 {"size":"1792x1024","n":1}）标记为 is_error，而实际上图片正在异步生成中。
    # 此时应继续轮询图片。
    detailed_error = ""
    if not file_ids and not sediment_ids and conversation_id:
        detailed_error = _get_detailed_error_from_tasks(backend, conversation_id, timeout_secs=5.0, wait_secs=1.0)
        if detailed_error and not should_poll_for_image and not is_text_reply:
            logger.info({
                "event": "image_task_error_before_poll",
                "conversation_id": conversation_id,
                "error": detailed_error,
            })
            if not is_policy_refusal(detailed_error):
                raise ImageUpstreamTaskError(detailed_error, conversation_id)
            yield ImageOutput(kind="message", model=request.model, index=index, total=total, text=detailed_error, conversation_id=conversation_id)
            return
        if detailed_error and (should_poll_for_image or is_text_reply):
            logger.info({
                "event": "image_task_error_skipped_for_poll",
                "conversation_id": conversation_id,
                "error": detailed_error,
            })

    # 当检测到文本回复（含 referenced_image_ids）时，使用更长的超时来轮询图片结果。
    # 因为上游可能将图片生成作为异步任务执行，SSE 流在工具完成前就断开了，
    # 导致对话文档中尚未写入图片工具的响应记录。
    poll_timeout = request.failover_timeout_secs or request.poll_timeout_secs or config.image_poll_timeout_secs
    if started and request.task_poll_timeout_secs:
        # 确认已经开画：在原账号上把剩下的总时限等完，不按换号生成的单号时限换号（换号会重复开画）
        poll_timeout = max(poll_timeout, request.task_poll_timeout_secs)
    if is_text_reply and conversation_id and not request.poll_timeout_secs and not request.failover_timeout_secs:
        # 文本回复场景下图片可能仍在异步生成，使用更长超时（默认 120s → 额外 180s = 300s）
        poll_timeout = max(poll_timeout, 300)
        logger.info({
            "event": "image_text_reply_extended_poll",
            "conversation_id": conversation_id,
            "poll_timeout_secs": poll_timeout,
        })

    try:
        image_urls = backend.resolve_conversation_image_urls(
            conversation_id, file_ids, sediment_ids, poll_timeout_secs=poll_timeout,
        )
    except (ImageContentPolicyError, ImagePollTimeoutError) as exc:
        if started and isinstance(exc, ImagePollTimeoutError):
            exc.task_started = True  # 上层看到这个标记就不换号
            exc.waited_secs = poll_timeout
        # 当检测到文本回复时，task error 不应直接判定为内容策略违规，
        # 因为图片可能仍在后台异步生成中
        if is_text_reply and isinstance(exc, ImageContentPolicyError):
            logger.warning({
                "event": "image_text_reply_task_error_ignored",
                "conversation_id": conversation_id,
                "error": str(exc),
            })
            image_urls = []
        else:
            raise
    except Exception as exc:
        # 当检测到文本回复时，首次轮询的临时网络错误不应直接中断，
        # 因为图片可能仍在后台异步生成中，后续 retry poll 会继续尝试。
        if is_text_reply and conversation_id:
            logger.warning({
                "event": "image_text_reply_first_poll_error_ignored",
                "conversation_id": conversation_id,
                "error": repr(exc)[:300],
            })
            image_urls = []
        else:
            raise

    if image_urls:
        if request.progress_callback:
            request.progress_callback("receiving_image")
        image_items = [
            {"b64_json": base64.b64encode(image_data).decode("ascii")}
            for image_data in backend.download_image_bytes(image_urls)
        ]
        data = format_image_result(
            image_items,
            request.prompt,
            request.response_format,
            request.base_url,
            int(time.time()),
            size=request.size,
        )["data"]
        if data:
            yield ImageOutput(kind="result", model=request.model, index=index, total=total, data=data, conversation_id=conversation_id)
        return

    if message:
        # 检测模型是否返回了文本描述（含 referenced_image_ids）而非实际生成图片
        # 这说明模型已发起图片生成工具调用，但 SSE 在工具完成前断开。
        # 此时应再尝试轮询图片结果，而不是直接把文本当作最终输出。
        # 当 is_text_reply 但 conversation_id 丢失时，尝试从最近对话列表恢复
        if is_text_reply and not conversation_id:
            try:
                import time as _time
                recovered_id = backend.find_conversation_by_prompt(
                    request.prompt, _time.time(), timeout_secs=5.0,
                )
                if recovered_id:
                    conversation_id = recovered_id
                    logger.info({
                        "event": "image_text_reply_conversation_id_recovered",
                        "conversation_id": conversation_id,
                        "message_preview": message[:200],
                    })
            except Exception as exc:
                logger.warning({
                    "event": "image_text_reply_conversation_id_recovery_failed",
                    "error": repr(exc)[:300],
                })
        if is_text_reply and conversation_id:
            logger.info({
                "event": "image_model_text_reply_retry_poll",
                "conversation_id": conversation_id,
                "message_preview": message[:200],
            })
            # 文本回复场景下，图片可能需要 4-5 分钟才能异步生成完成。
            # 使用 300s 超时并允许多次重试，避免因临时网络问题提前退出。
            retry_poll_timeout = request.failover_timeout_secs or max(config.image_poll_timeout_secs, 300)
            MAX_POLL_RETRIES = 3
            for poll_attempt in range(1, MAX_POLL_RETRIES + 1):
                try:
                    polled_file_ids, polled_sediment_ids = backend._poll_image_results(
                        conversation_id,
                        retry_poll_timeout,
                        file_ids,
                        sediment_ids,
                    )
                    file_ids.extend(item for item in polled_file_ids if item and item not in file_ids)
                    sediment_ids.extend(item for item in polled_sediment_ids if item and item not in sediment_ids)
                    break  # 轮询成功，退出重试循环
                except Exception as exc:
                    if request.failover_timeout_secs and isinstance(exc, ImagePollTimeoutError):
                        raise  # 换号生成：交给上层换账号
                    error_str = str(exc)
                    is_transient = (
                        isinstance(exc, ImagePollTimeoutError)
                        or is_tls_connection_error(error_str)
                        or "upstream" in error_str.lower()
                        or "connection" in error_str.lower()
                        or "timeout" in error_str.lower()
                    )
                    logger.warning({
                        "event": "image_model_text_reply_poll_failed",
                        "conversation_id": conversation_id,
                        "poll_attempt": poll_attempt,
                        "error": repr(exc)[:300],
                        "is_transient": is_transient,
                    })
                    # 如果还有重试次数且不是超时/内容违规错误，继续重试
                    if poll_attempt < MAX_POLL_RETRIES and not isinstance(exc, (ImagePollTimeoutError, ImageContentPolicyError)):
                        # 递增退避：30s, 60s, 90s
                        backoff = 30.0 * poll_attempt
                        logger.info({
                            "event": "image_model_text_reply_poll_retry",
                            "conversation_id": conversation_id,
                            "poll_attempt": poll_attempt,
                            "backoff_secs": backoff,
                        })
                        time.sleep(backoff)
                        continue
                    # 超时错误或重试次数用尽，停止重试
                    break

            if file_ids or sediment_ids:
                image_urls = backend.resolve_conversation_image_urls(
                    conversation_id, file_ids, sediment_ids, poll=False,
                )
                if image_urls:
                    if request.progress_callback:
                        request.progress_callback("receiving_image")
                    image_items = [
                        {"b64_json": base64.b64encode(image_data).decode("ascii")}
                        for image_data in backend.download_image_bytes(image_urls)
                    ]
                    data = format_image_result(
                        image_items,
                        request.prompt,
                        request.response_format,
                        request.base_url,
                        int(time.time()),
                        size=request.size,
                    )["data"]
                    if data:
                        yield ImageOutput(kind="result", model=request.model, index=index, total=total, data=data, conversation_id=conversation_id)
                        return
        elif is_text_reply:
            logger.warning({
                "event": "image_model_text_reply_no_image",
                "conversation_id": conversation_id,
                "message_preview": message[:200],
            })
        yield ImageOutput(kind="message", model=request.model, index=index, total=total, text=message, conversation_id=conversation_id)
        return

    # 兜底：当 message 为空且图片 URL 解析失败时，先尝试一次短延迟重试轮询
    # 然后抛出明确错误而非让调用方得到 "upstream completed without generating images" 这种模糊报错
    logger.warning({
        "event": "image_stream_no_result_fallback",
        "conversation_id": conversation_id,
        "file_ids": file_ids,
        "sediment_ids": sediment_ids,
        "should_poll_for_image": should_poll_for_image,
    })
    # 当 should_poll_for_image 为 True 但 conversation_id 丢失时，尝试恢复
    if should_poll_for_image and not conversation_id:
        try:
            import time as _time
            recovered_id = backend.find_conversation_by_prompt(
                request.prompt, _time.time(), timeout_secs=5.0,
            )
            if recovered_id:
                conversation_id = recovered_id
                logger.info({
                    "event": "image_fallback_conversation_id_recovered",
                    "conversation_id": conversation_id,
                })
        except Exception as exc:
            logger.warning({
                "event": "image_fallback_conversation_id_recovery_failed",
                "error": repr(exc)[:300],
            })
    if should_poll_for_image and conversation_id:
        # 图片可能仍在异步处理中（上游 SSE 流在图片生成完成前就结束了）。
        # 使用 300s 超时并允许多次重试，避免因临时网络问题或图片尚未提交而提前退出。
        retry_poll_timeout = request.failover_timeout_secs or max(config.image_poll_timeout_secs, 300)
        MAX_FALLBACK_POLL_RETRIES = 3
        for poll_attempt in range(1, MAX_FALLBACK_POLL_RETRIES + 1):
            retry_wait_secs = min(30.0 * poll_attempt, config.image_poll_initial_wait_secs * poll_attempt)
            logger.info({
                "event": "image_stream_retry_poll_after_wait",
                "conversation_id": conversation_id,
                "retry_wait_secs": retry_wait_secs,
                "poll_attempt": poll_attempt,
            })
            time.sleep(retry_wait_secs)
            try:
                polled_file_ids, polled_sediment_ids = backend._poll_image_results(
                    conversation_id,
                    retry_poll_timeout,
                    file_ids,
                    sediment_ids,
                )
                file_ids.extend(item for item in polled_file_ids if item and item not in file_ids)
                sediment_ids.extend(item for item in polled_sediment_ids if item and item not in sediment_ids)
                break  # 轮询成功，退出重试循环
            except Exception as exc:
                if request.failover_timeout_secs and isinstance(exc, ImagePollTimeoutError):
                    raise  # 换号生成：交给上层换账号
                error_str = str(exc)
                is_transient = (
                    isinstance(exc, ImagePollTimeoutError)
                    or is_tls_connection_error(error_str)
                    or "upstream" in error_str.lower()
                    or "connection" in error_str.lower()
                    or "timeout" in error_str.lower()
                )
                logger.warning({
                    "event": "image_stream_retry_poll_failed",
                    "conversation_id": conversation_id,
                    "poll_attempt": poll_attempt,
                    "error": repr(exc)[:300],
                    "is_transient": is_transient,
                })
                # 如果还有重试次数且不是超时/内容违规错误，继续重试
                if poll_attempt < MAX_FALLBACK_POLL_RETRIES and not isinstance(exc, (ImagePollTimeoutError, ImageContentPolicyError)):
                    # 递增退避：30s, 60s
                    backoff = 30.0 * poll_attempt
                    logger.info({
                        "event": "image_stream_retry_poll_retry",
                        "conversation_id": conversation_id,
                        "poll_attempt": poll_attempt,
                        "backoff_secs": backoff,
                    })
                    time.sleep(backoff)
                    continue
                # 超时错误或重试次数用尽，停止重试
                break
        
        if file_ids or sediment_ids:
            image_urls = backend.resolve_conversation_image_urls(
                conversation_id, file_ids, sediment_ids, poll=False,
            )
            if image_urls:
                if request.progress_callback:
                    request.progress_callback("receiving_image")
                image_items = [
                    {"b64_json": base64.b64encode(image_data).decode("ascii")}
                    for image_data in backend.download_image_bytes(image_urls)
                ]
                data = format_image_result(
                    image_items,
                    request.prompt,
                    request.response_format,
                    request.base_url,
                    int(time.time()),
                    size=request.size,
                )["data"]
                if data:
                    yield ImageOutput(kind="result", model=request.model, index=index, total=total, data=data, conversation_id=conversation_id)
                    return
        
        # 重试后仍然失败，yield 错误消息
        yield ImageOutput(kind="message", model=request.model, index=index, total=total,
                          text="Image generation completed upstream but the result could not be retrieved. "
                               "The image may still be processing. Please try again in a moment.",
                          conversation_id=conversation_id)
    elif message:
        yield ImageOutput(kind="message", model=request.model, index=index, total=total, text=message, conversation_id=conversation_id)
    else:
        # conversation_id 也为空时（SSE 流极短、未捕获到会话 ID），
        # 仍然 yield 一条消息，避免 stream_image_outputs_with_pool 产生
        # "upstream completed without generating images" 模糊报错
        yield ImageOutput(kind="message", model=request.model, index=index, total=total,
                          text="Image generation started upstream but the response was incomplete. "
                               "Please try again.",
                          conversation_id=conversation_id)


def _codex_response_images(value: Any) -> list[str]:
    if isinstance(value, dict):
        if value.get("type") == "image_generation_call" and isinstance(value.get("result"), str):
            result = value["result"].strip()
            if result:
                return [result.split(",", 1)[1] if result.startswith("data:image/") else result]
        images: list[str] = []
        for item in value.values():
            images.extend(_codex_response_images(item))
        return images
    if isinstance(value, list):
        images: list[str] = []
        for item in value:
            images.extend(_codex_response_images(item))
        return images
    return []


def stream_codex_image_outputs(
        backend: OpenAIBackendAPI,
        request: ConversationRequest,
        index: int = 1,
        total: int = 1,
) -> Iterator[ImageOutput]:
    images = _codex_response_images(list(backend.iter_codex_image_response_events(
        prompt=request.prompt,
        images=request.images or [],
        size=request.size,
        quality=request.quality,
        model=request.model,
        output_format=request.output_format,
        frame_count=request.frame_count,
        frame_duration_ms=request.frame_duration_ms,
    )))
    if not images:
        raise ImageGenerationError("No image result found in response")
    data = format_image_result(
        [{"b64_json": item, "revised_prompt": request.prompt} for item in images],
        request.prompt,
        request.response_format,
        request.base_url,
        int(time.time()),
        size=request.size,
    )["data"]
    if data:
        yield ImageOutput(kind="result", model=request.model, index=index, total=total, data=data)
        return
    raise ImageGenerationError("No image result found in response")


class _ImageSlot:
    """取到一个号就占它 1 个并发名额。不管这次尝试怎么结束，名额只还一次：
    success=True/False 记成功/失败（mark_image_result 里会还名额），None 只还名额不记失败。"""

    def __init__(self, token: str) -> None:
        self.token = token
        self.finished = False

    def finish(self, success: bool | None) -> None:
        if self.finished:
            return
        self.finished = True
        if success is None:
            account_service.release_image_slot(self.token)
        else:
            account_service.mark_image_result(self.token, success)


def _log_attempt(index: int, token: str, account_email: str, decision: str, reason: str,
                 exc: Exception | None = None, conversation_id: str = "", **extra: Any) -> None:
    """每次尝试的结局：成功、在原号上等、换号还是结束，以及原因。不写提示词和完整 token。"""
    logger.warning({
        "event": "image_attempt_outcome",
        "index": index,
        "token": anonymize_token(token),
        "account_email": account_email,
        "conversation_id": conversation_id or str(getattr(exc, "conversation_id", "") or ""),
        "error_code": str(getattr(exc, "error_code", "") or ""),  # 上游给的错误码
        "code": str(getattr(exc, "code", "") or ""),  # 回给调用方的错误码
        "tool_invoked": getattr(exc, "tool_invoked", None),
        "task_state": str(getattr(exc, "task_state", "") or ""),
        "decision": decision,
        "reason": reason,
        **extra,
    })


def _generate_single_image(
        request: ConversationRequest,
        index: int,
        total: int,
) -> list[ImageOutput]:
    """为单张图片执行生成逻辑（含重试），返回结果列表。

    该函数在独立线程中运行，每个线程使用不同的账号，
    实现并行生图，避免串行超时阻塞。
    """
    # 模型返回文本而非图片的最大重试次数
    MAX_TEXT_REPLY_RETRIES = 3
    # TLS 连接错误最大重试次数
    MAX_TLS_RETRIES = 3
    # 连接超时错误最大重试次数（同账号短等待重试）
    MAX_CONN_TIMEOUT_RETRIES = 3
    # 轮询超时错误最大重试次数（换账号重试）
    MAX_POLL_TIMEOUT_RETRIES = 4
    # 换号生成：总时限剩下不到这么多秒就不再换号
    MIN_FAILOVER_SECS = 15
    # 图生图上传上限：冻当前号后换下一个，最多再试 5 个不同号
    MAX_EDIT_UPLOAD_THROTTLE_RETRIES = 5
    # 上游 403：冷却当前号，换其他出口的号重试
    MAX_FORBIDDEN_RETRIES = 3

    text_reply_retry_count = 0
    tls_retry_count = 0
    conn_timeout_retry_count = 0
    poll_timeout_retry_count = 0
    edit_upload_retry_count = 0
    model_not_found_retry_count = 0
    forbidden_retry_count = 0
    stream_reset_retry_count = 0
    not_started_switch_count = 0
    account_issue_switch_count = 0
    tried_upload_tokens: set[str] = set()
    # 上游没开画后换下来的号：本次请求不再选（和 avoided_tokens 不同，没号可选时也不清空）
    not_started_tokens: set[str] = set()
    last_not_started: ImageGenerationError | None = None
    # 换号生成：每个号最多等 image_failover_secs 出图，超时换号；所有尝试合计不超过总的等图时限。
    # 调用方显式给了 poll_timeout_secs（如账号测试）时不换号。
    failover_enabled = config.image_failover_enabled and not request.poll_timeout_secs
    failover_budget_secs = config.image_poll_timeout_secs
    # 403 后本次请求暂避与该号同出口的账号（只影响本次请求）
    avoided_tokens: set[str] = set()
    for_edits = bool(request.images)
    account_email = ""
    request_key = id(request)

    while True:
        try:
            if request.progress_callback:
                request.progress_callback("getting_account")
            plan_type, _ = split_image_model(request.model)
            codex_model = is_codex_image_model(request.model)
            token = account_service.get_available_access_token(
                plan_type=plan_type,
                source_type="codex" if codex_model else None,
                plan_types=("plus", "team", "pro") if codex_model and not plan_type else None,
                excluded_tokens=tried_upload_tokens | avoided_tokens | not_started_tokens,
                for_edits=for_edits,
            )
        except RuntimeError as exc:
            if avoided_tokens:
                # 其他出口没有可用的号，退回同出口的号
                avoided_tokens.clear()
                continue
            if last_not_started is not None:
                # 没有没试过的号了：报上一次没开画的原因，比「没有可用额度」更有用
                raise last_not_started from exc
            raise ImageGenerationError(str(exc) or "image generation failed", account_email=account_email) from exc

        slot = _ImageSlot(token)
        emitted_for_token = False
        returned_message = False
        returned_result = False
        attempt_request = request
        attempt_wait_secs = 0
        if failover_enabled:
            attempt_wait_secs = max(1, min(config.image_failover_secs, failover_budget_secs))
            attempt_request = replace(request, failover_timeout_secs=attempt_wait_secs,
                                      task_poll_timeout_secs=failover_budget_secs)
        account = account_service.get_account(token) or {}
        account_email = str(account.get("email") or "").strip()
        logger.debug({
            "event": "image_account_lookup",
            "token": anonymize_token(token),
            "account_email": account_email,
            "account_found": bool(account),
            "index": index,
        })
        backend = None
        try:
            backend = OpenAIBackendAPI(access_token=token)
            if request.progress_callback:
                backend.progress_callback = request.progress_callback
            stream_fn = stream_codex_image_outputs if is_codex_image_model(request.model) else stream_image_outputs
            outputs: list[ImageOutput] = []
            last_conversation_id = ""
            try:
                for output in stream_fn(backend, attempt_request, index, total):
                    last_conversation_id = output.conversation_id or last_conversation_id
                    if account_email and not output.account_email:
                        output.account_email = account_email
                    if output.kind == "message" and request.message_as_error:
                        raise ImageGenerationError(
                            output.text or "Image generation was rejected by upstream policy.",
                            status_code=400,
                            error_type="invalid_request_error",
                            code="content_policy_violation",
                            account_email=account_email,
                            conversation_id=output.conversation_id,
                        )
                    emitted_for_token = True
                    returned_message = output.kind == "message"
                    returned_result = returned_result or output.kind == "result"
                    outputs.append(output)
            except Exception as exc:
                # 异常路径（内容政策拒绝、轮询超时等）会话 ID 只挂在异常上
                last_conversation_id = last_conversation_id or str(getattr(exc, "conversation_id", "") or "")
                raise
            finally:
                _remove_image_conversation_later(backend, last_conversation_id, success=returned_result)
            if returned_message:
                slot.finish(False)
                _log_attempt(index, token, account_email, "finish", "returned_message", conversation_id=last_conversation_id)
                return outputs
            if not returned_result:
                slot.finish(False)
                if emitted_for_token:
                    conv_id = outputs[-1].conversation_id if outputs else ""
                    raise ImageGenerationError(
                        "upstream completed without generating images",
                        status_code=400,
                        error_type="invalid_request_error",
                        code="no_image_generated",
                        account_email=account_email,
                        conversation_id=conv_id,
                    )
                return outputs
            slot.finish(True)
            _log_attempt(index, token, account_email, "success", "image_returned", conversation_id=last_conversation_id)
            return outputs
        except ImageInputTooLargeError as exc:
            # 请求本身太长，换哪个号都一样；也不算账号故障
            slot.finish(None)
            exc.account_email = exc.account_email or account_email
            _log_attempt(index, token, account_email, "finish", "input_too_large", exc)
            raise
        except ImageNotStartedError as exc:
            exc.account_email = exc.account_email or account_email
            reason = exc.reason
            if reason == "cannot_generate":
                # 这个号在此对话里生不了图：记账号失败；同一个号在不同请求里反复这样才暂停给它派新的生图
                slot.finish(False)
                if note_cannot_generate(account_email or token, request_key):
                    account_service.sideline_image_account(token, CANNOT_GENERATE_COOLDOWN_SECS)
                    _log_attempt(index, token, account_email, "sideline", "cannot_generate_repeated", exc,
                                 sideline_secs=CANNOT_GENERATE_COOLDOWN_SECS)
            elif reason == "quota_exhausted":
                # 额度用完：按重置时间暂停派新的生图，账号状态和已在跑的任务都不动
                slot.finish(None)
                reset_secs = quota_reset_secs(exc.upstream_text, account)
                account_service.sideline_image_account(token, reset_secs)
                _log_attempt(index, token, account_email, "sideline", "quota_exhausted", exc, sideline_secs=reset_secs)
            else:
                # 只回文案或空回复：多半跟这次的请求有关，不算账号失败
                slot.finish(None)
            not_started_tokens.add(token)
            last_not_started = exc
            if reason in {"cannot_generate", "quota_exhausted"}:
                can_switch = account_issue_switch_count < MAX_ACCOUNT_ISSUE_SWITCHES
                if can_switch:
                    account_issue_switch_count += 1
            else:
                can_switch = not_started_switch_count < MAX_NOT_STARTED_SWITCHES
                if can_switch:
                    not_started_switch_count += 1
            _log_attempt(index, token, account_email, "switch" if can_switch else "finish", f"not_started:{reason}", exc)
            if can_switch:
                continue
            raise
        except ImagePollTimeoutError as exc:
            if account_email:
                setattr(exc, "account_email", account_email)
            if getattr(exc, "task_started", False):
                # 上游已经开画，已经在原号上把剩下的时限等完：不换号（换号会重复开画）
                slot.finish(False)
                waited = int(getattr(exc, "waited_secs", 0) or failover_budget_secs)
                _log_attempt(index, token, account_email, "finish", "task_started_timeout", exc, waited_secs=waited)
                started_error = ImagePollTimeoutError(
                    f"ChatGPT 生图超时：上游已经开始生成，同一个账号等了 {waited} 秒仍没有出图。"
                    f"为免重复生成没有换号，请稍后重试。",
                    exc.conversation_id,
                )
                if account_email:
                    setattr(started_error, "account_email", account_email)
                raise started_error from exc
            if failover_enabled:
                # 换号生成：超时是这次对话卡住，不算账号失败；换一个号重新生成
                failover_budget_secs -= attempt_wait_secs
                can_retry = (
                    not (returned_message or returned_result)
                    and failover_budget_secs >= MIN_FAILOVER_SECS
                    and poll_timeout_retry_count < MAX_POLL_TIMEOUT_RETRIES
                )
                if can_retry:
                    poll_timeout_retry_count += 1
                    avoided_tokens.add(token)
                    # 不记账号失败，但并发名额必须还
                    slot.finish(None)
                    _log_attempt(index, token, account_email, "switch", "failover_timeout", exc,
                                 waited_secs=attempt_wait_secs, budget_left_secs=failover_budget_secs,
                                 retry_count=poll_timeout_retry_count)
                    continue
                slot.finish(False)
                _log_attempt(index, token, account_email, "finish", "failover_exhausted", exc,
                             retry_count=poll_timeout_retry_count)
                failover_error = ImagePollTimeoutError(
                    f"ChatGPT 生图超时：换了 {poll_timeout_retry_count + 1} 个账号，"
                    f"每个最多等 {config.image_failover_secs} 秒都没有出图，上游可能正拥堵，请稍后重试。",
                    exc.conversation_id,
                )
                if account_email:
                    setattr(failover_error, "account_email", account_email)
                raise failover_error from exc
            slot.finish(False)
            # 轮询超时：换账号重试
            if not emitted_for_token:
                poll_timeout_retry_count += 1
                if poll_timeout_retry_count <= MAX_POLL_TIMEOUT_RETRIES:
                    _log_attempt(index, token, account_email, "switch", "poll_timeout", exc,
                                 retry_count=poll_timeout_retry_count)
                    continue
                _log_attempt(index, token, account_email, "finish", "poll_timeout_exhausted", exc,
                             retry_count=poll_timeout_retry_count)
                raise
            raise
        except ImageUpstreamTaskError as exc:
            # 上游生图服务自己出错，不是账号故障、也不是内容违规
            slot.finish(None)
            _log_attempt(index, token, account_email, "finish", "upstream_service_error", exc, error=str(exc)[:200])
            raise ImageGenerationError(
                f"上游生图服务出错：{str(exc)[:200] or '未知错误'}，请稍后重试。",
                status_code=502,
                error_type="server_error",
                code="upstream_service_error",
                account_email=account_email,
                conversation_id=getattr(exc, "conversation_id", ""),
            ) from exc
        except ImageContentPolicyError as exc:
            slot.finish(False)
            _log_attempt(index, token, account_email, "finish", "content_policy", exc, error=str(exc)[:200])
            raise ImageGenerationError(
                str(exc) or "Image generation was rejected by upstream policy.",
                status_code=400,
                error_type="invalid_request_error",
                code="content_policy_violation",
                account_email=account_email,
                conversation_id=getattr(exc, "conversation_id", ""),
            ) from exc
        except ImageGenerationError as exc:
            slot.finish(False)
            if account_email and not getattr(exc, "account_email", ""):
                exc.account_email = account_email
            error_text = str(exc)
            # 模型回了带生图参数 JSON 的文字、等图后仍没有图：换一个没试过的号重试。
            # 只看这次有没有出图，普通的 SSE 进度不挡重试。
            if is_model_text_reply_instead_of_image(error_text) and not returned_result:
                text_reply_retry_count += 1
                not_started_tokens.add(token)
                if text_reply_retry_count <= MAX_TEXT_REPLY_RETRIES:
                    _log_attempt(index, token, account_email, "switch", "text_reply_with_tool_params", exc,
                                 retry_count=text_reply_retry_count)
                    continue
                _log_attempt(index, token, account_email, "finish", "text_reply_retries_exhausted", exc,
                             retry_count=text_reply_retry_count)
                raise ImageGenerationError(
                    "Image generation failed: the upstream model returned a text description "
                    "instead of generating an image. Please try again later.",
                    status_code=502,
                    error_type="server_error",
                    code="upstream_text_reply",
                    account_email=account_email,
                    conversation_id=getattr(exc, "conversation_id", ""),
                ) from exc
            _log_attempt(index, token, account_email, "finish", "generation_error", exc, error=error_text[:200])
            raise
        except Exception as exc:
            slot.finish(False)
            last_error = str(exc)
            logger.warning({
                "event": "image_stream_fail",
                "token": anonymize_token(token),
                "account_email": account_email,
                "error": last_error,
                "index": index,
            })
            if not emitted_for_token and is_image_model_not_found_error(exc):
                # 仅本地暂避账号，不修改额度、有效性或代理健康状态。
                account_service.sideline_image_account(token, IMAGE_MODEL_NOT_FOUND_COOLDOWN_SECS)
                model_not_found_retry_count += 1
                if model_not_found_retry_count <= MAX_POLL_TIMEOUT_RETRIES:
                    continue
            if not emitted_for_token and is_token_invalid_error(last_error):
                refreshed_token = account_service.refresh_access_token(token, force=True, event="image_stream")
                if refreshed_token and refreshed_token != token:
                    token = refreshed_token
                    continue
                account_service.remove_invalid_token(token, "image_stream", error=last_error)
                continue
            if is_upstream_forbidden_error(exc):
                # 403 多为 Cloudflare 按出口 IP 验证：冷却该号；同出口的号多半也会被验证，本次先避开
                account_service.sideline_image_account(token, IMAGE_FORBIDDEN_COOLDOWN_SECS)
                forbidden_retry_count += 1
                if not emitted_for_token and forbidden_retry_count <= MAX_FORBIDDEN_RETRIES:
                    avoided_tokens |= account_service.same_proxy_tokens(token)
                    logger.warning({
                        "event": "image_forbidden_retry",
                        "token": anonymize_token(token),
                        "account_email": account_email,
                        "retry_count": forbidden_retry_count,
                        "index": index,
                        "error": last_error[:200],
                    })
                    continue
            # HTTP/2 断流：换一个号重试（本次请求避开刚断的号）
            if not emitted_for_token and is_stream_reset_error(last_error):
                breaker_open = _note_stream_reset()
                stream_reset_retry_count += 1
                if not breaker_open and stream_reset_retry_count <= MAX_STREAM_RESET_RETRIES:
                    avoided_tokens.add(token)
                    logger.warning({
                        "event": "image_stream_reset_retry",
                        "token": anonymize_token(token),
                        "account_email": account_email,
                        "retry_count": stream_reset_retry_count,
                        "index": index,
                        "error": last_error[:200],
                    })
                    continue
                if breaker_open:
                    logger.warning({
                        "event": "image_stream_reset_breaker_open",
                        "account_email": account_email,
                        "index": index,
                    })
            # TLS/SSL 连接错误：自动重试
            if not emitted_for_token and is_tls_connection_error(last_error):
                tls_retry_count += 1
                if tls_retry_count <= MAX_TLS_RETRIES:
                    logger.warning({
                        "event": "image_stream_tls_retry",
                        "token": anonymize_token(token),
                        "account_email": account_email,
                        "retry_count": tls_retry_count,
                        "index": index,
                        "error": last_error[:200],
                    })
                    time.sleep(min(2.0 * tls_retry_count, 10.0))
                    continue
            # 连接超时错误（curl 28）：同账号短等待重试，不切换账号
            if not emitted_for_token and is_connection_timeout_error(last_error):
                conn_timeout_retry_count += 1
                if conn_timeout_retry_count <= MAX_CONN_TIMEOUT_RETRIES:
                    wait_secs = min(3.0 * conn_timeout_retry_count, 9.0)
                    logger.warning({
                        "event": "image_stream_conn_timeout_retry",
                        "token": anonymize_token(token),
                        "account_email": account_email,
                        "retry_count": conn_timeout_retry_count,
                        "index": index,
                        "wait_secs": wait_secs,
                        "error": last_error[:200],
                    })
                    time.sleep(wait_secs)
                    continue
            if (
                for_edits
                and config.image_edit_upload_cooldown_enabled
                and not emitted_for_token
                and is_edit_upload_throttled(last_error)
            ):
                account_service.mark_edit_upload_cooldown(token, last_error)
                tried_upload_tokens.add(token)
                edit_upload_retry_count += 1
                if edit_upload_retry_count <= MAX_EDIT_UPLOAD_THROTTLE_RETRIES:
                    logger.warning({
                        "event": "image_edit_upload_throttle_retry",
                        "token": anonymize_token(token),
                        "account_email": account_email,
                        "retry_count": edit_upload_retry_count,
                        "index": index,
                        "error": last_error[:200],
                    })
                    continue
                logger.warning({
                    "event": "image_edit_upload_throttle_exhausted_retries",
                    "token": anonymize_token(token),
                    "account_email": account_email,
                    "retry_count": edit_upload_retry_count,
                    "index": index,
                })
            raise ImageGenerationError(image_stream_error_message(last_error), account_email=account_email, conversation_id="") from exc
        finally:
            # 兜底：任何没走到 finish 的路径也把名额还掉（只还一次，不记失败）
            slot.finish(None)
            if backend is not None:
                backend.close()


def stream_image_outputs_with_pool(request: ConversationRequest) -> Iterator[ImageOutput]:
    """并行生成多张图片，每张图片使用独立线程和账号，互不阻塞。"""
    if not is_supported_image_model(request.model):
        raise ImageGenerationError("unsupported image model,supported models: " + ", ".join(sorted(IMAGE_MODELS)))
    if str(request.output_format or "").lower() == "gif" and request.images:
        raise ImageGenerationError("GIF output is not supported for image edits")

    if request.n <= 1:
        # 单张图片，直接执行（无需线程池开销）
        outputs = _generate_single_image(request, 1, 1)
        for output in outputs:
            yield output
        return

    # 多张图片：根据配置选择并行或串行执行
    if not config.image_parallel_generation:
        logger.info({
            "event": "image_serial_generation_start",
            "n": request.n,
            "model": request.model,
        })
        for index in range(1, request.n + 1):
            outputs = _generate_single_image(request, index, request.n)
            for output in outputs:
                yield output
        return

    logger.info({
        "event": "image_parallel_generation_start",
        "n": request.n,
        "model": request.model,
    })
    # 每张图片一个线程，同时启动
    futures = {}
    results: dict[int, list[ImageOutput]] = {}
    errors: dict[int, Exception] = {}
    with ThreadPoolExecutor(max_workers=request.n) as executor:
        for index in range(1, request.n + 1):
            future = executor.submit(_generate_single_image, request, index, request.n)
            futures[future] = index

        # 按完成顺序收集结果
        for future in as_completed(futures):
            index = futures[future]
            try:
                results[index] = future.result()
            except Exception as exc:
                errors[index] = exc
                logger.warning({
                    "event": "image_parallel_generation_error",
                    "index": index,
                    "error": str(exc)[:300],
                })

    # yield 结果：跳过索引顺序限制，不再让低索引失败阻塞高索引成功结果
    emitted = False
    last_error = ""
    # 先 yield 所有成功的结果
    for index in range(1, request.n + 1):
        if index in results:
            for output in results[index]:
                emitted = True
                yield output
        elif index in errors:
            last_error = str(errors[index])
            if not emitted:
                logger.warning({
                    "event": "image_parallel_failure_before_success",
                    "failed_index": index,
                    "error": last_error[:200],
                })

    # 如果有失败但也有成功，记录警告
    if emitted:
        for index in range(1, request.n + 1):
            if index in errors:
                logger.warning({
                    "event": "image_parallel_partial_failure",
                    "failed_index": index,
                    "error": str(errors[index])[:200],
                })

    if not emitted:
        # 全部失败：有明确错误（如 400 input_too_large、上游未启动生图）就原样抛出，别包成 502
        for index in sorted(errors):
            if isinstance(errors[index], ImageGenerationError):
                raise errors[index]
        if not last_error:
            last_error = "no account in the pool could generate images — check account quota and rate-limit status"
        raise ImageGenerationError(image_stream_error_message(last_error), conversation_id="")


def stream_image_chunks(outputs: Iterable[ImageOutput]) -> Iterator[dict[str, Any]]:
    for output in outputs:
        yield output.to_chunk()


def collect_image_outputs(outputs: Iterable[ImageOutput]) -> dict[str, Any]:
    created = None
    data: list[dict[str, Any]] = []
    message = ""
    progress_parts: list[str] = []
    account_email = ""
    for output in outputs:
        created = created or output.created
        if output.account_email and not account_email:
            account_email = output.account_email
        if output.kind == "progress" and output.text:
            progress_parts.append(output.text)
        elif output.kind == "message":
            message = output.text
        elif output.kind == "result":
            data.extend(output.data)

    result: dict[str, Any] = {"created": created or int(time.time()), "data": data}
    if not data:
        text = message or "".join(progress_parts).strip()
        if text:
            result["message"] = text
    if account_email:
        result["_account_email"] = account_email
    return result
