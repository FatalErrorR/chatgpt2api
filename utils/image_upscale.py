from __future__ import annotations

import io
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
        if image.mode not in {"RGB", "RGBA", "L"}:
            image = image.convert("RGBA" if "A" in image.getbands() or image.mode == "P" else "RGB")
        size = (max(1, round(width * scale)), max(1, round(height * scale)))
        result = None
        if method == "model":
            try:
                result = _resize_with_model(image, size)
            except Exception as exc:
                logger.warning({"event": "image_upscale_model_failed", "error": repr(exc)[:300]})
        if result is None:
            result = _resize_lanczos(image, size)
    output = io.BytesIO()
    result.save(output, format="PNG")
    return output.getvalue()
