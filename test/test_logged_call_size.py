from __future__ import annotations

import unittest
from unittest import mock

from services import log_service as log_module


class LoggedCallSizeTests(unittest.TestCase):
    def _detail(self, **kwargs) -> dict:
        call = log_module.LoggedCall({"id": "k", "name": "n", "role": "admin"}, "/v1/images/edits", "gpt-image-2", "图生图", **kwargs)
        with mock.patch.object(log_module.log_service, "add") as add:
            call.log("调用完成")
        return add.call_args.args[2]

    def test_requested_size_is_logged(self):
        self.assertEqual(self._detail(size="1024x1024")["size"], "1024x1024")

    def test_missing_size_is_not_logged(self):
        for size in ("", "  "):
            self.assertNotIn("size", self._detail(size=size))


if __name__ == "__main__":
    unittest.main()
