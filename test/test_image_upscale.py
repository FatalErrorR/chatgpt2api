from __future__ import annotations

import base64
import io
import unittest
from unittest import mock

from PIL import Image

from services.protocol import conversation
from utils.image_upscale import upscale_to_edge


def _png(size: tuple[int, int], mode: str = "RGB") -> bytes:
    buffer = io.BytesIO()
    Image.new(mode, size, (200, 80, 40, 255) if mode == "RGBA" else (200, 80, 40)).save(buffer, format="PNG")
    return buffer.getvalue()


def _size(data: bytes) -> tuple[int, int]:
    with Image.open(io.BytesIO(data)) as image:
        return image.size


class UpscaleToEdgeTests(unittest.TestCase):
    def test_landscape_scales_long_edge_and_keeps_ratio(self):
        out = upscale_to_edge(_png((1536, 1024)), 2048)
        self.assertEqual(_size(out), (2048, 1365))

    def test_portrait_scales_long_edge(self):
        out = upscale_to_edge(_png((1086, 1448)), 2048)
        self.assertEqual(_size(out), (1536, 2048))

    def test_already_large_is_untouched(self):
        data = _png((2048, 1152))
        self.assertIs(upscale_to_edge(data, 2048), data)

    def test_excessive_scale_is_skipped(self):
        data = _png((256, 256))
        self.assertIs(upscale_to_edge(data, 2048), data)

    def test_rgba_keeps_alpha(self):
        out = upscale_to_edge(_png((1024, 768), "RGBA"), 2048)
        with Image.open(io.BytesIO(out)) as image:
            self.assertEqual(image.size, (2048, 1536))
            self.assertEqual(image.mode, "RGBA")


class ModelUpscaleTests(unittest.TestCase):
    def test_model_output_size_and_alpha(self):
        for mode in ("RGB", "RGBA"):
            out = upscale_to_edge(_png((300, 200), mode), 512, "model")
            with Image.open(io.BytesIO(out)) as image:
                self.assertEqual(image.size, (512, 341))
                self.assertEqual(image.mode, mode)

    def test_model_tiling_has_no_seam(self):
        # 尺寸跨越多个分块（TILE=256），纯色图放大后应仍是纯色
        out = upscale_to_edge(_png((600, 530)), 1200, "model")
        with Image.open(io.BytesIO(out)) as image:
            colors = image.convert("RGB").getcolors(maxcolors=1 << 24)
            dominant = max(colors)[1]
            self.assertLess(max(abs(a - b) for a, b in zip(dominant, (200, 80, 40))), 6)

    def test_model_failure_falls_back_to_lanczos(self):
        with mock.patch("utils.image_upscale._model_upscale_rgb", side_effect=RuntimeError("boom")):
            out = upscale_to_edge(_png((300, 200)), 512, "model")
        self.assertEqual(_size(out), (512, 341))

    def test_slow_model_aborts_early_and_falls_back(self):
        with mock.patch("utils.image_upscale.MODEL_TIMEOUT_SECS", 0.0):
            out = upscale_to_edge(_png((600, 530)), 1200, "model")
        self.assertEqual(_size(out), (1200, 1060))

    def test_unknown_method_uses_lanczos(self):
        with mock.patch("utils.image_upscale._model_upscale_rgb") as model:
            out = upscale_to_edge(_png((300, 200)), 512, "whatever")
        model.assert_not_called()
        self.assertEqual(_size(out), (512, 341))


class FormatImageResultTests(unittest.TestCase):
    def _run(self, enabled: bool, response_format: str):
        original = _png((1536, 1024))
        saved: list[bytes] = []
        items = [{"b64_json": base64.b64encode(original).decode("ascii")}]
        with mock.patch.object(conversation, "save_image_bytes", side_effect=lambda data, base_url=None: saved.append(data) or "u"), \
                mock.patch.object(type(conversation.config), "image_upscale_enabled", new_callable=mock.PropertyMock, return_value=enabled), \
                mock.patch.object(type(conversation.config), "image_upscale_target_edge", new_callable=mock.PropertyMock, return_value=2048):
            result = conversation.format_image_result(items, "p", response_format)
        return original, saved, result["data"][0]

    def test_disabled_keeps_original(self):
        original, saved, _ = self._run(False, "url")
        self.assertEqual(saved, [original])

    def test_enabled_saves_upscaled_url(self):
        _, saved, _ = self._run(True, "url")
        self.assertEqual(_size(saved[0]), (2048, 1365))

    def test_enabled_b64_matches_saved(self):
        _, saved, item = self._run(True, "b64_json")
        self.assertEqual(base64.b64decode(item["b64_json"]), saved[0])
        self.assertEqual(_size(saved[0]), (2048, 1365))

    def test_failure_falls_back_to_original(self):
        original = _png((1536, 1024))
        with mock.patch.object(type(conversation.config), "image_upscale_enabled", new_callable=mock.PropertyMock, return_value=True), \
                mock.patch.object(conversation, "upscale_to_edge", side_effect=RuntimeError("boom")):
            self.assertIs(conversation.maybe_upscale_image(original), original)


if __name__ == "__main__":
    unittest.main()
