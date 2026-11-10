import tempfile
import unittest
from pathlib import Path
from unittest import mock

import api.proxies as proxies_module
from services.proxy_list_service import FAILURE_THRESHOLD, ProxyList, proxy_key

A = "http://user:pw@1.1.1.1:8000"
B = "socks5://2.2.2.2:1080"
C = "http://3.3.3.3:8000"


def result(status, error=None):
    return {"ok": 0 < status < 500, "status": status, "latency_ms": 12, "error": error}


class ProxyBatchTestTests(unittest.TestCase):
    """批量测试：并发测清单里的代理，按状态码把结果记进健康状况。"""

    def setUp(self):
        self.dir = tempfile.TemporaryDirectory()
        self.addCleanup(self.dir.cleanup)
        self.proxies = ProxyList(Path(self.dir.name) / "proxies.json")
        for url in (A, B, C):
            self.proxies.add(url)
        for target, value in (
            ("proxy_list", self.proxies),
            ("account_service", mock.Mock(proxy_account_counts=lambda: {A: 2})),
        ):
            patcher = mock.patch.object(proxies_module, target, value)
            patcher.start()
            self.addCleanup(patcher.stop)

    def run_test(self, outcomes, urls=()):
        with mock.patch.object(proxies_module, "test_proxy", side_effect=lambda url: outcomes[url]) as test:
            data = proxies_module._test_proxies(list(urls))
        return data, test

    def test_tests_every_proxy_and_returns_results_by_key(self):
        data, test = self.run_test({A: result(200), B: result(403), C: result(0, "curl: (56) response 402")})
        self.assertEqual(sorted(call.args[0] for call in test.call_args_list), sorted([A, B, C]))
        self.assertEqual(data["results"][proxy_key(B)]["status"], 403)
        self.assertEqual(len(data["items"]), 3)

    def test_only_2xx_or_3xx_counts_as_healthy(self):
        for _ in range(FAILURE_THRESHOLD):
            self.proxies.record_failure(A, "boom")
        outcomes = {A: result(200), B: result(403), C: result(0, "curl: (56) response 402")}
        for _ in range(FAILURE_THRESHOLD):
            data, _ = self.run_test(outcomes)
        health = {item["url"]: item["health"] for item in data["items"]}
        self.assertFalse(health[A]["abnormal"])  # 续费后测试通过即清掉异常
        self.assertTrue(health[B]["abnormal"])  # 403 挑战算失败
        self.assertTrue(health[C]["abnormal"])
        self.assertIn("402", health[C]["last_error"])

    def test_limits_to_requested_urls(self):
        data, test = self.run_test({C: result(200)}, urls=[C])
        test.assert_called_once_with(C)
        self.assertEqual(list(data["results"]), [proxy_key(C)])


if __name__ == "__main__":
    unittest.main()
