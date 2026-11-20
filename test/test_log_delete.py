import json
import tempfile
import unittest
from pathlib import Path
from unittest import mock

from services.log_service import LogService


class LogDeleteTest(unittest.TestCase):
    def setUp(self) -> None:
        self._dir = tempfile.TemporaryDirectory()
        self.path = Path(self._dir.name) / "logs.jsonl"
        self.service = LogService(self.path)

    def tearDown(self) -> None:
        self._dir.cleanup()

    def _ids(self) -> list[str]:
        return [json.loads(line).get("id", "") for line in self.path.read_text(encoding="utf-8").splitlines()]

    def test_removes_only_the_given_ids_and_keeps_lines_verbatim(self) -> None:
        for n in range(5):
            self.service.add("call", f"line {n}", {"n": n})
        before = self.path.read_text(encoding="utf-8").splitlines()
        ids = self._ids()

        self.assertEqual(self.service.delete([ids[1], ids[3], "missing"]), {"removed": 2})

        self.assertEqual(self.path.read_text(encoding="utf-8").splitlines(), [before[0], before[2], before[4]])
        self.assertFalse(self.path.with_name("logs.jsonl.tmp").exists())

    def test_no_match_leaves_the_file_untouched(self) -> None:
        self.service.add("call", "keep")
        inode = self.path.stat().st_ino
        self.assertEqual(self.service.delete(["nope"]), {"removed": 0})
        self.assertEqual(self.path.stat().st_ino, inode)
        self.assertFalse(self.path.with_name("logs.jsonl.tmp").exists())

    def test_deletes_legacy_line_by_the_id_list_shows(self) -> None:
        legacy = json.dumps({"time": "2026-01-01 00:00:00", "type": "call", "summary": "old"})
        self.path.write_text(legacy + "\n", encoding="utf-8")
        self.service.add("call", "new")
        shown = [item["id"] for item in self.service.list()]
        legacy_id = next(i for i in shown if i not in self._ids())

        self.assertEqual(self.service.delete([legacy_id]), {"removed": 1})
        self.assertEqual([item["summary"] for item in self.service.list()], ["new"])

    def test_lines_appended_during_the_scan_survive(self) -> None:
        for n in range(3):
            self.service.add("call", f"line {n}")
        target = self._ids()[0]
        original = LogService._line_has_id
        appended = []

        def add_while_scanning(service, raw, ids):
            if not appended:
                appended.append(True)
                service.add("call", "written mid-delete")
            return original(service, raw, ids)

        with mock.patch.object(LogService, "_line_has_id", add_while_scanning):
            self.assertEqual(self.service.delete([target]), {"removed": 1})

        summaries = [item["summary"] for item in self.service.list()]
        self.assertEqual(summaries, ["written mid-delete", "line 2", "line 1"])


if __name__ == "__main__":
    unittest.main()
