import tempfile
import unittest
from pathlib import Path
from unittest import mock

from curl_cffi import requests
from curl_cffi.requests import Headers

import api.accounts as accounts_module
from services.openai_backend_api import OpenAIBackendAPI
from services.protocol_login_service import ProtocolLoginService
from services.proxy_list_service import FAILURE_THRESHOLD, ProxyList, mask_proxy_url

A = "http://user:secret@1.1.1.1:8000"
B = "http://2.2.2.2:8000"
C = "socks5://3.3.3.3:1080"


class ProxyListTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.path = Path(self.dir.name) / "proxies.json"
        self.proxies = ProxyList(self.path)

    def fail_times(self, url, times=FAILURE_THRESHOLD):
        for _ in range(times):
            self.proxies.record_failure(url, "boom")

    def test_mask_hides_only_the_password(self):
        self.assertEqual(mask_proxy_url(A), "http://user:***@1.1.1.1:8000")
        self.assertEqual(mask_proxy_url("user:pw@host:1"), "user:***@host:1")
        self.assertEqual(mask_proxy_url(B), B)

    def test_list_seeds_account_proxies_and_keeps_names(self):
        self.proxies.add(B, "美西")
        items = self.proxies.list_entries({A: 2, B: 1})
        self.assertEqual([(i["url"], i["name"], i["account_count"]) for i in items], [(B, "美西", 1), (A, "", 2)])
        self.assertEqual(items[1]["masked_url"], "http://user:***@1.1.1.1:8000")
        reloaded = ProxyList(self.path).list_entries({})
        self.assertEqual([(i["url"], i["name"]) for i in reloaded], [(B, "美西"), (A, "")])

    def test_rename_add_and_delete_rules(self):
        self.proxies.add(A)
        with self.assertRaises(ValueError):
            self.proxies.add(A)
        with self.assertRaises(ValueError):
            self.proxies.add("  ")
        self.proxies.rename(A, " 主力 ")
        self.assertEqual(self.proxies.list_entries({})[0]["name"], "主力")
        with self.assertRaisesRegex(ValueError, "2 个账号"):
            self.proxies.delete(A, 2)
        self.proxies.delete(A, 0)
        self.assertEqual(self.proxies.list_entries({}), [])

    def test_consecutive_failures_mark_abnormal_and_success_recovers(self):
        self.fail_times(A, FAILURE_THRESHOLD - 1)
        self.assertFalse(self.proxies.is_abnormal(A))
        self.fail_times(A, 1)
        self.assertTrue(self.proxies.is_abnormal(A))
        self.proxies.record_success(A)
        self.assertFalse(self.proxies.is_abnormal(A))
        health = self.proxies.list_entries({A: 1})[0]["health"]
        self.assertEqual((health["failures"], health["successes"], health["last_error"]), (FAILURE_THRESHOLD, 1, "boom"))

    def test_distribute_fills_least_loaded_counting_existing_accounts(self):
        self.assertEqual(self.proxies.distribute([A, B, C], 5, {A: 2, B: 0}), [B, C, B, C, A])

    def test_distribute_skips_abnormal_unless_all_are_abnormal(self):
        self.fail_times(B)
        self.assertEqual(self.proxies.distribute([A, B], 3, {}), [A, A, A])
        self.fail_times(A)
        self.assertEqual(self.proxies.distribute([A, B], 2, {}), [A, B])

    def test_distribute_requires_a_candidate(self):
        with self.assertRaises(ValueError):
            self.proxies.distribute(["", " "], 1, {})


