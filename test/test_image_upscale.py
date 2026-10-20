from __future__ import annotations

import base64
import io
import unittest
from unittest import mock


from PIL import Image

from services.protocol import conversation
from utils.image_upscale import fit_to_size, requested_dimensions, requested_long_edge, upscale_to_edge


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


class RequestedSizeTests(unittest.TestCase):
    """用户请求的尺寸优先：要 1024 就不能被放大到设置里的 2048。"""

    def test_requested_long_edge_parsing(self):
        self.assertEqual(requested_long_edge("1024x1024"), 1024)
        self.assertEqual(requested_long_edge("1536x1024"), 1536)
        self.assertEqual(requested_long_edge("1024X1536"), 1536)
        self.assertEqual(requested_long_edge("2048 × 1152"), 2048)
        self.assertEqual(requested_long_edge("2K"), 2048)
        self.assertEqual(requested_long_edge("4k"), 4096)
        for value in (None, "", "auto", "square", "16:9"):
            self.assertIsNone(requested_long_edge(value), value)

    def _upscale(self, size, source=(1024, 1024), target=2048):
        data = _png(source)
        with mock.patch.object(type(conversation.config), "image_upscale_enabled", new_callable=mock.PropertyMock, return_value=True), \
                mock.patch.object(type(conversation.config), "image_upscale_target_edge", new_callable=mock.PropertyMock, return_value=target), \
                mock.patch.object(type(conversation.config), "image_upscale_method", new_callable=mock.PropertyMock, return_value="lanczos"):
            return data, conversation.maybe_upscale_image(data, size)

    def test_1024_request_is_not_upscaled_to_2048(self):
        data, out = self._upscale("1024x1024")
        self.assertIs(out, data)

    def test_smaller_output_is_only_raised_to_the_requested_edge(self):
        _, out = self._upscale("1536x1024", source=(1024, 683))
        self.assertEqual(_size(out)[0], 1536)

    def test_larger_k_request_is_capped_by_configured_target(self):
        _, out = self._upscale("4K", source=(1024, 1024))
        self.assertEqual(_size(out), (2048, 2048))

    def test_auto_or_missing_size_uses_configured_target(self):
        for size in (None, "auto", ""):
            _, out = self._upscale(size)
            self.assertEqual(_size(out), (2048, 2048), size)

    def test_format_image_result_passes_size_through(self):
        items = [{"b64_json": base64.b64encode(_png((1024, 1024))).decode("ascii")}]
        with mock.patch.object(type(conversation.config), "image_upscale_enabled", new_callable=mock.PropertyMock, return_value=True), \
                mock.patch.object(type(conversation.config), "image_upscale_target_edge", new_callable=mock.PropertyMock, return_value=2048):
            small = conversation.format_image_result(items, "p", "b64_json", size="1024x1024")["data"][0]["b64_json"]
            big = conversation.format_image_result(items, "p", "b64_json")["data"][0]["b64_json"]
        self.assertEqual(_size(base64.b64decode(small)), (1024, 1024))
        self.assertEqual(_size(base64.b64decode(big)), (2048, 2048))


