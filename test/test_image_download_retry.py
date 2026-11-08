import unittest
from unittest import mock

from curl_cffi.requests import Headers

from services.openai_backend_api import OpenAIBackendAPI
from utils.helper import UpstreamHTTPError

SERVER_BUSY = b'<?xml version="1.0" encoding="utf-8"?><Error><Code>ServerBusy</Code></Error>'


class _Response:
    def __init__(self, status_code, content=b"", headers=None):
        self.status_code = status_code
        self.content = content
        self.text = content.decode("utf-8", "replace")
        self.headers = Headers(headers or {})


class ImageDownloadRetryTests(unittest.TestCase):
    """图片存储（Azure Blob）偶发 503 ServerBusy：退避重试，不让已生成的图片白白丢掉。"""

    def download(self, *queue):
        api = OpenAIBackendAPI("")
        self.addCleanup(api.close)
        with (
            mock.patch.object(api.session, "get", side_effect=list(queue)) as get,
            mock.patch("services.openai_backend_api.time.sleep") as sleep,
        ):
            try:
                result = api.download_image_bytes(["https://files.example.com/img.png?sig=secret"])
            except Exception as exc:
                result = exc
        return result, [call.args[0] for call in sleep.call_args_list], get.call_count

    def test_server_busy_is_retried_with_backoff(self):
        result, waits, _ = self.download(_Response(503, SERVER_BUSY), _Response(503, SERVER_BUSY), _Response(200, b"png"))
        self.assertEqual(result, [b"png"])
        self.assertEqual(waits, [1.0, 2.0])

    def test_retry_after_is_honored_but_capped(self):
        result, waits, _ = self.download(
            _Response(503, SERVER_BUSY, {"Retry-After": "3"}),
            _Response(429, b"", {"Retry-After": "120"}),
            _Response(200, b"png"),
        )
        self.assertEqual(result, [b"png"])
        self.assertEqual(waits, [3.0, 10.0])

    def test_connection_errors_are_retried(self):
        result, waits, _ = self.download(OSError("curl: (56) Connection reset by peer"), _Response(200, b"png"))
        self.assertEqual(result, [b"png"])
        self.assertEqual(waits, [1.0])

    def test_gives_up_after_four_attempts_with_the_upstream_error(self):
        result, waits, calls = self.download(*[_Response(503, SERVER_BUSY) for _ in range(4)])
        self.assertIsInstance(result, UpstreamHTTPError)
        self.assertEqual(result.status_code, 503)
        self.assertEqual(waits, [1.0, 2.0, 4.0])
        self.assertEqual(calls, 4)

    def test_client_errors_are_not_retried(self):
        result, waits, calls = self.download(_Response(404, b"<Error><Code>BlobNotFound</Code></Error>"))
        self.assertIsInstance(result, UpstreamHTTPError)
        self.assertEqual(result.status_code, 404)
        self.assertEqual(waits, [])
        self.assertEqual(calls, 1)


if __name__ == "__main__":
    unittest.main()
