"""The measured restart exception is exact and cannot qualify a candidate."""
from copy import deepcopy
import json
from pathlib import Path
import unittest
from unittest.mock import patch
from walkassert import Failure, HERE
from walkreference import approved, signature


class RestartReferenceTests(unittest.TestCase):
    def setUp(self):
        self.record = json.loads((HERE / "reference-restart.json").read_text())
        self.row = deepcopy(self.record["step"])
        self.result = {"target": "published signed release " + self.record["release"],
                       "cli_sha256": self.record["cli_sha256"], "steps": [self.row],
                       "ux": {"missing_remedies": []}}

    def test_exact_measured_restart_failure_is_bound_to_requirement_and_digest(self):
        policy = json.loads((HERE / "reference-gaps.json").read_text())
        rule = policy["steps"]["server-restart"]
        self.assertEqual(rule["requirements"], ["REQ-C6-013"])
        self.assertEqual(policy["cli_sha256"], self.record["cli_sha256"])
        self.assertEqual(rule["signatures"], [signature(self.row)])
        detail = signature(self.row)["detail"]
        for text in ("init-artifact-oberth", "source transfer unavailable", "oberth-executor-token"):
            self.assertIn(text, detail)
        self.assertEqual(approved(self.result, {"server-restart"}), {"server-restart"})

    def test_candidate_with_the_restart_gap_fails_without_reading_reference_policy(self):
        self.result.update(cli_sha256="b" * 64, target_kind="reference")
        read = Path.read_text
        def guarded(path, *args, **kwargs):
            self.assertNotEqual(path.name, "reference-gaps.json")
            return read(path, *args, **kwargs)
        with patch.object(Path, "read_text", guarded), self.assertRaisesRegex(Failure, "server-restart"):
            approved(self.result, {"server-restart"})

    def test_different_restart_failure_text_or_container_is_refused(self):
        original = deepcopy(self.row)
        for old, new in (("source transfer unavailable", "source receipt invalid"),
                         ("init-artifact-oberth", "main")):
            self.row = deepcopy(original)
            node = next(iter(self.row["facts"]["native_nodes"].values()))
            node["message"] = node["message"].replace(old, new)
            self.result["steps"] = [self.row]
            with self.subTest(change=new), self.assertRaisesRegex(Failure, "server-restart"):
                approved(self.result, {"server-restart"})

    def test_restart_gap_requires_the_actual_in_outage_dependency_start(self):
        check = self.row["facts"]["source_checks"][0]
        check["waited_started_at"] = self.row["facts"]["outage_end"]
        self.row["facts"]["outage_starts"] = [check["uid"]]
        with self.assertRaisesRegex(Failure, "server-restart"):
            approved(self.result, {"server-restart"})

    def test_different_first_server_dependency_is_refused(self):
        self.row["facts"]["source_checks"][0]["waited_container"] = "init-artifact-oberth"
        with self.assertRaisesRegex(Failure, "server-restart"):
            approved(self.result, {"server-restart"})


if __name__ == "__main__":
    unittest.main()
