from __future__ import annotations

import os
import unittest
from io import BytesIO
from pathlib import Path

os.environ.setdefault("CHATGPT2API_AUTH_KEY", "test-auth")

from PIL import Image, ImageDraw

from utils.image_fit import (
    DEFAULT_BUDGET_BYTES,
    DEFAULT_REQUEST_OVERHEAD_BYTES,
    DEFAULT_REQUEST_PER_IMAGE_BYTES,
    fit_reference_images,
    fit_web_reference_images,
    leftover_image_budget,
)

FLARE10_REF_DIR = Path(__file__).resolve().parents[1] / "tmp-401-out" / "flare10" / "refs"


def _png_bytes(width: int, height: int, color: tuple[int, int, int] = (30, 80, 120)) -> bytes:
    image = Image.new("RGB", (width, height), color)
    buffer = BytesIO()
    image.save(buffer, format="PNG")
    return buffer.getvalue()


def _noise_jpeg(width: int, height: int, quality: int = 95) -> bytes:
    image = Image.frombytes("RGB", (width, height), os.urandom(width * height * 3))
    buffer = BytesIO()
    image.save(buffer, format="JPEG", quality=quality)
    return buffer.getvalue()


def _striped_jpeg(width: int, height: int, quality: int = 95) -> bytes:
    image = Image.new("RGB", (width, height), (20, 40, 80))
    draw = ImageDraw.Draw(image)
    for y in range(0, height, 2):
        draw.line((0, y, width, y), fill=(y % 256, (y * 3) % 256, 40))
    buffer = BytesIO()
    image.save(buffer, format="JPEG", quality=quality)
    return buffer.getvalue()


