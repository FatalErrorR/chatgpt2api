from __future__ import annotations

import io
import re
import os
import threading
import time
from pathlib import Path

from PIL import Image, ImageFilter

from utils.log import logger

MAX_SCALE = 4.0
METHODS = ("lanczos", "model")

MODEL_PATH = Path(__file__).parent / "models" / "realesr-general-x4v3.onnx"
MODEL_SCALE = 4
TILE = 256
TILE_PAD = 16
MODEL_TIMEOUT_SECS = 60.0

_session = None
_session_lock = threading.Lock()
# 模型推理吃满 CPU，同一时刻只跑一个；排队超时后回退 Lanczos
_infer_lock = threading.Lock()


def _get_session():
    global _session
    with _session_lock:
        if _session is None:
            import onnxruntime as ort

            options = ort.SessionOptions()
            options.intra_op_num_threads = max(1, min(4, os.cpu_count() or 1))
            _session = ort.InferenceSession(str(MODEL_PATH), options, providers=["CPUExecutionProvider"])
        return _session


def _model_upscale_rgb(image: Image.Image) -> Image.Image:
    """RealESRGAN general x4v3（ONNX，CPU），分块推理，返回 4 倍大小的 RGB 图。"""
    import numpy as np

    session = _get_session()
    started = time.monotonic()
    if not _infer_lock.acquire(timeout=MODEL_TIMEOUT_SECS):
        raise TimeoutError("upscale model busy")
    try:
        source = np.asarray(image.convert("RGB"), dtype=np.uint8)
        height, width = source.shape[:2]
        output = np.empty((height * MODEL_SCALE, width * MODEL_SCALE, 3), dtype=np.uint8)
        total_tiles = -(-height // TILE) * -(-width // TILE)
        done_tiles = 0
        for y in range(0, height, TILE):
            for x in range(0, width, TILE):
                y1, x1 = min(y + TILE, height), min(x + TILE, width)
                py0, px0 = max(y - TILE_PAD, 0), max(x - TILE_PAD, 0)
                py1, px1 = min(y1 + TILE_PAD, height), min(x1 + TILE_PAD, width)
                tile = source[py0:py1, px0:px1].astype(np.float32) / 255.0
                result = session.run(None, {"input": np.ascontiguousarray(tile.transpose(2, 0, 1)[None])})[0][0]
                result = (result.clip(0.0, 1.0) * 255.0 + 0.5).astype(np.uint8).transpose(1, 2, 0)
                oy, ox = (y - py0) * MODEL_SCALE, (x - px0) * MODEL_SCALE
                output[y * MODEL_SCALE:y1 * MODEL_SCALE, x * MODEL_SCALE:x1 * MODEL_SCALE] = result[
                    oy:oy + (y1 - y) * MODEL_SCALE, ox:ox + (x1 - x) * MODEL_SCALE
                ]
                done_tiles += 1
                elapsed = time.monotonic() - started
                # 按已完成分块估算总耗时，预计超时就立刻放弃，不白等到上限
                if elapsed / done_tiles * total_tiles > MODEL_TIMEOUT_SECS:
                    raise TimeoutError(
                        f"upscale model too slow: {elapsed / done_tiles:.1f}s/tile x {total_tiles} tiles"
                    )
        return Image.fromarray(output, "RGB")
    finally:
        _infer_lock.release()


def _resize_with_model(image: Image.Image, size: tuple[int, int]) -> Image.Image:
    has_alpha = "A" in image.getbands()
    big = _model_upscale_rgb(image).resize(size, Image.Resampling.LANCZOS)
    if not has_alpha:
        return big
    alpha = image.getchannel("A").resize(size, Image.Resampling.LANCZOS)
    big.putalpha(alpha)
    return big


def _resize_lanczos(image: Image.Image, size: tuple[int, int]) -> Image.Image:
    resized = image.resize(size, Image.Resampling.LANCZOS)
    return resized.filter(ImageFilter.UnsharpMask(radius=1.2, percent=60, threshold=2))


_SIZE_WXH = re.compile(r"(\d{2,5})\s*[x×*]\s*(\d{2,5})", re.IGNORECASE)
_SIZE_K = re.compile(r"^\s*([1-8])\s*k\s*$", re.IGNORECASE)


def requested_long_edge(size: object) -> int | None:
    """解析请求里的 size（如 1024x1024、1536x1024、2K），返回长边像素；auto/空/无法识别返回 None。"""
    text = str(size or "").strip()
    if not text:
        return None
    match = _SIZE_WXH.search(text)
    if match:
        return max(int(match.group(1)), int(match.group(2)))
    match = _SIZE_K.match(text)
    if match:
        return int(match.group(1)) * 1024
    return None


def upscale_to_edge(data: bytes, target_edge: int, method: str = "lanczos") -> bytes:
    """把长边小于 target_edge 的图等比放大到 target_edge，输出 PNG。

    只放大不缩小；长边已达标或放大倍数超过 MAX_SCALE 时原样返回。
    method="lanczos": LANCZOS + 轻度锐化。
    method="model": RealESRGAN 小模型（CPU），失败/超时/缺依赖时自动回退 lanczos。
    """
    with Image.open(io.BytesIO(data)) as image:
        width, height = image.size
        long_edge = max(width, height)
        if long_edge <= 0 or long_edge >= target_edge:
            return data
        scale = target_edge / long_edge
        if scale > MAX_SCALE:
            return data
        size = (max(1, round(width * scale)), max(1, round(height * scale)))
        return _enlarge_to_png(_normalize_mode(image), size, method, (width, height))


def _normalize_mode(image: Image.Image) -> Image.Image:
    if image.mode in {"RGB", "RGBA", "L"}:
        return image
    return image.convert("RGBA" if "A" in image.getbands() or image.mode == "P" else "RGB")


def _enlarge_to_png(image: Image.Image, size: tuple[int, int], method: str, source: tuple[int, int]) -> bytes:
    started = time.monotonic()
    result = None
    used = "lanczos"
    if method == "model":
        try:
            result = _resize_with_model(image, size)
            used = "model"
        except Exception as exc:
            logger.warning({"event": "image_upscale_model_failed", "error": repr(exc)[:300]})
    if result is None:
        result = _resize_lanczos(image, size)
    output = io.BytesIO()
    result.save(output, format="PNG")
    logger.info({
        "event": "image_upscaled",
        "method": used,
        "requested_method": method,
        "from": f"{source[0]}x{source[1]}",
        "to": f"{size[0]}x{size[1]}",
        "elapsed_secs": round(time.monotonic() - started, 2),
    })
    return output.getvalue()


# 精确适配只接受合理范围内的 宽x高，超出范围的请求仍走原来的按长边放大
MIN_EXACT_EDGE = 64
MAX_EXACT_EDGE = 4096
# 实际比例和请求比例相差不超过 2% 时不裁剪，直接缩放到目标尺寸
ASPECT_TOLERANCE = 0.02


def requested_dimensions(size: object) -> tuple[int, int] | None:
    """解析请求里明确写出的 宽x高（如 1024x1024）；auto/空/2K/超出范围返回 None。"""
    match = _SIZE_WXH.search(str(size or ""))
    if not match:
        return None
    width, height = int(match.group(1)), int(match.group(2))
    if not all(MIN_EXACT_EDGE <= value <= MAX_EXACT_EDGE for value in (width, height)):
        return None
    return width, height


def fit_to_size(data: bytes, width: int, height: int, *, allow_upscale: bool = True, method: str = "lanczos") -> bytes:
    """把图片处理成请求的 宽x高，输出 PNG。

    比例和请求不一致时先居中裁剪到请求比例；再缩放到精确尺寸（过大缩小，过小放大）。
    放大被关闭或倍数超过 MAX_SCALE 时，只做裁剪不放大。尺寸已经精确匹配时原样返回。
    """
    with Image.open(io.BytesIO(data)) as image:
        src_w, src_h = image.size
        if src_w <= 0 or src_h <= 0 or (src_w, src_h) == (width, height):
            return data
        image = _normalize_mode(image)
        wanted = width / height
        if abs(src_w / src_h / wanted - 1) > ASPECT_TOLERANCE:
            if src_w / src_h > wanted:
                new_w, new_h = max(1, round(src_h * wanted)), src_h
            else:
                new_w, new_h = src_w, max(1, round(src_w / wanted))
            left, top = (src_w - new_w) // 2, (src_h - new_h) // 2
            image = image.crop((left, top, left + new_w, top + new_h))
        cur_w, cur_h = image.size
        scale = max(width / cur_w, height / cur_h)
        if scale > 1 and allow_upscale and scale <= MAX_SCALE:
            return _enlarge_to_png(image, (width, height), method, (src_w, src_h))
        if scale > 1:
            # 不能放大：只保留裁剪出的正确比例，没裁剪就原样返回
            if (cur_w, cur_h) == (src_w, src_h):
                return data
            resized = image
        else:
            resized = image.resize((width, height), Image.Resampling.LANCZOS)
        output = io.BytesIO()
        resized.save(output, format="PNG")
    logger.info({"event": "image_fitted", "requested": f"{width}x{height}", "from": f"{src_w}x{src_h}",
                 "to": f"{resized.width}x{resized.height}"})
    return output.getvalue()
