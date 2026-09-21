from __future__ import annotations

from io import BytesIO

from PIL import Image, ImageOps

from utils.log import logger

DEFAULT_BUDGET_BYTES = int(1.4 * 1024 * 1024)
DEFAULT_MAX_EDGE = 2048
DEFAULT_REQUEST_OVERHEAD_BYTES = 4096
DEFAULT_REQUEST_PER_IMAGE_BYTES = 256
JPEG_QUALITIES = (90, 85, 80, 75, 70, 65, 60, 50, 40)
WEBP_QUALITIES = (90, 85, 80, 75, 70, 60, 50)
DOWNSCALE_FACTOR = 0.85
MAX_SHRINK_ROUNDS = 24
MIN_JUST_FIT_BYTES = 80 * 1024


def leftover_image_budget(
    prompt: str,
    *,
    budget: int = DEFAULT_BUDGET_BYTES,
    n_images: int = 0,
) -> int:
    extra = (
        len((prompt or "").encode("utf-8"))
        + DEFAULT_REQUEST_OVERHEAD_BYTES
        + DEFAULT_REQUEST_PER_IMAGE_BYTES * max(n_images, 0)
    )
    leftover = budget - extra
    return leftover if leftover > 0 else 1


def fit_web_reference_images(
    images: list[bytes],
    prompt: str = "",
    *,
    ref_enabled: bool = True,
    ref_budget: int = DEFAULT_BUDGET_BYTES,
    req_enabled: bool = True,
    req_budget: int = DEFAULT_BUDGET_BYTES,
    max_edge: int = DEFAULT_MAX_EDGE,
) -> list[bytes]:
    """Fit the package once to the tighter of the file budget and leftover request budget."""
    if not images:
        return []
    budgets: list[int] = []
    if ref_enabled:
        budgets.append(ref_budget)
    if req_enabled:
        budgets.append(leftover_image_budget(prompt, budget=req_budget, n_images=len(images)))
    if not budgets:
        return list(images)
    return fit_reference_images(images, budget=min(budgets), max_edge=max_edge)


def fit_reference_images(
    images: list[bytes],
    *,
    budget: int = DEFAULT_BUDGET_BYTES,
    max_edge: int = DEFAULT_MAX_EDGE,
) -> list[bytes]:
    """Keep web picture_v2 reference images under a package budget.

    Under budget: return the originals unchanged. Over budget: shrink edges
    above ``max_edge``, then JPEG/WebP from high quality down so the package
    stays as large as possible while still fitting. Unreadable images are
    left as-is; any unexpected error fail-opens to the originals.
    """
    if not images:
        return []
    originals = list(images)
    original_total = sum(len(item) for item in originals)
    if original_total <= budget:
        return originals
    try:
        fitted = _fit(originals, budget=budget, max_edge=max_edge)
    except Exception as exc:
        logger.warning({"event": "image_ref_fit_failed", "error": str(exc), "count": len(originals)})
        return originals
    logger.info({
        "event": "image_ref_fit",
        "count": len(fitted),
        "orig": original_total,
        "new": sum(len(item) for item in fitted),
        "budget": budget,
    })
    return fitted


def _open_image(data: bytes) -> Image.Image | None:
    try:
        image = Image.open(BytesIO(data))
        image.load()
        return ImageOps.exif_transpose(image)
    except Exception:
        return None


def _has_alpha(image: Image.Image) -> bool:
    if image.mode in {"RGBA", "LA", "PA"}:
        return True
    return image.mode == "P" and "transparency" in image.info


def _to_rgb(image: Image.Image) -> Image.Image:
    if image.mode == "RGB":
        return image
    if _has_alpha(image):
        background = Image.new("RGB", image.size, (255, 255, 255))
        rgba = image.convert("RGBA")
        background.paste(rgba, mask=rgba.split()[-1])
        return background
    return image.convert("RGB")


def _resize_to_edge(image: Image.Image, max_edge: int) -> Image.Image:
    width, height = image.size
    longest = max(width, height)
    if longest <= max_edge:
        return image
    scale = max_edge / float(longest)
    size = (max(1, int(width * scale)), max(1, int(height * scale)))
    return image.resize(size, Image.Resampling.LANCZOS)


def _encode_jpeg(image: Image.Image, quality: int) -> bytes:
    buffer = BytesIO()
    _to_rgb(image).save(buffer, format="JPEG", quality=quality, optimize=True)
    return buffer.getvalue()


def _encode_webp(image: Image.Image, quality: int) -> bytes:
    buffer = BytesIO()
    payload = image.convert("RGBA") if _has_alpha(image) else image.convert("RGB")
    payload.save(buffer, format="WEBP", quality=quality, method=4)
    return buffer.getvalue()


def _encode_png(image: Image.Image) -> bytes:
    buffer = BytesIO()
    payload = image
    if image.mode == "P":
        payload = image.convert("RGBA") if _has_alpha(image) else image.convert("RGB")
    payload.save(buffer, format="PNG", optimize=True)
    return buffer.getvalue()


