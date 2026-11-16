"""代理运维：停用失败代理并迁移账号、启用、检测、重新分配、单代理账号上限。全部走公开 API，用临时数据。"""

from __future__ import annotations

import contextlib
import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from fastapi import FastAPI
from fastapi.testclient import TestClient

import api.accounts as accounts_module
import api.proxies as proxies_module
import services.protocol_login_service as protocol_login_module
import services.proxy_reassign_service as reassign_module
from services.account_service import AccountService
from services.config import ConfigStore
from services.proxy_list_service import ProxyList, pick_least_loaded, CapacityError, proxy_key
from services.storage.json_storage import JSONStorageBackend

A = "http://u:p@10.0.0.1:1000"
B = "http://u:p@10.0.0.2:1000"
C = "http://u:p@10.0.0.3:1000"
D = "http://u:p@10.0.0.4:1000"


class ProxyOpsTestCase(unittest.TestCase):
    max_accounts = 4

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.accounts = AccountService(JSONStorageBackend(Path(self.dir.name) / "accounts.json"))
        self.proxies = ProxyList(Path(self.dir.name) / "proxies.json")
        self.log = mock.Mock()
        stack = contextlib.ExitStack()
        self.addCleanup(stack.close)
        for module in (accounts_module, proxies_module, protocol_login_module, reassign_module):
            stack.enter_context(mock.patch.object(module, "account_service", self.accounts))
            if hasattr(module, "proxy_list"):
                stack.enter_context(mock.patch.object(module, "proxy_list", self.proxies))
        for module in (accounts_module, proxies_module):
            stack.enter_context(mock.patch.object(module, "require_admin", lambda *_: {"role": "admin"}))
        stack.enter_context(mock.patch.object(reassign_module, "log_service", self.log))
        stack.enter_context(mock.patch.object(self.accounts, "refresh_accounts", lambda tokens: {"refreshed": 0, "errors": [], "items": self.accounts.list_accounts()}))
        self.max_patch = stack.enter_context(
            mock.patch.object(ConfigStore, "proxy_max_accounts", new_callable=mock.PropertyMock, return_value=self.max_accounts)
        )
        app = FastAPI()
        app.include_router(accounts_module.create_router())
        app.include_router(proxies_module.create_router())
        self.client = TestClient(app)

    def seed(self, layout: dict[str, int], extra_proxies=(), disabled=()):
        """layout: 代理 -> 账号数。账号 token 形如 a0-1（代理序号-账号序号）。"""
        items = []
        for p_index, (url, count) in enumerate(layout.items()):
            for a_index in range(count):
                items.append({"access_token": f"t{p_index}-{a_index}", "email": f"u{p_index}{a_index}@example.com", "proxy": url})
        self.accounts.add_account_items(items)
        for url in list(layout) + list(extra_proxies):
            if not any(entry["key"] == proxy_key(url) for entry in self.listed()):
                self.proxies.add(url)
        if disabled:
            self.proxies.set_disabled(list(disabled), True)

    def listed(self):
        return self.client.get("/api/proxies").json()["items"]

    def counts(self):
        return {proxy_key(url): n for url, n in self.accounts.proxy_account_counts().items()}

    def post(self, path, body, status=200):
        response = self.client.post(path, json=body)
        self.assertEqual(response.status_code, status, response.text)
        return response.json()