class FitToSizeTests(unittest.TestCase):
    """明确写了 宽x高 时，返回的图必须是这个尺寸和比例。"""

    def test_requested_dimensions_parsing(self):
        self.assertEqual(requested_dimensions("1024x1024"), (1024, 1024))
        self.assertEqual(requested_dimensions("1536 × 1024"), (1536, 1024))
        for value in (None, "", "auto", "2K", "16:9", "8000x8000", "10x10"):
            self.assertIsNone(requested_dimensions(value), value)

    def test_same_ratio_larger_output_is_scaled_down(self):
        self.assertEqual(_size(fit_to_size(_png((1254, 1254)), 1024, 1024)), (1024, 1024))

    def test_exact_size_is_untouched(self):
        data = _png((1024, 1024))
        self.assertIs(fit_to_size(data, 1024, 1024), data)

    def test_smaller_output_is_scaled_up(self):
        self.assertEqual(_size(fit_to_size(_png((1254, 1254)), 2048, 2048)), (2048, 2048))

    def test_different_ratio_is_center_cropped_to_requested_ratio(self):
        out = fit_to_size(_png((1086, 1448)), 1024, 1024)
        self.assertEqual(_size(out), (1024, 1024))
        out = fit_to_size(_png((1448, 1086)), 1024, 1536)
        self.assertEqual(_size(out), (1024, 1536))

    def test_crop_keeps_the_center(self):
        image = Image.new("RGB", (100, 200), (255, 0, 0))
        image.paste((0, 255, 0), (0, 75, 100, 125))
        buffer = io.BytesIO()
        image.save(buffer, format="PNG")
        with Image.open(io.BytesIO(fit_to_size(buffer.getvalue(), 100, 100))) as out:
            self.assertEqual(out.getpixel((50, 50)), (0, 255, 0))
            self.assertEqual(out.getpixel((50, 2)), (255, 0, 0))

    def test_rgba_keeps_alpha(self):
        with Image.open(io.BytesIO(fit_to_size(_png((1254, 1254), "RGBA"), 1024, 1024))) as out:
            self.assertEqual((out.size, out.mode), ((1024, 1024), "RGBA"))

    def test_upscale_disabled_only_crops(self):
        self.assertEqual(_size(fit_to_size(_png((1254, 1254)), 2048, 2048, allow_upscale=False)), (1254, 1254))
        data = _png((1254, 1254))
        self.assertIs(fit_to_size(data, 2048, 2048, allow_upscale=False), data)
        self.assertEqual(_size(fit_to_size(_png((1086, 1448)), 2048, 2048, allow_upscale=False)), (1086, 1086))

    def test_excessive_upscale_is_skipped(self):
        data = _png((256, 256))
        self.assertIs(fit_to_size(data, 2048, 2048), data)

    def _run(self, size, source, enabled=True, exact=True):
        data = _png(source)
        with mock.patch.object(type(conversation.config), "image_exact_size_enabled", new_callable=mock.PropertyMock, return_value=exact), \
                mock.patch.object(type(conversation.config), "image_upscale_enabled", new_callable=mock.PropertyMock, return_value=enabled), \
                mock.patch.object(type(conversation.config), "image_upscale_target_edge", new_callable=mock.PropertyMock, return_value=2048), \
                mock.patch.object(type(conversation.config), "image_upscale_method", new_callable=mock.PropertyMock, return_value="lanczos"):
            return data, conversation.maybe_upscale_image(data, size)

    def test_pipeline_1024_request_gets_1024_even_when_upscale_disabled(self):
        for enabled in (True, False):
            _, out = self._run("1024x1024", (1254, 1254), enabled)
            self.assertEqual(_size(out), (1024, 1024), enabled)

    def test_pipeline_explicit_size_beyond_configured_target_is_honored(self):
        _, out = self._run("3072x3072", (1254, 1254))
        self.assertEqual(_size(out), (3072, 3072))

    def test_pipeline_auto_keeps_old_behaviour(self):
        _, out = self._run("auto", (1254, 1254))
        self.assertEqual(_size(out), (2048, 2048))
        data, out = self._run("auto", (1254, 1254), enabled=False)
        self.assertIs(out, data)

    def test_pipeline_switch_off_keeps_old_behaviour(self):
        data, out = self._run("1024x1024", (1254, 1254), enabled=False, exact=False)
        self.assertIs(out, data)
        _, out = self._run("1024x1024", (1254, 1254), enabled=True, exact=False)
        self.assertEqual(_size(out), (1254, 1254))

    def test_pipeline_failure_returns_original(self):
        data = _png((1254, 1254))
        with mock.patch.object(type(conversation.config), "image_exact_size_enabled", new_callable=mock.PropertyMock, return_value=True), \
                mock.patch.object(conversation, "fit_to_size", side_effect=RuntimeError("boom")):
            self.assertIs(conversation.maybe_upscale_image(data, "1024x1024"), data)

    def test_config_defaults_off_and_is_exposed(self):
        self.assertFalse(conversation.config.image_exact_size_enabled)
        self.assertIn("image_exact_size_enabled", conversation.config.get())


if __name__ == "__main__":
    unittest.main()
