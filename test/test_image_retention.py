from __future__ import annotations

import os
import tempfile
import time
import unittest
from datetime import date
from pathlib import Path
from unittest import mock

from services.config import config


class ImageRetentionByDayTests(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.images = Path(tmp.name) / "images"
        self.images.mkdir()
        for patcher in (
            mock.patch.object(type(config), "images_dir", new_callable=mock.PropertyMock, return_value=self.images),
            mock.patch.object(type(config), "image_retention_days", new_callable=mock.PropertyMock, return_value=3),
        ):
            patcher.start()
            self.addCleanup(patcher.stop)
        self.today = date(2026, 11, 20)

    def _image(self, rel: str, mtime_day: str | None = None) -> Path:
        path = self.images / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(b"png")
        if mtime_day:
            stamp = time.mktime(time.strptime(mtime_day + " 12:00", "%Y-%m-%d %H:%M"))
            os.utime(path, (stamp, stamp))
        return path

    def test_keeps_the_last_n_calendar_days_including_today(self):
        self.assertEqual(config.image_retention_keep_from(self.today), "2026-11-18")
        kept = [self._image("2026/11/18/a.png"), self._image("2026/11/20/b.png")]
        # 目录日期优先于修改时间：11-17 目录里的图哪怕刚被改过也照样清理
        doomed = [self._image("2026/11/17/c.png", mtime_day="2026-11-20"), self._image("2026/10/31/d.png")]
        self.assertEqual(config.cleanup_old_images(self.today), 2)
        self.assertTrue(all(p.exists() for p in kept))
        self.assertFalse(any(p.exists() for p in doomed))
        self.assertFalse((self.images / "2026/10").exists())

    def test_images_outside_date_folders_use_their_modified_day(self):
        old = self._image("legacy/old.png", mtime_day="2026-11-01")
        recent = self._image("legacy/new.png", mtime_day="2026-11-19")
        self.assertEqual(config.cleanup_old_images(self.today), 1)
        self.assertFalse(old.exists())
        self.assertTrue(recent.exists())

    def test_one_day_keeps_only_today(self):
        with mock.patch.object(type(config), "image_retention_days", new_callable=mock.PropertyMock, return_value=1):
            self.assertEqual(config.image_retention_keep_from(self.today), "2026-11-20")


if __name__ == "__main__":
    unittest.main()