class RetireTests(ProxyOpsTestCase):
    def test_dry_run_plans_least_loaded_moves_and_changes_nothing(self):
        self.seed({A: 3, B: 1}, extra_proxies=[C, D], disabled=[D])
        before = self.counts()
        plan = self.post("/api/proxies/retire", {"urls": [A], "dry_run": True})
        self.assertEqual([move["to"] for move in plan["moves"]], ["10.0.0.3:1000", "10.0.0.2:1000", "10.0.0.3:1000"])
        self.assertEqual({t["label"]: (t["before"], t["after"]) for t in plan["targets"]}, {"10.0.0.2:1000": (1, 2), "10.0.0.3:1000": (0, 2)})
        self.assertNotIn("token", plan["moves"][0])
        self.assertNotIn(":p@", json.dumps(plan["moves"]))  # 方案里不带代理密码
        self.assertEqual(self.counts(), before)
        self.assertFalse(next(e for e in self.listed() if e["key"] == proxy_key(A))["disabled"])

    def test_apply_moves_accounts_then_disables_and_logs(self):
        self.seed({A: 3, B: 1}, extra_proxies=[C])
        result = self.post("/api/proxies/retire", {"urls": [A], "dry_run": False})
        self.assertEqual((result["moved"], result["disabled"]), (3, 1))
        self.assertEqual(self.counts(), {proxy_key(B): 2, proxy_key(C): 2})
        entry = next(e for e in result["items"] if e["key"] == proxy_key(A))
        self.assertTrue(entry["disabled"])
        self.assertEqual(entry["account_count"], 0)
        summaries = [call.args[1] for call in self.log.add.call_args_list]
        self.assertIn("停用 1 个代理：迁移 3 个账号的代理", summaries)
        self.assertIn("停用 1 个代理", summaries)
        # 停用标记落盘，重启后还在
        self.assertTrue(ProxyList(Path(self.dir.name) / "proxies.json").is_disabled(A))

    def test_capacity_shortfall_previews_and_blocks_apply(self):
        self.max_patch.return_value = 2
        self.seed({A: 3, B: 2}, extra_proxies=[C])
        plan = self.post("/api/proxies/retire", {"urls": [A], "dry_run": True})
        self.assertEqual(plan["shortfall"], 1)
        self.assertIn("单代理最多账号数", plan["error"])
        error = self.post("/api/proxies/retire", {"urls": [A], "dry_run": False}, status=400)
        self.assertIn("还差 1 个位置", error["detail"]["error"])
        self.assertEqual(self.counts()[proxy_key(A)], 3)  # 不做部分迁移
        self.assertFalse(self.proxies.is_disabled(A))

    def test_abnormal_and_disabled_proxies_never_receive_accounts(self):
        self.seed({A: 2}, extra_proxies=[B, C, D], disabled=[D])
        for _ in range(3):
            self.proxies.record_failure(B, "boom")
        result = self.post("/api/proxies/retire", {"urls": [A], "dry_run": False})
        self.assertEqual(result["moved"], 2)
        self.assertEqual(self.counts(), {proxy_key(C): 2})

    def test_no_usable_target_is_an_error(self):
        self.seed({A: 1}, extra_proxies=[B], disabled=[B])
        plan = self.post("/api/proxies/retire", {"urls": [A], "dry_run": True})
        self.assertIn("没有可用的代理", plan["error"])
        self.post("/api/proxies/retire", {"urls": [A], "dry_run": False}, status=400)

    def test_retiring_an_empty_proxy_just_disables_it(self):
        self.seed({A: 1}, extra_proxies=[B])
        result = self.post("/api/proxies/retire", {"urls": [B], "dry_run": False})
        self.assertEqual((result["moved"], result["disabled"]), (0, 1))

    def test_enable_brings_a_proxy_back_into_distribution(self):
        self.seed({A: 1}, extra_proxies=[B], disabled=[B])
        error = self.post("/api/accounts", {"tokens": ["n1"], "distribute_proxies": [B]}, status=400)
        self.assertIn("都已停用", error["detail"]["error"])
        self.assertEqual(self.post("/api/proxies/enable", {"urls": [B]})["enabled"], 1)
        self.post("/api/accounts", {"tokens": ["n1"], "distribute_proxies": [B]})
        self.assertEqual(self.accounts.get_account("n1")["proxy"], B)


class DistributionCapTests(ProxyOpsTestCase):
    def test_import_respects_the_per_proxy_cap_and_skips_disabled(self):
        self.max_patch.return_value = 2
        self.seed({A: 1}, extra_proxies=[B, C], disabled=[C])
        self.post("/api/accounts", {"tokens": ["n1", "n2", "n3"], "distribute_proxies": [A, B, C]})
        self.assertEqual(self.counts(), {proxy_key(A): 2, proxy_key(B): 2})
        error = self.post("/api/accounts", {"tokens": ["n4"], "distribute_proxies": [A, B]}, status=400)
        self.assertIn("还差 1 个位置", error["detail"]["error"])
        self.assertIsNone(self.accounts.get_account("n4"))

    def test_pick_least_loaded_is_all_or_nothing(self):
        self.assertEqual(pick_least_loaded(["a", "b"], {"a": 1, "b": 0}, 3, 2), ["b", "a", "b"])
        with self.assertRaises(CapacityError) as caught:
            pick_least_loaded(["a", "b"], {"a": 1, "b": 0}, 4, 2)
        self.assertEqual(caught.exception.shortfall, 1)
        self.assertEqual(pick_least_loaded(["a"], {"a": 9}, 2, None), ["a", "a"])


