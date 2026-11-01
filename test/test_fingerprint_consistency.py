"""Restored pre-2026-10-15 browser identity: chrome110 TLS with Edge 143 headers."""

from __future__ import annotations

import unittest
from unittest import mock

from services import openai_backend_api as backend
from services.openai_backend_api import OpenAIBackendAPI


class FingerprintConsistencyTests(unittest.TestCase):
    def setUp(self):
        self.api = OpenAIBackendAPI("")
        self.addCleanup(self.api.close)
        self.headers = self.api.session.headers

    def test_default_identity_is_the_pre_chrome146_edge_profile(self):
        self.assertEqual(self.api.fp["impersonate"], "chrome110")
        self.assertIn("Edg/143.0.0.0", self.headers["User-Agent"])
        self.assertIn("Microsoft Edge", self.headers["Sec-Ch-Ua"])
        self.assertEqual(self.headers["Sec-Ch-Ua-Full-Version"], '"143.0.3650.96"')
        self.assertEqual(self.headers["Sec-Ch-Ua-Platform"], '"Windows"')

    def test_a_stored_fingerprint_is_kept(self):
        with self._cached_account({"fp": {"impersonate": "edge101", "user-agent": "custom-ua"}}):
            api = OpenAIBackendAPI("test-token")
            self.addCleanup(api.close)
            self.assertEqual(api.fp["impersonate"], "edge101")
            self.assertEqual(api.fp["user-agent"], "custom-ua")

    class _cached_account:  # noqa: N801 - small helper, used as a context manager
        def __init__(self, account: dict):
            self.account = account

        def __enter__(self):
            self._patch = mock.patch.object(backend.account_service, "get_account", return_value=self.account)
            self._patch.start()

        def __exit__(self, *exc):
            self._patch.stop()


if __name__ == "__main__":
    unittest.main()
