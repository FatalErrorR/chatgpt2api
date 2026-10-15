from __future__ import annotations

import os
import unittest
from datetime import datetime
from zoneinfo import ZoneInfo

os.environ.setdefault("CHATGPT2API_AUTH_KEY", "test-auth")

from services.openai_backend_api import image_client_timezone


class ImageClientTimezoneTests(unittest.TestCase):
    def test_matches_los_angeles_daylight_offset(self) -> None:
        moment = datetime(2026, 10, 15, 12, tzinfo=ZoneInfo("UTC"))
        name, offset = image_client_timezone(moment)
        self.assertEqual(name, "America/Los_Angeles")
        self.assertEqual(offset, 420)

    def test_matches_los_angeles_standard_offset(self) -> None:
        moment = datetime(2026, 12, 15, 12, tzinfo=ZoneInfo("UTC"))
        _, offset = image_client_timezone(moment)
        self.assertEqual(offset, 480)
