from __future__ import annotations

import os
import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path

os.environ.setdefault("CHATGPT2API_AUTH_KEY", "test-auth")

from services.account_service import AccountService
from services.storage.json_storage import JSONStorageBackend
from utils.helper import UpstreamHTTPError, is_model_not_found
from utils.pow import _local_date_string


class ModelNotFoundTests(unittest.TestCase):
    def test_matches_only_exact_conversation_detail(self) -> None:
        hit = UpstreamHTTPError(
            "/backend-api/f/conversation",
            404,
            {"detail": "Model not found"},
        )
        self.assertTrue(is_model_not_found(hit))
        self.assertFalse(is_model_not_found(UpstreamHTTPError(
            "/backend-api/f/conversation",
            404,
            "",
        )))
        self.assertFalse(is_model_not_found(UpstreamHTTPError(
            "/backend-api/f/conversation/prepare",
            404,
            {"detail": "Model not found"},
        )))
        self.assertFalse(is_model_not_found(UpstreamHTTPError(
            "/backend-api/f/conversation",
            404,
            {"detail": "Something else"},
        )))

    def test_cooled_account_is_skipped(self) -> None:
        with tempfile.TemporaryDirectory() as tmp_dir:
            service = AccountService(JSONStorageBackend(Path(tmp_dir) / "accounts.json"))
            service.add_account_items([
                {"access_token": "cooled", "type": "plus", "status": "正常", "quota": 5},
                {"access_token": "ready", "type": "plus", "status": "正常", "quota": 5},
            ])
            service.fetch_remote_info = (
                lambda access_token, event="fetch_remote_info": service.get_account(access_token)
            )
            service.mark_model_not_found_cooldown("cooled", seconds=600)
            cooled = service.get_account("cooled") or {}
            until = datetime.fromisoformat(str(cooled.get("model_not_found_until")))
            self.assertGreater(until, datetime.now(timezone.utc) + timedelta(minutes=9))
            chosen = service.get_available_access_token()
            service.release_image_slot(chosen)
            self.assertEqual(chosen, "ready")

    def test_pow_clock_uses_china_time(self) -> None:
        text = _local_date_string()
        self.assertTrue(text.endswith("GMT+0800 (中国标准时间)"))
