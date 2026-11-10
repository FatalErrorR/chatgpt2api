import io
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from PIL import Image

import services.image_service as image_service
from services.image_storage_service import ImageStorageService


def png(seed: int) -> bytes:
    buffer = io.BytesIO()
    Image.new("RGB", (4, 4), (seed, 0, 0)).save(buffer, format="PNG")
    return buffer.getvalue()


class ImageListPaginationTests(unittest.TestCase):
    """图片列表分页：只返回当前页，标签在后端筛选；平时读索引，按间隔或 refresh 才和磁盘对账。"""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        data_dir = Path(self.tmp.name)
        self.images_dir = data_dir / "images"
        config_patcher = mock.patch("services.image_storage_service.config")
        mock_config = config_patcher.start()
        self.addCleanup(config_patcher.stop)
        mock_config.images_dir = self.images_dir
        mock_config.base_url = "http://app.test"
        mock_config.cleanup_old_images.return_value = 0
        mock_config.get_image_storage_settings.return_value = {"enabled": False, "mode": "local", "public_base_url": ""}
        self.storage = ImageStorageService(data_dir / "image_index.json")
        self.tags: dict[str, list[str]] = {}
        for target, value in (("image_storage_service", self.storage), ("load_tags", lambda: self.tags)):
            patcher = mock.patch.object(image_service, target, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        self.paths = []
        for seed in range(5):
            with (
                mock.patch("services.image_storage_service.time.time", return_value=1_800_000_000 + seed),
                mock.patch("services.image_storage_service._now_iso", return_value=f"2026-11-10 00:00:0{seed}"),
            ):
                self.paths.append(self.storage.save(png(seed), "http://app.test").rel)

    def test_page_returns_only_that_page_newest_first(self):
        data = image_service.list_images("http://app.test", page=2, page_size=2)
        self.assertEqual((data["total"], data["page"], data["page_size"], data["page_count"]), (5, 2, 2, 3))
        self.assertEqual([item["path"] for item in data["items"]], [self.paths[2], self.paths[1]])

    def test_page_past_the_end_is_clamped_to_the_last_page(self):
        data = image_service.list_images("http://app.test", page=9, page_size=2)
        self.assertEqual(data["page"], 3)
        self.assertEqual([item["path"] for item in data["items"]], [self.paths[0]])

    def test_page_size_is_capped(self):
        data = image_service.list_images("http://app.test", page=1, page_size=10_000)
        self.assertEqual(data["page_size"], image_service.IMAGE_PAGE_SIZE_MAX)

    def test_tags_filter_requires_every_tag(self):
        self.tags = {self.paths[0]: ["猫", "夜景"], self.paths[1]: ["猫"]}
        data = image_service.list_images("http://app.test", tags=["猫", "夜景"], page=1, page_size=10)
        self.assertEqual(data["total"], 1)
        self.assertEqual(data["items"][0]["tags"], ["猫", "夜景"])

    def test_without_page_returns_everything_like_before(self):
        data = image_service.list_images("http://app.test")
        self.assertEqual(len(data["items"]), 5)
        self.assertNotIn("page", data)

    def test_paths_lists_every_match_across_pages(self):
        self.tags = {self.paths[0]: ["猫"], self.paths[3]: ["猫"]}
        self.assertEqual(image_service.list_image_paths(tags=["猫"])["paths"], [self.paths[3], self.paths[0]])

    def test_disk_changes_show_up_on_refresh_not_on_every_request(self):
        image_service.list_images("http://app.test")  # 首次对账
        (self.images_dir / self.paths[0]).unlink()
        stray = self.images_dir / "2026" / "01" / "01" / "stray.png"
        stray.parent.mkdir(parents=True)
        stray.write_bytes(png(9))
        cached = {item["path"] for item in image_service.list_images("http://app.test")["items"]}
        self.assertIn(self.paths[0], cached)
        self.assertNotIn("2026/01/01/stray.png", cached)
        fresh = {item["path"] for item in image_service.list_images("http://app.test", refresh=True)["items"]}
        self.assertNotIn(self.paths[0], fresh)
        self.assertIn("2026/01/01/stray.png", fresh)


if __name__ == "__main__":
    unittest.main()
