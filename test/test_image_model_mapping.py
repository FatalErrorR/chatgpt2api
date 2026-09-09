from __future__ import annotations

import os
import unittest

os.environ.setdefault("CHATGPT2API_AUTH_KEY", "test-auth")

from utils.helper import (
    IMAGE_MODELS,
    codex_tool_model,
    is_supported_image_model,
    resolve_image_upstream_model,
)


class ImageModelMappingTests(unittest.TestCase):
    def test_web_gpt_image_2_uses_default_upstream(self) -> None:
        self.assertEqual(resolve_image_upstream_model("gpt-image-2", "gpt-5-5"), "gpt-5-5")
        self.assertEqual(resolve_image_upstream_model("gpt-image-2", "gpt-5-6"), "gpt-5-6")

    def test_web_gpt_image_2_5_slugs_are_passed_through(self) -> None:
        self.assertEqual(resolve_image_upstream_model("gpt-image-2.5", "gpt-5-5"), "gpt-image-2.5")
        self.assertEqual(
            resolve_image_upstream_model("gpt-image-2.5-flare", "gpt-5-5"),
            "gpt-image-2.5-flare",
        )
        self.assertEqual(
            resolve_image_upstream_model("gpt-image-2.5-sunburst", "gpt-5-5"),
            "gpt-image-2.5-sunburst",
        )

    def test_codex_tool_models(self) -> None:
        self.assertEqual(codex_tool_model("codex-gpt-image-2"), "gpt-image-2")
        self.assertEqual(codex_tool_model("codex-gpt-image-2.5"), "gpt-image-2.5-flare")
        self.assertEqual(codex_tool_model("codex-gpt-image-2.5-flare"), "gpt-image-2.5-flare")
        self.assertEqual(
            codex_tool_model("pro-codex-gpt-image-2.5-sunburst"),
            "gpt-image-2.5-sunburst",
        )
        self.assertEqual(codex_tool_model("gpt-image-2.5-flare"), "gpt-image-2")

    def test_new_image_models_are_supported(self) -> None:
        for model in (
            "gpt-image-2.5",
            "gpt-image-2.5-flare",
            "gpt-image-2.5-sunburst",
            "codex-gpt-image-2.5",
            "codex-gpt-image-2.5-flare",
            "team-codex-gpt-image-2.5-sunburst",
        ):
            self.assertTrue(is_supported_image_model(model), model)
            self.assertIn(model, IMAGE_MODELS)
        self.assertFalse(is_supported_image_model("dall-e-3"))
