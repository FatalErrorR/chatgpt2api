import json
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest import mock

from curl_cffi import requests
from curl_cffi.requests import Headers
from fastapi import FastAPI
from fastapi.testclient import TestClient

import api.accounts as accounts_module
import api.proxies as proxies_module
import services.openai_backend_api as backend_module
import services.protocol_login_service as protocol_login_module
from services.account_service import AccountService
from services.openai_backend_api import OpenAIBackendAPI
from services.protocol_login_service import ProtocolLoginService
from services.proxy_list_service import FAILURE_THRESHOLD, ProxyList, mask_proxy_url, proxy_key
from services.storage.json_storage import JSONStorageBackend

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
            self.proxies.delete(A, {A: 2})
        self.proxies.delete(A, {B: 1})
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

    def test_reserve_fills_least_loaded_counting_existing_accounts(self):
        self.assertEqual(self.proxies.reserve([A, B, C], 5, lambda: {A: 2, B: 0}), [B, C, B, C, A])

    def test_reserve_skips_abnormal_unless_all_are_abnormal(self):
        self.fail_times(B)
        self.assertEqual(self.proxies.reserve([A, B], 3, dict), [A, A, A])
        self.proxies.release([A, A, A])
        self.fail_times(A)
        self.assertEqual(self.proxies.reserve([A, B], 2, dict), [A, B])

    def test_reserve_requires_a_candidate(self):
        with self.assertRaises(ValueError):
            self.proxies.reserve(["", " "], 1, dict)

    def test_reservations_count_until_released(self):
        self.assertEqual(self.proxies.reserve([A, B], 2, dict), [A, B])
        self.assertEqual(self.proxies.reserve([A, B], 2, dict), [A, B])
        self.proxies.release([A, A])
        self.assertEqual(self.proxies.reserve([A, B], 2, dict), [A, A])
        self.proxies.release([A, A, B, B, B])  # 多释放不会变负数
        self.assertEqual(self.proxies._reserved, {})

    def test_spellings_of_one_proxy_share_entry_health_and_load(self):
        self.proxies.add("socks5://3.3.3.3:1080")
        with self.assertRaisesRegex(ValueError, "已经在清单里"):
            self.proxies.add("SOCKS5H://3.3.3.3:1080/")
        self.fail_times("socks5h://3.3.3.3:1080")
        [item] = self.proxies.list_entries({"socks5://3.3.3.3:1080": 1, "socks5h://3.3.3.3:1080": 2})
        self.assertEqual((item["url"], item["account_count"], item["health"]["abnormal"]), (C, 3, True))
        with self.assertRaisesRegex(ValueError, "3 个账号"):
            self.proxies.delete("socks5h://3.3.3.3:1080", {C: 1, "socks5h://3.3.3.3:1080": 2})
        self.assertEqual(self.proxies.reserve(["1.1.1.1:8000", "http://1.1.1.1:8000"], 1, dict), ["1.1.1.1:8000"])


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
    """走公开 API 和完整协议登录流程；账号、代理清单都在临时目录，不连上游。"""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.accounts = AccountService(JSONStorageBackend(Path(self.dir.name) / "accounts.json"))
        self.proxies = ProxyList(Path(self.dir.name) / "proxies.json")
        modules = (accounts_module, proxies_module, protocol_login_module, backend_module)
        patchers = [mock.patch.object(module, "account_service", self.accounts) for module in modules]
        patchers += [mock.patch.object(module, "proxy_list", self.proxies) for module in modules]
        patchers += [mock.patch.object(module, "require_admin", lambda *_: {"role": "admin"})
                     for module in (accounts_module, proxies_module)]
        patchers.append(mock.patch.object(self.accounts, "refresh_accounts", self.fake_refresh))
        for patcher in patchers:
            patcher.start()
            self.addCleanup(patcher.stop)
        app = FastAPI()
        app.include_router(accounts_module.create_router())
        app.include_router(proxies_module.create_router())
        self.client = TestClient(app)

    def fake_refresh(self, tokens):
        return {"refreshed": 0, "errors": [], "items": self.accounts.list_accounts()}

    def proxy_of(self, token):
        return (self.accounts.get_account(token) or {}).get("proxy") or ""

    def listed(self, url):
        return next(item for item in self.client.get("/api/proxies").json()["items"] if item["url"] == url)

    def import_accounts(self, **body):
        response = self.client.post("/api/accounts", json=body)
        self.assertEqual(response.status_code, 200, response.text)
        return response.json()

    # ── 1. 健康记录与清单、分配用同一个识别值 ──────────────
    def test_socks5_proxy_health_matches_the_requests_and_abnormal_proxy_is_skipped(self):
        self.accounts.add_account_items([{"access_token": "s0", "proxy": C}])
        api = OpenAIBackendAPI("s0")
        self.addCleanup(api.close)
        self.assertEqual(api.proxy_url, "socks5h://3.3.3.3:1080")  # 实际请求用的是归一化后的地址
        challenge = mock.Mock(status_code=403, headers=Headers({"Content-Type": "text/html"}))
        with (
            mock.patch.object(api.session, "get", return_value=challenge),
            mock.patch("services.openai_backend_api.time.sleep"),
        ):
            for _ in range(FAILURE_THRESHOLD):
                with self.assertRaises(RuntimeError):
                    api._get_me()
        health = self.listed(C)["health"]
        self.assertEqual((health["abnormal"], health["failures"]), (True, FAILURE_THRESHOLD))
        self.assertEqual(self.listed(C)["account_count"], 1)

        self.import_accounts(tokens=["n1", "n2"], distribute_proxies=[C, B])
        self.assertEqual((self.proxy_of("n1"), self.proxy_of("n2")), (B, B))
        self.assertEqual(self.proxy_of("s0"), C)  # 账号上的原始写法不改

        self.proxies.record_success(api.proxy_url)
        self.assertFalse(self.listed(C)["health"]["abnormal"])

    def test_a_proxy_written_two_ways_is_one_entry_with_combined_load(self):
        self.accounts.add_account_items([
            {"access_token": "x1", "proxy": C},
            {"access_token": "x2", "proxy": "socks5h://3.3.3.3:1080"},
        ])
        items = self.client.get("/api/proxies").json()["items"]
        self.assertEqual([(item["url"], item["account_count"]) for item in items], [(C, 2)])
        response = self.client.post("/api/proxies/delete", json={"url": "socks5h://3.3.3.3:1080"})
        self.assertEqual(response.status_code, 400)
        self.assertIn("2 个账号", response.json()["detail"]["error"])
        self.accounts.add_account_items([{"access_token": "b1", "proxy": B}])
        self.import_accounts(tokens=["n1"], distribute_proxies=["socks5h://3.3.3.3:1080", B])
        self.assertEqual(self.proxy_of("n1"), B)

    # ── 2. 平均分配不动已存在账号的代理 ──────────────────
    def test_distribute_keeps_existing_proxies_even_when_the_json_carries_one(self):
        self.accounts.add_account_items([
            {"access_token": "old", "proxy": "http://original:80"},
            {"access_token": "old-direct"},
        ])
        imported = "http://imported:80"
        result = self.import_accounts(
            accounts=[
                {"access_token": "old", "proxy": imported},
                {"access_token": "old-direct", "proxy": imported},
                {"access_token": "new", "proxy": imported},
            ],
            tokens=["old", "new-plain"],
            distribute_proxies=[A, B],
        )
        self.assertEqual((result["added"], result["skipped"]), (2, 2))
        self.assertEqual(self.proxy_of("old"), "http://original:80")
        self.assertEqual(self.proxy_of("old-direct"), "")
        self.assertEqual((self.proxy_of("new-plain"), self.proxy_of("new")), (A, B))  # tokens 排在 JSON 前
        self.assertEqual(self.proxies._reserved, {})

    def test_without_distribute_the_json_and_body_proxy_behave_as_before(self):
        self.accounts.add_account_items([{"access_token": "old", "proxy": "http://original:80"}])
        self.import_accounts(accounts=[{"access_token": "old", "proxy": "http://imported:80"}])
        self.assertEqual(self.proxy_of("old"), "http://imported:80")
        self.import_accounts(accounts=[{"access_token": "old", "proxy": "http://imported:80"}], tokens=["t1"], proxy=B)
        self.assertEqual((self.proxy_of("old"), self.proxy_of("t1")), (B, B))
        self.import_accounts(tokens=["t2"])
        self.assertEqual(self.proxy_of("t2"), "")

    # ── 3. 协议登录跨批次预留 ─────────────────────────────
    def login_service(self, login):
        return ProtocolLoginService(login=login)

    @staticmethod
    def lines(prefix, count):
        return "\n".join(f"{prefix}{i}@example.com----pw" for i in range(count))

    def wait_done(self, service, *progress_ids):
        for _ in range(200):
            if all(service.get_progress(pid)["status"] == "done" for pid in progress_ids):
                return
            time.sleep(0.02)
        self.fail("protocol login did not finish")

    def test_overlapping_login_batches_count_each_others_pending_accounts(self):
        self.accounts.add_account_items([{"access_token": f"a{i}", "proxy": A} for i in range(10)])
        self.accounts.add_account_items([{"access_token": "kept", "email": "kept@example.com", "proxy": C}])
        gate = threading.Event()

        def login(email, password, totp_secret):
            gate.wait(5)
            return {"ok": True, "access_token": "tok-" + email, "email": email}

        service = self.login_service(login)
        first = service.start(self.lines("one", 10), distribute_proxies=[A, B])
        second = service.start(self.lines("two", 10) + "\nkept@example.com----pw", distribute_proxies=[A, B])
        self.assertEqual(self.proxies._reserved, {proxy_key(B): 15, proxy_key(A): 5})
        gate.set()
        self.wait_done(service, first["progress_id"], second["progress_id"])

        assigned = {account["email"]: account.get("proxy") or "" for account in self.accounts.list_accounts() if account.get("email")}
        self.assertEqual({assigned[f"one{i}@example.com"] for i in range(10)}, {B})
        second_batch = [assigned[f"two{i}@example.com"] for i in range(10)]
        self.assertEqual((second_batch.count(A), second_batch.count(B)), (5, 5))
        self.assertEqual(assigned["kept@example.com"], C)  # 已在号池的邮箱沿用原代理，不占名额
        counts = self.accounts.proxy_account_counts()
        self.assertEqual((counts[A], counts[B], counts[C]), (15, 15, 1))
        self.assertEqual(self.proxies._reserved, {})

    def test_failed_logins_release_their_reservations(self):
        def login(email, password, totp_secret, proxy=""):
            if email.startswith("bad"):
                return {"ok": False, "error": "wrong password"}
            if email.startswith("boom"):
                raise RuntimeError("upstream exploded")
            return {"ok": True, "access_token": "tok-" + email, "email": email}

        service = self.login_service(login)
        text = "\n".join([self.lines("ok", 2), self.lines("bad", 2), self.lines("boom", 2)])
        progress = service.start(text, distribute_proxies=[A, B], run_async=False)
        self.assertEqual(service.get_progress(progress["progress_id"])["success"], 2)
        self.assertEqual(self.proxies._reserved, {})
        self.assertEqual(sum(self.accounts.proxy_account_counts().values()), 2)
        # 失败的名额已释放，下一批按实际账号数分
        self.assertEqual(self.proxies.reserve([A, B], 2, self.accounts.proxy_account_counts), [A, B])

    def test_a_batch_that_dies_releases_every_reservation(self):
        service = self.login_service(lambda *args, **kwargs: {"ok": True})
        with mock.patch.object(protocol_login_module, "ThreadPoolExecutor", side_effect=RuntimeError("no threads")):
            with self.assertRaises(RuntimeError):
                service.start(self.lines("n", 4), distribute_proxies=[A, B], run_async=False)
        self.assertEqual(self.proxies._reserved, {})
        with mock.patch.object(protocol_login_module.threading.Thread, "start", side_effect=RuntimeError("no thread")):
            with self.assertRaises(RuntimeError):
                service.start(self.lines("m", 4), distribute_proxies=[A, B])
        self.assertEqual(self.proxies._reserved, {})

    def test_reservation_is_released_before_refresh_so_the_next_batch_sees_real_load(self):
        self.accounts.add_account_items([{"access_token": f"a{i}", "proxy": A} for i in range(2)])
        first_tokens = {"tok-f0@example.com", "tok-f1@example.com"}
        saved = threading.Semaphore(0)
        finish_refresh = threading.Event()
        self.addCleanup(finish_refresh.set)

        def refresh(tokens):
            if set(tokens) & first_tokens:
                saved.release()
                finish_refresh.wait(5)  # 第一批：已入库，卡在刷新额度
            return self.fake_refresh(tokens)

        login = lambda email, password, totp: {"ok": True, "access_token": "tok-" + email, "email": email}
        service = self.login_service(login)
        with mock.patch.object(self.accounts, "refresh_accounts", refresh):
            first = service.start(self.lines("f", 2), distribute_proxies=[A, B])
            self.assertTrue(saved.acquire(timeout=5) and saved.acquire(timeout=5))
            self.assertEqual(self.accounts.proxy_account_counts(), {A: 2, B: 2})
            self.assertEqual(self.proxies._reserved, {})  # 已入库的不再同时占预留
            service.start(self.lines("g", 2), distribute_proxies=[A, B], run_async=False)
            finish_refresh.set()
            self.wait_done(service, first["progress_id"])
        self.assertEqual(self.accounts.proxy_account_counts(), {A: 3, B: 3})
        self.assertEqual(self.proxies._reserved, {})

    def test_refresh_failure_does_not_release_another_batchs_reservation(self):
        hold_login = threading.Event()
        self.addCleanup(hold_login.set)

        def login(email, password, totp):
            if email.startswith("wait"):
                hold_login.wait(5)
            return {"ok": True, "access_token": "tok-" + email, "email": email}

        def refresh(tokens):
            raise RuntimeError("quota refresh failed")

        service = self.login_service(login)
        with mock.patch.object(self.accounts, "refresh_accounts", refresh):
            waiting = service.start(self.lines("wait", 2), distribute_proxies=[A, B])  # 预留 A、B 各一
            self.assertEqual(self.proxies._reserved, {proxy_key(A): 1, proxy_key(B): 1})
            done = service.start(self.lines("ok", 2), distribute_proxies=[A, B], run_async=False)
            progress = service.get_progress(done["progress_id"])
            self.assertEqual(progress["failed"], 2)  # 刷新失败记为失败，但账号已入库
            self.assertEqual(self.accounts.proxy_account_counts(), {A: 1, B: 1})
            self.assertEqual(self.proxies._reserved, {proxy_key(A): 1, proxy_key(B): 1})  # 另一批的预留原样保留
            hold_login.set()
            self.wait_done(service, waiting["progress_id"])
        self.assertEqual(self.accounts.proxy_account_counts(), {A: 2, B: 2})
        self.assertEqual(self.proxies._reserved, {})

    # ── 代理别名：前端靠后端给的 key 匹配 ──────────────────
    def test_accounts_and_proxy_list_expose_the_same_key_for_aliases(self):
        socks5h = "socks5h://3.3.3.3:1080"
        self.accounts.add_account_items([
            {"access_token": "x1", "proxy": C},
            {"access_token": "x2", "proxy": socks5h},
            {"access_token": "d1"},
        ])
        [entry] = self.client.get("/api/proxies").json()["items"]
        self.assertEqual((entry["url"], entry["key"], entry["account_count"]), (C, proxy_key(C), 2))

        def keys(items):
            return {item["access_token"]: (item.get("proxy") or "", item["proxy_key"]) for item in items}

        expected = {"x1": (C, entry["key"]), "x2": (socks5h, entry["key"]), "d1": ("", "")}
        self.assertEqual(keys(self.client.get("/api/accounts").json()["items"]), expected)  # 初次加载

        imported = self.import_accounts(tokens=["n1"], distribute_proxies=[C])  # 导入完成
        self.assertEqual(keys(imported["items"])["n1"], (C, entry["key"]))
        self.assertEqual({k: v for k, v in keys(imported["items"]).items() if k != "n1"}, expected)

        response = self.client.post("/api/accounts/update", json={"access_token": "d1", "proxy": socks5h})  # 修改账号
        self.assertEqual(response.status_code, 200, response.text)
        self.assertEqual(response.json()["item"]["proxy_key"], entry["key"])
        self.assertEqual(keys(response.json()["items"])["d1"], (socks5h, entry["key"]))
        removed = self.client.request("DELETE", "/api/accounts", json={"tokens": ["n1"]})  # 删除后的列表也带 key
        self.assertEqual(removed.status_code, 200, removed.text)
        self.assertEqual(keys(removed.json()["items"])["x2"], (socks5h, entry["key"]))
        self.assertEqual([item["account_count"] for item in self.client.get("/api/proxies").json()["items"]], [3])

    def test_proxy_key_is_derived_and_never_stored(self):
        self.import_accounts(accounts=[{"access_token": "j1", "proxy": C, "proxy_key": "bogus"}])
        self.assertEqual(self.client.get("/api/accounts").json()["items"][0]["proxy_key"], proxy_key(C))
        self.assertEqual(self.accounts.get_account("j1")["proxy"], C)  # 原始写法不改
        stored = (Path(self.dir.name) / "accounts.json").read_text(encoding="utf-8")
        self.assertNotIn("proxy_key", stored)
        self.assertNotIn("bogus", stored)

    def test_single_proxy_login_is_unchanged(self):
        service = self.login_service(lambda email, password, totp: {"ok": True, "access_token": "tok-" + email, "email": email})
        service.start(self.lines("n", 2), proxy=C, run_async=False)
        self.assertEqual([self.proxy_of(f"tok-n{i}@example.com") for i in range(2)], [C, C])
        self.assertEqual(self.proxies._reserved, {})