class ReassignTests(ProxyOpsTestCase):
    def test_reassign_selected_accounts_to_all_usable_proxies(self):
        self.seed({A: 4}, extra_proxies=[B, C])
        plan = self.post("/api/accounts/reassign-proxy", {"tokens": ["t0-0", "t0-1", "t0-2", "t0-3"], "dry_run": True})
        self.assertEqual(len(plan["moves"]) + plan["unchanged"], 4)
        self.assertEqual(self.counts(), {proxy_key(A): 4})
        result = self.post("/api/accounts/reassign-proxy", {"tokens": ["t0-0", "t0-1", "t0-2", "t0-3"], "dry_run": False})
        self.assertEqual(sorted(self.counts().values()), [1, 1, 2])
        self.assertEqual(result["moved"], len(result["moves"]))
        self.assertEqual(len(result["items"]), 4)

    def test_reassign_to_chosen_proxies_only(self):
        self.seed({A: 2, B: 1}, extra_proxies=[C, D])
        self.post("/api/accounts/reassign-proxy", {"tokens": ["t0-0", "t0-1"], "proxies": [D], "dry_run": False})
        self.assertEqual(self.counts(), {proxy_key(B): 1, proxy_key(D): 2})

    def test_unknown_accounts_are_an_error(self):
        self.seed({A: 1})
        self.post("/api/accounts/reassign-proxy", {"tokens": ["nope"], "dry_run": True}, status=400)
        self.post("/api/accounts/reassign-proxy", {"tokens": [], "dry_run": True}, status=400)


class CheckTests(ProxyOpsTestCase):
    def check(self, failing: dict[str, str]):
        def fake_test(url, **_):
            key = proxy_key(url)
            if key in failing:
                return {"ok": False, "status": 0, "latency_ms": 5, "error": failing[key]}
            return {"ok": True, "status": 200, "latency_ms": 5, "error": None}

        with mock.patch.object(reassign_module, "test_proxy", side_effect=fake_test):
            return self.post("/api/proxies/check", {})

    def test_one_failed_round_is_offered_for_retirement(self):
        self.seed({A: 2, B: 1, C: 1, D: 0})
        result = self.check({proxy_key(A): "curl: (28) Connection timed out"})
        self.assertEqual((result["tested"], result["passed"], result["outage"]), (4, 3, False))
        self.assertEqual([item["label"] for item in result["failures"]], ["10.0.0.1:1000"])
        self.assertEqual(len(result["plan"]["moves"]), 2)
        self.assertEqual(self.counts()[proxy_key(A)], 2)  # 只检测，不执行
        health = next(e for e in result["items"] if e["key"] == proxy_key(A))["health"]
        self.assertEqual(health["failures"], 1)

    def test_disabled_proxies_are_not_tested(self):
        self.seed({A: 1}, extra_proxies=[B], disabled=[B])
        self.assertEqual(self.check({})["tested"], 1)

    def test_widespread_failure_is_a_provider_outage_without_a_plan(self):
        self.seed({A: 1, B: 1, C: 1, D: 1})
        result = self.check({proxy_key(A): "x", proxy_key(B): "x", proxy_key(C): "x"})
        self.assertTrue(result["outage"])
        self.assertIsNone(result["plan"])

    def test_all_402_failures_are_a_provider_outage(self):
        self.assertTrue(reassign_module.is_provider_outage(10, [{"error": "CONNECT tunnel failed, response 402"}] * 3))
        self.assertFalse(reassign_module.is_provider_outage(10, [{"error": "CONNECT tunnel failed, response 402"}] * 2))
        self.assertFalse(reassign_module.is_provider_outage(10, [{"error": "timed out"}] * 3))
        self.assertFalse(reassign_module.is_provider_outage(1, [{"error": "x"}]))


class MaxAccountsConfigTests(unittest.TestCase):
    def test_defaults_and_bounds(self):
        store = ConfigStore.__new__(ConfigStore)
        for raw, expected in (({}, 4), ({"proxy_max_accounts": "6"}, 6), ({"proxy_max_accounts": 0}, 1), ({"proxy_max_accounts": "x"}, 4)):
            store.data = raw
            with self.subTest(raw=raw):
                self.assertEqual(store.proxy_max_accounts, expected)


if __name__ == "__main__":
    unittest.main()