class ImageFitTests(unittest.TestCase):
    def test_under_budget_keeps_original_bytes(self) -> None:
        small = _png_bytes(32, 32)
        oversized_but_tiny = _png_bytes(3000, 40, (10, 20, 30))
        self.assertLess(len(small) + len(oversized_but_tiny), DEFAULT_BUDGET_BYTES)
        fitted = fit_reference_images([small, oversized_but_tiny])
        self.assertEqual(fitted[0], small)
        self.assertEqual(fitted[1], oversized_but_tiny)

    def test_over_budget_single_image_stays_near_budget(self) -> None:
        large = _noise_jpeg(2048, 2048, quality=95)
        self.assertGreater(len(large), DEFAULT_BUDGET_BYTES)
        fitted = fit_reference_images([large])
        self.assertEqual(len(fitted), 1)
        self.assertLessEqual(len(fitted[0]), DEFAULT_BUDGET_BYTES)
        self.assertGreater(len(fitted[0]), 400_000)
        self.assertNotEqual(fitted[0], large)

    def test_over_budget_only_shrinks_large_images(self) -> None:
        small = _png_bytes(16, 16)
        large = _noise_jpeg(1800, 1800, quality=95)
        self.assertGreater(len(small) + len(large), DEFAULT_BUDGET_BYTES)
        fitted = fit_reference_images([small, large])
        self.assertEqual(fitted[0], small)
        self.assertLessEqual(sum(len(item) for item in fitted), DEFAULT_BUDGET_BYTES)
        self.assertLess(len(fitted[1]), len(large))

    def test_corrupt_image_fail_open(self) -> None:
        small = _png_bytes(24, 24)
        corrupt = b"not-an-image"
        large = _striped_jpeg(1600, 1600, quality=95)
        fitted = fit_reference_images([small, corrupt, large])
        self.assertEqual(fitted[0], small)
        self.assertEqual(fitted[1], corrupt)
        self.assertEqual(len(fitted), 3)

    def test_empty_list(self) -> None:
        self.assertEqual(fit_reference_images([]), [])

    def test_leftover_image_budget_subtracts_prompt(self) -> None:
        prompt = "分镜" * 4000
        leftover = leftover_image_budget(prompt, n_images=10)
        extra = len(prompt.encode("utf-8")) + DEFAULT_REQUEST_OVERHEAD_BYTES + DEFAULT_REQUEST_PER_IMAGE_BYTES * 10
        self.assertEqual(leftover, DEFAULT_BUDGET_BYTES - extra)
        self.assertLess(leftover, DEFAULT_BUDGET_BYTES)
        self.assertEqual(leftover_image_budget("x" * 3_000_000), 1)

    def test_images_under_file_budget_still_shrink_for_long_prompt(self) -> None:
        images = [_striped_jpeg(900, 900, quality=90) for _ in range(3)]
        total = sum(len(item) for item in images)
        self.assertLess(total, DEFAULT_BUDGET_BYTES)
        self.assertEqual(sum(len(item) for item in fit_reference_images(images)), total)
        leftover = leftover_image_budget("hello", budget=total - 10_000, n_images=len(images))
        self.assertLess(leftover, total)
        fitted = fit_reference_images(images, budget=leftover)
        self.assertLessEqual(sum(len(item) for item in fitted), leftover)
        self.assertLess(sum(len(item) for item in fitted), total)

    def test_web_fit_uses_tighter_request_budget_in_one_pass(self) -> None:
        images = [_noise_jpeg(1600, 1200, quality=95) for _ in range(3)]
        file_only = fit_web_reference_images(images, "", ref_enabled=True, req_enabled=False)
        file_total = sum(len(item) for item in file_only)
        self.assertLessEqual(file_total, DEFAULT_BUDGET_BYTES)
        tighter = fit_web_reference_images(
            images,
            "hello",
            ref_enabled=True,
            req_enabled=True,
            req_budget=file_total - 20_000,
        )
        leftover = leftover_image_budget("hello", budget=file_total - 20_000, n_images=len(images))
        self.assertLess(sum(len(item) for item in tighter), file_total)
        self.assertLessEqual(sum(len(item) for item in tighter), leftover)

    def test_web_fit_skips_when_already_under_target(self) -> None:
        small = [_png_bytes(32, 32)]
        self.assertEqual(
            fit_web_reference_images(small, "hi", ref_enabled=True, req_enabled=True),
            small,
        )

    def test_over_budget_keeps_aspect_ratio(self) -> None:
        large = _noise_jpeg(3000, 2000, quality=95)
        fitted = fit_reference_images([large])
        original = Image.open(BytesIO(large))
        output = Image.open(BytesIO(fitted[0]))
        self.assertAlmostEqual(
            original.size[0] / original.size[1],
            output.size[0] / output.size[1],
            places=2,
        )
        self.assertLessEqual(max(output.size), 2048)

    def test_transparent_png_under_budget_unchanged(self) -> None:
        image = Image.new("RGBA", (64, 48), (255, 0, 0, 80))
        buffer = BytesIO()
        image.save(buffer, format="PNG")
        data = buffer.getvalue()
        self.assertEqual(fit_reference_images([data])[0], data)

    def test_multi_large_images_do_not_crush_to_thumbnails(self) -> None:
        first = _noise_jpeg(1600, 1200, quality=95)
        second = _noise_jpeg(1500, 1500, quality=95)
        third = _png_bytes(32, 32)
        self.assertGreater(len(first) + len(second) + len(third), DEFAULT_BUDGET_BYTES)
        fitted = fit_reference_images([first, second, third])
        self.assertEqual(fitted[2], third)
        self.assertLessEqual(sum(len(item) for item in fitted), DEFAULT_BUDGET_BYTES)
        for index, original in enumerate((first, second)):
            output = Image.open(BytesIO(fitted[index]))
            self.assertGreaterEqual(min(output.size), 256)
            self.assertGreater(len(fitted[index]), 40_000)


