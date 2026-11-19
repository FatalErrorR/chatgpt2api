from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from services import image_service, image_storage_service, image_tags_service
from services.image_storage_service import ImageStorageService


class BatchImageDeleteTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        root = Path(tmp.name)
        self.images = root / "images"
        self.thumbs = root / "image_thumbnails"
        self.index_file = root / "image_index.json"
        self.tags_file = root / "image_tags.json"
        cfg = mock.Mock(images_dir=self.images, image_thumbnails_dir=self.thumbs, base_url="http://app.test")
        cfg.get_image_storage_settings.return_value = {"mode": "local"}
        self.storage = ImageStorageService(self.index_file)
        for patcher in (
            mock.patch.object(image_service, "config", cfg),
            mock.patch.object(image_storage_service, "config", cfg),
            mock.patch.object(image_service, "image_storage_service", self.storage),
            mock.patch.object(image_tags_service, "TAGS_FILE", self.tags_file),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        self.rels = [f"2026/11/0{day}/img{n}.png" for day in (1, 2) for n in range(3)]
        for rel in self.rels:
            for base in (self.images, self.thumbs):
                (base / rel).parent.mkdir(parents=True, exist_ok=True)
            (self.images / rel).write_bytes(b"png")
            (self.thumbs / f"{rel}.png").write_bytes(b"thumb")
        self.index_file.write_text(json.dumps({"items": {rel: {"date": rel[:10]} for rel in self.rels}}))
        self.tags_file.write_text(json.dumps({self.rels[0]: ["keep"], self.rels[5]: ["x"]}))

    def test_deletes_files_thumbnails_index_and_tags_in_one_write(self):
        doomed = self.rels[:4]
        with mock.patch.object(ImageStorageService, "_save_index", autospec=True,
                               side_effect=ImageStorageService._save_index) as save_index, \
                mock.patch.object(image_tags_service, "save_tags", wraps=image_tags_service.save_tags) as save_tags:
            result = image_service.delete_images(doomed + ["../outside.png", "2026/11/09/missing.png"])
        self.assertEqual(result, {"removed": 4})
        self.assertEqual(save_index.call_count, 1)
        self.assertEqual(save_tags.call_count, 1)
        for rel in doomed:
            self.assertFalse((self.images / rel).exists())
            self.assertFalse((self.thumbs / f"{rel}.png").exists())
        for rel in self.rels[4:]:
            self.assertTrue((self.images / rel).exists())
        self.assertEqual(sorted(json.loads(self.index_file.read_text())["items"]), self.rels[4:])
        self.assertEqual(json.loads(self.tags_file.read_text()), {self.rels[5]: ["x"]})

    def test_untagged_batch_does_not_rewrite_tags(self):
        with mock.patch.object(image_tags_service, "save_tags") as save_tags:
            image_service.delete_images([self.rels[1]])
        save_tags.assert_not_called()

    def test_single_delete_still_reports_result(self):
        self.assertTrue(self.storage.delete(self.rels[2]))
        self.assertFalse(self.storage.delete(self.rels[2]))


if __name__ == "__main__":
    unittest.main()