class ProxyHealthHookTests(unittest.TestCase):
    def setUp(self):
        self.api = OpenAIBackendAPI("")
        self.addCleanup(self.api.close)
        self.api.proxy_url = A
        patcher = mock.patch("services.openai_backend_api.proxy_list")
        self.proxies = patcher.start()
        self.addCleanup(patcher.stop)
        sleep = mock.patch("services.openai_backend_api.time.sleep")
        sleep.start()
        self.addCleanup(sleep.stop)

    def response(self, status, headers=None):
        response = mock.Mock(status_code=status, headers=Headers(headers or {}))
        response.json.return_value = {}
        return response

    def test_response_after_challenge_counts_as_success(self):
        side_effect = [self.response(403, {"CF-Mitigated": "challenge"}), self.response(200)]
        with mock.patch.object(self.api.session, "get", side_effect=side_effect):
            self.api._get_me()
        self.proxies.record_success.assert_called_once_with(A)
        self.proxies.record_failure.assert_not_called()

    def test_exhausted_challenge_counts_as_one_failure(self):
        with mock.patch.object(self.api.session, "get", return_value=self.response(403, {"Content-Type": "text/html"})):
            with self.assertRaises(RuntimeError):
                self.api._get_me()
        self.proxies.record_failure.assert_called_once_with(A, "Cloudflare challenge (HTTP 403)")

    def test_json_rejection_still_means_the_proxy_works(self):
        with mock.patch.object(self.api.session, "get", return_value=self.response(403, {"Content-Type": "application/json"})):
            with self.assertRaises(RuntimeError):
                self.api._get_me()
        self.proxies.record_success.assert_called_once_with(A)

    def test_transport_error_counts_as_failure_and_propagates(self):
        with mock.patch.object(self.api.session, "get", side_effect=requests.RequestsError("curl: (7) proxy refused")):
            with self.assertRaises(requests.RequestsError):
                self.api._get_me()
        self.proxies.record_failure.assert_called_once()
        self.assertIn("proxy refused", self.proxies.record_failure.call_args.args[1])


class ImportDistributionTests(unittest.TestCase):
    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.proxies = ProxyList(Path(self.dir.name) / "proxies.json")
        accounts = [{"access_token": "old", "email": "old@example.com", "proxy": B}]
        for target, attr, value in (
            (accounts_module, "proxy_list", self.proxies),
            ("services.protocol_login_service.proxy_list", None, self.proxies),
        ):
            patcher = mock.patch.object(target, attr, value) if attr else mock.patch(target, value)
            patcher.start()
            self.addCleanup(patcher.stop)
        service = mock.patch.multiple(
            "services.account_service.account_service",
            get_account=lambda token: accounts[0] if token == "old" else None,
            proxy_account_counts=lambda: {B: 1},
            list_accounts=lambda: accounts,
        )
        service.start()
        self.addCleanup(service.stop)

    def test_token_import_distributes_only_new_tokens(self):
        assigned = accounts_module._import_proxies(["old", "t1", "t2", "t3"], "", [A, B])
        self.assertEqual(assigned, {"t1": A, "t2": A, "t3": B})

    def test_token_import_single_proxy_is_unchanged(self):
        self.assertEqual(accounts_module._import_proxies(["old", "t1"], C, []), {"old": C, "t1": C})
        self.assertEqual(accounts_module._import_proxies(["t1"], "", []), {})

    def test_plain_tokens_without_proxy_never_write_an_empty_proxy(self):
        with (
            mock.patch("services.account_service.account_service.add_accounts", return_value={}) as add_accounts,
            mock.patch("services.account_service.account_service.add_account_items", return_value={}) as add_items,
        ):
            accounts_module._add_plain_tokens(["old"], {})
            accounts_module._add_plain_tokens(["old", "t1"], {"t1": A})
        add_accounts.assert_called_once_with(["old"])
        self.assertEqual(add_items.call_args.args[0], [
            {"access_token": "old", "source_type": "web"},
            {"access_token": "t1", "source_type": "web", "proxy": A},
        ])

    def test_protocol_login_assigns_before_login_and_keeps_existing_email_proxy(self):
        entries = [{"email": "OLD@example.com"}, {"email": "n1@example.com"}, {"email": "n2@example.com"}]
        ProtocolLoginService._assign_proxies(entries, "", [A, B])
        self.assertEqual([entry["proxy"] for entry in entries], [B, A, A])

    def test_protocol_login_single_proxy_is_unchanged(self):
        entries = [{"email": "old@example.com"}, {"email": "n1@example.com"}]
        ProtocolLoginService._assign_proxies(entries, C, [])
        self.assertEqual([entry["proxy"] for entry in entries], [C, C])


if __name__ == "__main__":
    unittest.main()
