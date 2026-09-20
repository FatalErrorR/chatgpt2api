from __future__ import annotations

import os
import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path

os.environ.setdefault("CHATGPT2API_AUTH_KEY", "test-auth")

from services.account_service import (
    AccountService,
    DEFAULT_EDIT_UPLOAD_COOLDOWN,
    MAX_EDIT_UPLOAD_COOLDOWN,
    is_edit_upload_throttled,
    parse_edit_upload_cooldown,
)
from services.config import config
from services.storage.json_storage import JSONStorageBackend


SAMPLE_429 = (
    '/backend-api/files failed: status=429, body={"detail": '
    '"已达到文件上传上限，请 3 小时内重试"}'
)


class EditUploadCooldownTests(unittest.TestCase):
    def test_parse_hours_minutes_default_and_cap(self) -> None:
        self.assertEqual(parse_edit_upload_cooldown("请 3 小时内重试"), timedelta(hours=3))
        self.assertEqual(parse_edit_upload_cooldown("请16分钟内重试"), timedelta(minutes=16))
        self.assertEqual(parse_edit_upload_cooldown(SAMPLE_429), timedelta(hours=3))
        self.assertEqual(parse_edit_upload_cooldown("请 48 小时内重试"), MAX_EDIT_UPLOAD_COOLDOWN)
        self.assertEqual(parse_edit_upload_cooldown("no retry hint"), DEFAULT_EDIT_UPLOAD_COOLDOWN)
        self.assertEqual(parse_edit_upload_cooldown("请 0 小时内重试"), DEFAULT_EDIT_UPLOAD_COOLDOWN)

    def test_detects_upload_limit_phrase(self) -> None:
        self.assertTrue(is_edit_upload_throttled(SAMPLE_429))
        self.assertTrue(is_edit_upload_throttled("You have reached the file upload limit."))
        self.assertFalse(is_edit_upload_throttled("/backend-api/f/conversation failed: status=413"))
        self.assertFalse(is_edit_upload_throttled("status=429, body=rate limited"))

    def test_edits_skip_cooled_account_generations_do_not(self) -> None:
        original = config.data.get("image_edit_upload_cooldown_enabled")
        config.data["image_edit_upload_cooldown_enabled"] = True
        try:
            with tempfile.TemporaryDirectory() as tmp_dir:
                service = AccountService(JSONStorageBackend(Path(tmp_dir) / "accounts.json"))
                until = (datetime.now(timezone.utc) + timedelta(hours=3)).isoformat()
                service.add_account_items(
                    [
                        {
                            "access_token": "token-cool",
                            "type": "free",
                            "status": "正常",
                            "quota": 11,
                            "edit_upload_blocked_until": until,
                        },
                        {"access_token": "token-ok", "type": "Plus", "status": "正常", "quota": 11},
                    ]
                )
                service.fetch_remote_info = (
                    lambda access_token, event="fetch_remote_info": service.get_account(access_token)
                )

                edits_token = service.get_available_access_token(for_edits=True)
                service.release_image_slot(edits_token)
                self.assertEqual(edits_token, "token-ok")

                gen_service = AccountService(JSONStorageBackend(Path(tmp_dir) / "accounts.json"))
                gen_service.fetch_remote_info = (
                    lambda access_token, event="fetch_remote_info": gen_service.get_account(access_token)
                )
                gen_token = gen_service.get_available_access_token(for_edits=False, excluded_tokens={"token-ok"})
                gen_service.release_image_slot(gen_token)
                self.assertEqual(gen_token, "token-cool")

                with self.assertRaises(RuntimeError):
                    service.get_available_access_token(for_edits=True, excluded_tokens={"token-ok"})
        finally:
            if original is None:
                config.data.pop("image_edit_upload_cooldown_enabled", None)
            else:
                config.data["image_edit_upload_cooldown_enabled"] = original

    def test_mark_cooldown_and_refresh_keeps_field(self) -> None:
        with tempfile.TemporaryDirectory() as tmp_dir:
            service = AccountService(JSONStorageBackend(Path(tmp_dir) / "accounts.json"))
            service.add_account_items(
                [{"access_token": "token-plus", "type": "Plus", "status": "正常", "quota": 5}]
            )
            before = datetime.now(timezone.utc)
            until = service.mark_edit_upload_cooldown("token-plus", SAMPLE_429)
            self.assertIsNotNone(until)
            parsed = datetime.fromisoformat(str(until).replace("Z", "+00:00"))
            self.assertGreaterEqual(parsed, before + timedelta(hours=2, minutes=50))
            self.assertLessEqual(parsed, before + timedelta(hours=3, minutes=10))

            service.update_account(
                "token-plus",
                {"email": "plus@example.com", "quota": 5, "status": "正常", "type": "Plus"},
            )
            account = service.get_account("token-plus") or {}
            self.assertEqual(account.get("edit_upload_blocked_until"), until)
            self.assertEqual(account.get("email"), "plus@example.com")

    def test_excluded_tokens_are_not_reused(self) -> None:
        original = config.data.get("image_edit_upload_cooldown_enabled")
        config.data["image_edit_upload_cooldown_enabled"] = True
        try:
            with tempfile.TemporaryDirectory() as tmp_dir:
                service = AccountService(JSONStorageBackend(Path(tmp_dir) / "accounts.json"))
                service.add_account_items(
                    [
                        {"access_token": "token-a", "type": "free", "status": "正常", "quota": 4},
                        {"access_token": "token-b", "type": "free", "status": "正常", "quota": 4},
                    ]
                )
                service.fetch_remote_info = (
                    lambda access_token, event="fetch_remote_info": service.get_account(access_token)
                )
                token = service.get_available_access_token(for_edits=True, excluded_tokens={"token-a"})
                service.release_image_slot(token)
                self.assertEqual(token, "token-b")
        finally:
            if original is None:
                config.data.pop("image_edit_upload_cooldown_enabled", None)
            else:
                config.data["image_edit_upload_cooldown_enabled"] = original


if __name__ == "__main__":
    unittest.main()