def _encode_after_resize(image: Image.Image, source_format: str) -> bytes:
    fmt = source_format.upper()
    jpeg = _encode_jpeg(image, 90)
    if fmt in {"JPEG", "JPG"}:
        return jpeg
    try:
        if _has_alpha(image):
            webp = _encode_webp(image, 90)
            png = _encode_png(image)
            return min((webp, png, jpeg), key=len)
        if fmt == "WEBP":
            return min((_encode_webp(image, 90), jpeg), key=len)
        if fmt == "PNG":
            return min((_encode_png(image), jpeg), key=len)
    except Exception:
        pass
    return jpeg


def _encode_to_fit(image: Image.Image, max_bytes: int) -> bytes | None:
    if _has_alpha(image):
        for quality in WEBP_QUALITIES:
            try:
                data = _encode_webp(image, quality)
            except Exception:
                break
            if len(data) <= max_bytes:
                return data
    for quality in JPEG_QUALITIES:
        data = _encode_jpeg(image, quality)
        if len(data) <= max_bytes:
            return data
    return None


def _encode_candidates(image: Image.Image) -> list[bytes]:
    candidates: list[bytes] = []
    if _has_alpha(image):
        for quality in WEBP_QUALITIES:
            try:
                candidates.append(_encode_webp(image, quality))
            except Exception:
                break
    candidates.extend(_encode_jpeg(image, quality) for quality in JPEG_QUALITIES)
    return candidates


def _largest_under(image: Image.Image, limit: int) -> bytes | None:
    smaller = [item for item in _encode_candidates(image) if 0 < len(item) <= limit]
    return max(smaller, key=len) if smaller else None


def _one_step_smaller(data: bytes, max_edge: int) -> bytes:
    """Reduce about 15% while keeping the largest file that still shrinks."""
    image = _open_image(data)
    if image is None:
        return data
    current = _resize_to_edge(image, max_edge)
    target = max(1, int(len(data) * DOWNSCALE_FACTOR))
    stepped = _largest_under(current, target)
    if stepped is not None:
        return stepped
    width, height = current.size
    if max(width, height) <= 256:
        fallback = _largest_under(current, len(data) - 1)
        return fallback if fallback is not None else data
    current = current.resize(
        (max(1, int(width * DOWNSCALE_FACTOR)), max(1, int(height * DOWNSCALE_FACTOR))),
        Image.Resampling.LANCZOS,
    )
    # Smaller canvas at high JPEG quality can be larger than a low-quality original.
    fallback = _largest_under(current, len(data) - 1)
    return fallback if fallback is not None else data


def _shrink_one(data: bytes, max_bytes: int, max_edge: int) -> bytes:
    image = _open_image(data)
    if image is None:
        return data
    current = _resize_to_edge(image, max_edge)
    fitted = _encode_to_fit(current, max_bytes)
    if fitted is not None:
        return fitted if len(fitted) < len(data) else data
    for _ in range(MAX_SHRINK_ROUNDS):
        width, height = current.size
        if max(width, height) <= 256:
            break
        current = current.resize(
            (max(1, int(width * DOWNSCALE_FACTOR)), max(1, int(height * DOWNSCALE_FACTOR))),
            Image.Resampling.LANCZOS,
        )
        fitted = _encode_to_fit(current, max_bytes)
        if fitted is not None:
            return fitted
    last = _encode_jpeg(current, JPEG_QUALITIES[-1])
    return last if len(last) < len(data) else data


def _fit(images: list[bytes], *, budget: int, max_edge: int) -> list[bytes]:
    result = list(images)
    opened: list[Image.Image | None] = []
    formats: list[str] = []
    for item in result:
        image = _open_image(item)
        opened.append(image)
        formats.append((image.format or "") if image is not None else "")

    for index, image in enumerate(opened):
        if image is None or max(image.size) <= max_edge:
            continue
        encoded = _encode_after_resize(_resize_to_edge(image, max_edge), formats[index])
        if len(encoded) < len(result[index]):
            result[index] = encoded
            opened[index] = _open_image(encoded)

    skip = [image is None for image in opened]
    max_rounds = max(64, len(result) * (len(JPEG_QUALITIES) + MAX_SHRINK_ROUNDS))
    for _ in range(max_rounds):
        total = sum(len(item) for item in result)
        if total <= budget:
            return result
        candidates = [index for index, ignored in enumerate(skip) if not ignored]
        if not candidates:
            break
        index = max(candidates, key=lambda item: len(result[item]))
        others = total - len(result[index])
        leftover = budget - others
        current_len = len(result[index])
        can_just_fit = (
            others <= budget
            and leftover >= min(MIN_JUST_FIT_BYTES, max(1, current_len // 2))
        )
        if can_just_fit:
            shrunk = _shrink_one(result[index], leftover, max_edge)
        else:
            # Remainder already over budget, or leftover too small to absorb this image.
            shrunk = _one_step_smaller(result[index], max_edge)
        if len(shrunk) >= len(result[index]):
            skip[index] = True
            continue
        result[index] = shrunk
        opened[index] = _open_image(shrunk)
    return result
