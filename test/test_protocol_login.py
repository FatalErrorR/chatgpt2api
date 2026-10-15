from __future__ import annotations

import os
import tempfile
import unittest
from pathlib import Path
from unittest import mock

os.environ.setdefault("CHATGPT2API_AUTH_KEY", "test-auth")

from services import protocol_login_service as pls
from services.account_service import AccountService
from services.storage.json_storage import JSONStorageBackend
from utils.totp import normalize_totp_secret, seconds_left_in_period, totp_code

# RFC 6238 附录 B 的 SHA-1 测试密钥 "12345678901234567890"
RFC_SECRET = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"


class TotpTests(unittest.TestCase):
    def test_rfc6238_vectors(self) -> None:
        self.assertEqual(totp_code(RFC_SECRET, now=59, digits=8), "94287082")
        self.assertEqual(totp_code(RFC_SECRET, now=1111111109, digits=8), "07081804")
        self.assertEqual(totp_code(RFC_SECRET, now=59), "287082")

    def test_secret_is_normalized(self) -> None:
        self.assertEqual(normalize_totp_secret("gezd gnbv-gy3t qojq"), "GEZDGNBVGY3TQOJQ")
        self.assertEqual(normalize_totp_secret("not base32 !!"), "")
        self.assertEqual(normalize_totp_secret(""), "")
        with self.assertRaises(ValueError):
            totp_code("!!!")

    def test_seconds_left(self) -> None:
        self.assertEqual(seconds_left_in_period(now=60), 30)
        self.assertEqual(seconds_left_in_period(now=89), 1)


class ParseLinesTests(unittest.TestCase):
    def test_three_fields(self) -> None:
        entries, invalid = pls.parse_account_lines(f"a@example.com----@pw1----{RFC_SECRET}\n")
        self.assertEqual(invalid, [])
        self.assertEqual(entries, [{"line": 1, "email": "a@example.com", "password": "@pw1", "totp_secret": RFC_SECRET}])

    def test_without_totp_and_password_with_separator(self) -> None:
        entries, _ = pls.parse_account_lines("a@example.com----pw\nb@example.com----p----w----GEZDGNBV")
        self.assertEqual(entries[0]["totp_secret"], "")
        self.assertEqual(entries[1]["password"], "p----w")
        self.assertEqual(entries[1]["totp_secret"], "GEZDGNBV")

    def test_invalid_lines_are_reported_with_line_numbers(self) -> None:
        text = "\n".join([
            "# comment",
            "",
            "no-separator",
            "not-an-email----pw",
            "a@example.com----",
            "c@example.com----pw----!!bad!!",
            "d@example.com----pw",
            "D@example.com----pw2",
        ])
        entries, invalid = pls.parse_account_lines(text)
        self.assertEqual([e["email"] for e in entries], ["d@example.com"])
        self.assertEqual([item["line"] for item in invalid], [3, 4, 5, 6, 8])
        self.assertEqual(invalid[-1]["error"], "邮箱重复")


class ErrorTextTests(unittest.TestCase):
    def test_known_and_unknown_errors(self) -> None:
        self.assertEqual(pls.describe_login_error({"error": "invalid_password"}), "密码错误")
        self.assertEqual(pls.describe_login_error({"error": "mfa_verify_failed_400"}), "二步验证码被拒绝（400）")
        self.assertEqual(
            pls.describe_login_error({"error": "password_verify_failed_403", "detail": {"error": {"code": "account_deactivated"}}}),
            "账号已被停用",
        )
        self.assertEqual(pls.describe_login_error({"error": "weird"}), "weird")