class ProxyBatchAddTests(unittest.TestCase):
    """POST /api/proxies/batch：多行添加、厂商命名接着最大序号、去重、无效行、密码不外露。"""

    # 借用同一套临时账号/代理清单和 API 客户端，但不继承（否则会把那边的用例再跑一遍）
    setUp = ImportDistributionTests.setUp
    fake_refresh = ImportDistributionTests.fake_refresh
    listed = ImportDistributionTests.listed

    def batch(self, text, vendor="", status=200):
        response = self.client.post("/api/proxies/batch", json={"text": text, "vendor": vendor})
        self.assertEqual(response.status_code, status, response.text)
        return response.json()

    def names(self):
        return {item["masked_url"]: item["name"] for item in self.client.get("/api/proxies").json()["items"]}

    def test_colon_format_is_saved_as_url_and_the_password_is_hidden(self):
        result = self.batch("194.39.33.194:5903:rgqflcyp:gqnfwz1z3697\n 5.6.7.8:3128 \n\nsocks5://u:p@9.9.9.9:1080", "IPRoyal")
        self.assertEqual([item["name"] for item in result["added"]], ["IPRoyal-1", "IPRoyal-2", "IPRoyal-3"])
        # 显示用的字段不带密码（url/key 是管理员接口给前端改名、删除用的，本来就是完整地址）
        displayed = [item["masked_url"] for item in self.client.get("/api/proxies").json()["items"]]
        self.assertFalse(any("gqnfwz1z3697" in text for text in displayed + [i["masked_url"] for i in result["added"]]))
        self.assertEqual(self.names(), {
            "http://rgqflcyp:***@194.39.33.194:5903": "IPRoyal-1",
            "http://5.6.7.8:3128": "IPRoyal-2",
            "socks5://u:***@9.9.9.9:1080": "IPRoyal-3",
        })
        stored = json.loads((Path(self.dir.name) / "proxies.json").read_text(encoding="utf-8"))
        self.assertEqual(stored[0]["url"], "http://rgqflcyp:gqnfwz1z3697@194.39.33.194:5903")  # 实际请求用的地址

    def test_numbering_continues_from_the_vendors_highest_number(self):
        self.batch("1.1.1.1:1\n1.1.1.2:1", "IPRoyal")
        self.client.post("/api/proxies/update", json={"url": "http://1.1.1.2:1", "name": "iproyal 7"})
        self.batch("1.1.1.3:1", "Other")
        result = self.batch("1.1.1.4:1\n1.1.1.5:1", "IPRoyal")
        self.assertEqual([item["name"] for item in result["added"]], ["IPRoyal-8", "IPRoyal-9"])
        self.assertEqual(self.batch("1.1.1.6:1", "Other")["added"][0]["name"], "Other-2")

    def test_duplicates_and_invalid_lines_are_reported_and_do_not_use_numbers(self):
        self.accounts.add_account_items([{"access_token": "a1", "proxy": "socks5://2.2.2.2:1080"}])
        self.batch("3.3.3.3:80", "V")
        result = self.batch(
            "socks5h://2.2.2.2:1080\n3.3.3.3:80\n4.4.4.4:80:u:p\n4.4.4.4:80:u:p\nnot-a-proxy\n5.5.5.5:99999\nftp://6.6.6.6:21\n7.7.7.7:80",
            "V",
        )
        self.assertEqual([(item["name"], item["masked_url"]) for item in result["added"]],
                         [("V-2", "http://u:***@4.4.4.4:80"), ("V-3", "http://7.7.7.7:80")])
        self.assertEqual([(item["line"], item["reason"]) for item in result["skipped"]],
                         [(1, "已在清单里"), (2, "已在清单里"), (4, "本批重复")])
        self.assertEqual([item["line"] for item in result["invalid"]], [5, 6, 7])
        shown = [item["masked_url"] for item in result["added"]] + [item["text"] for item in result["skipped"] + result["invalid"]]
        self.assertFalse(any(":p@" in text for text in shown))
        self.assertEqual(self.listed("socks5://2.2.2.2:1080")["account_count"], 1)  # 账号带进来的那条不变

    def test_without_vendor_names_stay_empty(self):
        result = self.batch("8.8.8.8:80")
        self.assertEqual(result["added"][0]["name"], "")

    def test_empty_and_oversized_batches_are_rejected(self):
        self.assertIn("每行一个", self.batch(" \n ", status=400)["detail"]["error"])
        lines = "\n".join(f"10.0.{i // 250}.{i % 250}:80" for i in range(501))
        self.assertIn("500", self.batch(lines, status=400)["detail"]["error"])
        self.assertEqual(self.client.get("/api/proxies").json()["items"], [])

    def test_colon_format_proxy_on_an_account_is_masked_in_the_list(self):
        self.accounts.add_account_items([{"access_token": "c1", "proxy": "194.39.33.194:5903:rgqflcyp:gqnfwz1z3697"}])
        [item] = self.client.get("/api/proxies").json()["items"]
        self.assertEqual(item["masked_url"], "http://rgqflcyp:***@194.39.33.194:5903")
        self.assertEqual(item["account_count"], 1)
        self.assertEqual(self.batch("194.39.33.194:5903:rgqflcyp:gqnfwz1z3697")["skipped"][0]["reason"], "已在清单里")


if __name__ == "__main__":
    unittest.main()