class Conversation413RetryTests(unittest.TestCase):
    def test_is_conversation_413_ignores_prepare_and_files(self) -> None:
        from utils.helper import UpstreamHTTPError, is_conversation_413

        conv = UpstreamHTTPError("/backend-api/f/conversation", 413, "")
        prepare = UpstreamHTTPError("/backend-api/f/conversation/prepare", 413, "")
        files = UpstreamHTTPError("/backend-api/files", 413, "")
        self.assertTrue(is_conversation_413(conv))
        self.assertTrue(is_conversation_413("/backend-api/f/conversation failed: status=413, body="))
        self.assertFalse(is_conversation_413(prepare))
        self.assertFalse(is_conversation_413(files))
        self.assertFalse(is_conversation_413(UpstreamHTTPError("/backend-api/f/conversation", 429, "")))

    def test_stream_does_not_reupload_on_conversation_413(self) -> None:
        import base64

        from services.openai_backend_api import OpenAIBackendAPI
        from utils.helper import UpstreamHTTPError

        jpeg = _noise_jpeg(1600, 1600, quality=95)
        payload = base64.b64encode(jpeg).decode("ascii")
        backend = object.__new__(OpenAIBackendAPI)
        backend.access_token = "token"
        backend.progress_callback = None
        backend.uploads = []
        backend.starts = 0

        def upload(image: str, file_name: str = "image.png") -> dict:
            data = backend._decode_image_base64(image)
            backend.uploads.append(len(data))
            return {
                "file_id": f"file_{len(backend.uploads)}",
                "file_name": file_name,
                "file_size": len(data),
                "mime_type": "image/jpeg",
                "width": 10,
                "height": 10,
            }

        def start(*_args, **_kwargs):
            backend.starts += 1
            raise UpstreamHTTPError("/backend-api/f/conversation", 413, "")

        backend._upload_image = upload  # type: ignore[method-assign]
        backend._bootstrap = lambda: None  # type: ignore[method-assign]
        backend._get_chat_requirements = lambda: object()  # type: ignore[method-assign]
        backend._prepare_image_conversation = lambda *args, **kwargs: "conduit"  # type: ignore[method-assign]
        backend._start_image_generation = start  # type: ignore[method-assign]
        backend._iter_sse_payloads_capped = lambda response, _timeout: iter((response,))  # type: ignore[method-assign]
        backend._report_progress = lambda _step: None  # type: ignore[method-assign]

        with self.assertRaises(UpstreamHTTPError) as raised:
            list(backend._stream_picture_conversation("prompt", "gpt-image-2.5-sunburst", [payload]))
        self.assertEqual(raised.exception.status_code, 413)
        self.assertEqual(backend.starts, 1)
        self.assertEqual(len(backend.uploads), 1)


@unittest.skipUnless(FLARE10_REF_DIR.is_dir(), "flare10 refs are not present")
class ImageFitFlare10Tests(unittest.TestCase):
    def _load_refs(self) -> list[tuple[str, bytes]]:
        items = []
        for path in sorted(FLARE10_REF_DIR.iterdir()):
            if path.suffix.lower() not in {".jpg", ".jpeg", ".png"}:
                continue
            items.append((path.name, path.read_bytes()))
        return items

    def test_singles_only_touch_over_budget_images(self) -> None:
        for name, data in self._load_refs():
            fitted = fit_reference_images([data])[0]
            if len(data) <= DEFAULT_BUDGET_BYTES:
                self.assertEqual(fitted, data, name)
                continue
            output = Image.open(BytesIO(fitted))
            original = Image.open(BytesIO(data))
            self.assertLessEqual(len(fitted), DEFAULT_BUDGET_BYTES, name)
            self.assertGreater(len(fitted), 200_000, name)
            self.assertEqual(output.size, original.size, name)

    def test_all_ten_stay_under_budget_without_thumbnailing(self) -> None:
        items = self._load_refs()
        originals = [data for _, data in items]
        fitted = fit_reference_images(originals)
        self.assertLessEqual(sum(len(item) for item in fitted), DEFAULT_BUDGET_BYTES)
        for (name, data), out in zip(items, fitted, strict=True):
            original = Image.open(BytesIO(data))
            output = Image.open(BytesIO(out))
            if min(original.size) >= 800:
                self.assertGreaterEqual(min(output.size), 400, name)
            if len(data) >= 200_000:
                self.assertGreater(len(out), 50_000, name)
            self.assertAlmostEqual(
                original.size[0] / original.size[1],
                output.size[0] / output.size[1],
                places=2,
                msg=name,
            )
            if len(data) < 200_000:
                self.assertEqual(out, data, name)


if __name__ == "__main__":
    unittest.main()
