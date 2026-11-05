from __future__ import annotations

import base64
import os
import unittest
from unittest import mock

os.environ.setdefault("CHATGPT2API_AUTH_KEY", "test-auth")

from services.image_storage_service import ImageStorageService
from services.protocol import conversation
from services.protocol.conversation import ImageGenerationError
from utils.helper import gif_frame_count, gif_frame_duration_ms

GIF = base64.b64decode("R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7")


class GifRequestTests(unittest.TestCase):
    def test_frame_fields_use_official_bounds(self) -> None:
        self.assertEqual(gif_frame_count(None), 9)
        self.assertEqual(gif_frame_count(1), 2)
        self.assertEqual(gif_frame_count(99), 64)
        self.assertEqual(gif_frame_duration_ms(None), 200)
        self.assertEqual(gif_frame_duration_ms(25), 20)
        self.assertEqual(gif_frame_duration_ms(5000), 2000)

    def test_web_prompt_states_gif_timing(self) -> None:
        prompt = conversation.build_image_prompt("a lantern", "1024x1024", "low", "gif", 9, 200)
        self.assertIn("输出动画 GIF，9 帧，每帧 200 毫秒。", prompt)

    def test_gif_bytes_skip_upscale(self) -> None:
        items = [{"b64_json": base64.b64encode(GIF).decode("ascii")}]
        saved: list[bytes] = []
        with mock.patch.object(conversation, "save_image_bytes", side_effect=lambda data, base_url=None: saved.append(data) or "u"), \
                mock.patch.object(conversation, "maybe_upscale_image", side_effect=AssertionError("gif was resized")):
            conversation.format_image_result(items, "p", "b64_json", size="1024x1024")
        self.assertEqual(saved, [GIF])

    def test_edits_reject_gif(self) -> None:
        request = conversation.ConversationRequest(model="gpt-image-2", prompt="cat", output_format="gif", images=["a"])
        with self.assertRaises(ImageGenerationError):
            list(conversation.stream_image_outputs_with_pool(request))

    def test_saved_name_uses_gif_suffix(self) -> None:
        self.assertTrue(ImageStorageService().make_relative_path(GIF).endswith(".gif"))
