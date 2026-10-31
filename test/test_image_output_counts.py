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
            self.assertTrue((path.parent / "image_output_counts.json").exists())
            self.assertEqual(LogService(path).image_output_counts(now), counts)

    def test_new_success_updates_the_daily_file(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "logs.jsonl"
            service = LogService(path)
            service.add("call", "图生图调用完成", {"endpoint": "/v1/images/edits", "status": "success", "urls": ["u", "v"]})
            self.assertEqual(service.image_output_counts()["edits"]["today"], 2)
            service.add("call", "图生图调用完成", {"endpoint": "/v1/images/edits", "status": "success", "urls": ["w"]})
            service.add("call", "图生图调用失败", {"endpoint": "/v1/images/edits", "status": "failed", "urls": []})
            self.assertEqual(service.image_output_counts()["edits"]["today"], 3)

    def test_list_reads_the_newest_lines(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "logs.jsonl"
            lines = [json.dumps({"id": str(index), "time": f"2026-10-01 00:{index:02d}:00", "type": "call"}) for index in range(40)]
            path.write_text("\n".join(lines) + "\n", encoding="utf-8")
            service = LogService(path)
            items = list(service._reversed_lines(chunk_size=7))
            self.assertEqual([item["id"] for item in service.list(limit=3)], ["39", "38", "37"])
            self.assertEqual(json.loads(items[-1])["id"], "0")
            self.assertEqual(service.list(start_date="2026-10-02"), [])


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