class ServiceTests(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.accounts = AccountService(JSONStorageBackend(Path(self.tmp.name) / "accounts.json"))
        patcher = mock.patch.object(pls, "account_service", self.accounts)
        patcher.start()
        self.addCleanup(patcher.stop)
        # 额度刷新要联网，这里不测
        refresh = mock.patch.object(self.accounts, "refresh_accounts", return_value={})
        self.refresh = refresh.start()
        self.addCleanup(refresh.stop)

    @staticmethod
    def _ok(token: str, **extra):
        return {"ok": True, "access_token": token, "refresh_token": "rt-" + token, "id_token": "id", "email": "", **extra}

    def test_success_adds_account_with_credentials(self) -> None:
        calls = []

        def login(email, password, totp):
            calls.append((email, password, totp))
            return self._ok("tok-1", account_id="acc-1")

        service = pls.ProtocolLoginService(login)
        started = service.start(f"a@example.com----pw----{RFC_SECRET}", run_async=False)
        progress = service.get_progress(started["progress_id"])
        self.assertEqual(calls, [("a@example.com", "pw", RFC_SECRET)])
        self.assertEqual((progress["status"], progress["success"], progress["failed"]), ("done", 1, 0))
        saved = self.accounts.get_account("tok-1")
        self.assertEqual(saved["email"], "a@example.com")
        self.assertEqual(saved["password"], "pw")
        self.assertEqual(saved["totp_secret"], RFC_SECRET)
        self.assertEqual(saved["status"], "正常")
        self.assertEqual(saved["source_type"], "password")
        self.refresh.assert_called_once_with(["tok-1"])
        # 进度里不能带密码/密钥
        self.assertNotIn("pw", str(progress["items"]))
        self.assertNotIn(RFC_SECRET, str(progress))

    def test_failure_and_exception_do_not_stop_the_batch(self) -> None:
        def login(email, password, totp):
            if email.startswith("bad"):
                return {"ok": False, "error": "invalid_password"}
            if email.startswith("boom"):
                raise RuntimeError("network down")
            return self._ok("tok-" + email)

        service = pls.ProtocolLoginService(login)
        started = service.start("bad@example.com----x\nboom@example.com----x\ngood@example.com----x", run_async=False)
        progress = service.get_progress(started["progress_id"])
        statuses = {item["email"]: (item["status"], item["error"]) for item in progress["items"]}
        self.assertEqual(statuses["bad@example.com"], ("失败", "密码错误"))
        self.assertEqual(statuses["boom@example.com"][0], "失败")
        self.assertIn("network down", statuses["boom@example.com"][1])
        self.assertEqual(statuses["good@example.com"][0], "成功")
        self.assertEqual((progress["success"], progress["failed"], progress["completed"]), (1, 2, 3))
        self.assertIsNone(self.accounts.get_account("tok-bad@example.com"))

    def test_existing_email_is_updated_in_place(self) -> None:
        self.accounts.add_account_items([{"access_token": "old-token", "email": "a@example.com", "status": "异常", "password": "old"}])
        service = pls.ProtocolLoginService(lambda *_: self._ok("new-token"))
        service.start("A@Example.com----newpw----" + RFC_SECRET, run_async=False)
        self.assertIsNone(self.accounts.get_account("old-token") if self.accounts._accounts.get("old-token") else None)
        self.assertEqual(len(self.accounts.list_accounts()), 1)
        saved = self.accounts.get_account("new-token")
        self.assertEqual((saved["status"], saved["password"], saved["totp_secret"]), ("正常", "newpw", RFC_SECRET))

    def test_batch_limit_and_empty_input(self) -> None:
        service = pls.ProtocolLoginService(lambda *_: self._ok("t"))
        text = "\n".join(f"u{i}@example.com----pw" for i in range(pls.MAX_BATCH + 1))
        with self.assertRaises(ValueError):
            service.start(text, run_async=False)
        started = service.start("garbage", run_async=False)
        progress = service.get_progress(started["progress_id"])
        self.assertEqual((progress["total"], progress["status"], len(progress["invalid"])), (0, "done", 1))
        self.assertIsNone(service.get_progress("missing"))


class MfaHelperTests(unittest.TestCase):
    def test_factor_id_sources(self) -> None:
        f = AccountService._mfa_factor_id
        self.assertEqual(f({"page": {"payload": {"factor_id": "f1"}}}), "f1")
        self.assertEqual(f({"page": {"payload": {"factor": {"id": "f2"}}}}), "f2")
        self.assertEqual(
            f({"page": {"payload": {"factors": [{"id": "sms", "factor_type": "sms"}, {"id": "totp1", "factor_type": "totp"}]}}}),
            "totp1",
        )
        self.assertEqual(f({"continue_url": "https://auth.openai.com/mfa-challenge/f3?x=1"}), "f3")
        self.assertEqual(f({}), "")

    def test_is_mfa_page(self) -> None:
        is_mfa = AccountService._is_mfa_page
        self.assertTrue(is_mfa({"page": {"type": "mfa_challenge"}}))
        self.assertTrue(is_mfa({"continue_url": "https://auth.openai.com/mfa-challenge/x"}))
        self.assertFalse(is_mfa({"page": {"type": "email_otp_verification"}}))
        self.assertFalse(is_mfa({}))


class FakeResponse:
    def __init__(self, status=200, data=None, url="", headers=None, text=None):
        self.status_code = status
        self._data = data
        self.url = url
        self.headers = headers or {}
        self.text = text if text is not None else ("{}" if data is None else "x")

    def json(self):
        return self._data or {}


class FakeSession:
    """按 URL 路由的假会话，记录所有请求。"""

    def __init__(self, routes, log):
        self.routes = routes
        self.log = log
        self.cookies = mock.Mock()

    def _route(self, method, url, **kwargs):
        self.log.append((method, url, kwargs.get("json")))
        for fragment, handler in self.routes.items():
            if fragment in url:
                return handler(method, url, kwargs)
        return FakeResponse(404, {})

    def get(self, url, **kwargs):
        return self._route("GET", url, **kwargs)

    def post(self, url, **kwargs):
        return self._route("POST", url, **kwargs)

    def close(self):
        pass


class LoginFlowTests(unittest.TestCase):
    CALLBACK = "https://platform.openai.com/auth/callback"

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.accounts = AccountService(JSONStorageBackend(Path(self.tmp.name) / "accounts.json"))
        self.log: list = []

    def _login(self, verify_response, mfa_handler=None, totp_secret=RFC_SECRET):
        jwt_payload = {"https://api.openai.com/profile": {"email": "a@example.com"}, "exp": 9999999999}
        routes = {
            "/api/accounts/authorize": lambda m, u, k: FakeResponse(200, None, url=u, text="<html>"),
            "/api/accounts/password/verify": lambda m, u, k: verify_response,
            "/api/accounts/oauth/token": lambda m, u, k: FakeResponse(
                200, {"access_token": "jwt-token", "refresh_token": "rt", "id_token": "idt"}
            ),
            "chatgpt.com/backend-api/me": lambda m, u, k: FakeResponse(200, {"account": {"account_id": "acc"}}),
        }
        if mfa_handler:
            routes["/api/accounts/mfa/verify"] = mfa_handler
        routes["/hop"] = lambda m, u, k: FakeResponse(302, None, headers={"location": self.CALLBACK + "?code=final-code&state=s"})
        session = FakeSession(routes, self.log)
        with mock.patch("curl_cffi.requests.Session", return_value=session), \
                mock.patch("utils.sentinel.build_sentinel_token", side_effect=RuntimeError("skip")), \
                mock.patch.object(self.accounts, "_decode_jwt_payload", return_value=jwt_payload), \
                mock.patch("services.account_service.time.sleep"):
            return self.accounts._login_with_password("a@example.com", "pw", totp_secret)

    def test_password_only_login_still_works(self) -> None:
        result = self._login(FakeResponse(200, {"continue_url": self.CALLBACK + "?code=abc"}))
        self.assertTrue(result["ok"], result)
        self.assertEqual(result["access_token"], "jwt-token")

    def test_mfa_login_submits_totp_and_follows_redirect(self) -> None:
        verify = FakeResponse(200, {
            "continue_url": "https://auth.openai.com/mfa-challenge/factor-9",
            "page": {"type": "mfa_challenge", "payload": {"factor": {"id": "factor-9"}}},
        })
        mfa = lambda m, u, k: FakeResponse(200, {"continue_url": "https://auth.openai.com/hop"})
        with mock.patch("utils.totp.time.time", return_value=100.0):
            result = self._login(verify, mfa)
        self.assertTrue(result["ok"], result)
        mfa_calls = [c for c in self.log if "/mfa/verify" in c[1]]
        self.assertEqual(len(mfa_calls), 1)
        body = mfa_calls[0][2]
        self.assertEqual((body["id"], body["type"]), ("factor-9", "totp"))
        self.assertEqual(body["code"], totp_code(RFC_SECRET, now=100.0))
        exchange = [c for c in self.log if "/oauth/token" in c[1]]
        self.assertEqual(exchange[0][2]["code"], "final-code")

    def test_mfa_without_secret_reports_need_totp_secret(self) -> None:
        verify = FakeResponse(200, {"page": {"type": "mfa_challenge"}, "continue_url": "https://auth.openai.com/mfa-challenge/f"})
        result = self._login(verify, totp_secret="")
        self.assertEqual((result["ok"], result["error"]), (False, "need_totp_secret"))
        self.assertFalse([c for c in self.log if "/mfa/verify" in c[1]])

    def test_mfa_rejected_code(self) -> None:
        verify = FakeResponse(200, {"page": {"type": "mfa_challenge"}, "continue_url": "https://auth.openai.com/mfa-challenge/f"})
        result = self._login(verify, lambda m, u, k: FakeResponse(400, {"error": {"code": "invalid_code"}}))
        self.assertEqual((result["ok"], result["error"]), (False, "mfa_verify_failed_400"))

    def test_invalid_totp_secret(self) -> None:
        verify = FakeResponse(200, {"page": {"type": "mfa_challenge"}, "continue_url": "https://auth.openai.com/mfa-challenge/f"})
        result = self._login(verify, totp_secret="!!!")
        self.assertEqual(result["error"], "invalid_totp_secret")

    def test_email_otp_is_still_unsupported(self) -> None:
        result = self._login(FakeResponse(200, {"page": {"type": "email_otp_verification"}}))
        self.assertEqual(result["error"], "need_verification_code")

    def test_relogin_passes_stored_totp_secret(self) -> None:
        self.accounts.add_account_items([{
            "access_token": "t", "email": "a@example.com", "password": "pw", "totp_secret": RFC_SECRET, "status": "异常",
        }])
        with mock.patch.object(self.accounts, "_login_with_password", return_value={"ok": False, "error": "x"}) as login, \
                mock.patch.object(self.accounts, "remove_invalid_token"):
            self.accounts._password_re_login_thread("t", "a@example.com", "pw", "manual", None, RFC_SECRET)
        login.assert_called_once_with("a@example.com", "pw", RFC_SECRET)


if __name__ == "__main__":
    unittest.main()
