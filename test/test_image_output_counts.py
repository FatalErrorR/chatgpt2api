import json
import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path

from services.log_service import LogService

CST = timezone(timedelta(hours=8))


class ImageOutputCountsTest(unittest.TestCase):
    def test_counts_successful_images_in_beijing_windows(self) -> None:
        now = datetime(2026, 9, 23, 15, 0, tzinfo=CST)
        server_tz = datetime.now().astimezone().tzinfo or timezone.utc

        def stamp(moment: datetime) -> str:
            return moment.astimezone(server_tz).strftime("%Y-%m-%d %H:%M:%S")

        lines = [
            _call(stamp(datetime(2026, 9, 23, 1, 0, tzinfo=CST)), "/v1/images/edits", ["a", "b"]),
            _call(stamp(datetime(2026, 9, 22, 23, 0, tzinfo=CST)), "/v1/images/generations", ["c"]),
            _call(stamp(datetime(2026, 9, 10, 12, 0, tzinfo=CST)), "/v1/images/edits", ["d"]),
            _call(stamp(datetime(2026, 8, 1, 12, 0, tzinfo=CST)), "/v1/images/edits", ["old"]),
            _call(stamp(datetime(2026, 9, 23, 2, 0, tzinfo=CST)), "/v1/images/edits", ["x"], status="failed"),
            _call(stamp(datetime(2026, 9, 23, 3, 0, tzinfo=CST)), "/v1/images/generations", []),
            json.dumps({"type": "account", "time": stamp(now), "detail": {}}, ensure_ascii=False),
        ]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "logs.jsonl"
            path.write_text("\n".join(lines) + "\n", encoding="utf-8")
            counts = LogService(path).image_output_counts(now)

        self.assertEqual(counts["edits"], {"today": 2, "yesterday": 0, "d7": 2, "d14": 3, "d30": 3})
        self.assertEqual(counts["generations"], {"today": 0, "yesterday": 1, "d7": 1, "d14": 1, "d30": 1})


def _call(time: str, endpoint: str, urls: list[str], status: str = "success") -> str:
    return json.dumps(
        {
            "type": "call",
            "time": time,
            "detail": {"endpoint": endpoint, "status": status, "urls": urls},
        },
        ensure_ascii=False,
        separators=(",", ":"),
    )


if __name__ == "__main__":
    unittest.main()
