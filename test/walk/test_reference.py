"""Exact digest-bound reference exceptions never weaken candidate acceptance."""
from copy import deepcopy
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from walkassert import Failure
from walkreference import approved, signature, gap_changes


class ReferenceTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.expected = {"build", "readonly-source"}
        self.sha = "a" * 64
        self.rows = [
            {
                "name": "build",
                "status": "pass",
                "reason": "Assertion satisfied",
                "facts": {},
                "evidence_path": "build.json",
            },
            {
                "name": "readonly-source",
                "status": "fail",
                "reason": "Default-user source probe failed: main: Error (exit code 1)",
                "facts": {"probe_log": "AssertionError: source accepted a write: /work/src/.walk-write"},
                "evidence_path": "readonly-source.json",
            },
        ]
        self.rule = {
            "release": "v0.23.4",
            "cli_sha256": self.sha,
            "steps": {
                "readonly-source": {
                    "requirements": ["REQ-C6-009", "REQ-C6-010"],
                    "signatures": [signature(self.rows[1])],
                    "missing_remedies": [],
                }
            },
        }
        self.result = {
            "target": "published signed release v0.23.4",
            "cli_sha256": self.sha,
            "mode": "fresh",
            "host_busy": False,
            "steps": self.rows,
            "ux": {"missing_remedies": []},
        }
        self.baseline = {
            "reference": {"release": "v0.23.4", "cli_sha256": self.sha},
            "samples": {"fresh": [], "warm": []},
            "profiles": {"reference": {"fresh": None, "warm": None}},
        }
        (self.root / "golden-path.json").write_text(json.dumps(self.baseline))
        (self.root / "reference-gaps.json").write_text(json.dumps(self.rule))
        context = patch("walkreference.HERE", self.root)
        context.start()
        self.addCleanup(context.stop)

    def test_reference_accepts_exact_observed_signature(self):
        self.assertEqual(approved(self.result, self.expected), {"readonly-source"})

    def test_candidate_with_listed_gap_fails_without_reading_gap_file(self):
        self.result["cli_sha256"] = "b" * 64
        real = Path.read_text
        def read(path, *args, **kwargs):
            self.assertNotEqual(path.name, "reference-gaps.json")
            return real(path, *args, **kwargs)
        with patch.object(Path, "read_text", read), self.assertRaises(Failure):
            approved(self.result, self.expected)

    def test_green_candidate_never_reads_gap_file(self):
        self.result["cli_sha256"] = "b" * 64
        self.rows[1]["status"] = "pass"
        (self.root / "reference-gaps.json").unlink()
        self.assertEqual(approved(self.result, self.expected), set())

    def test_candidate_cannot_select_reference_using_a_flag_or_version_alone(self):
        self.result.update(cli_sha256="b" * 64, target_kind="reference")
        with self.assertRaises(Failure):
            approved(self.result, self.expected)

    def test_unlisted_reference_failure_is_refused(self):
        self.rows[0].update(status="fail", reason="wrong source")
        with self.assertRaisesRegex(Failure, "build"):
            approved(self.result, self.expected)

    def test_same_step_new_signature_or_status_is_refused(self):
        original = deepcopy(self.rows[1])
        for changed in (
            {"reason": "source unavailable"},
            {"facts": {"probe_log": "AssertionError: chmod failed"}},
            {"status": "blocked"},
        ):
            self.rows[1] = {**original, **changed}
            with self.subTest(changed=changed), self.assertRaises(Failure):
                approved(self.result, self.expected)

    def test_new_message_failure_is_refused(self):
        self.result["ux"]["missing_remedies"] = [{"step": "readonly-source", "message": "another failure"}]
        with self.assertRaisesRegex(Failure, "next-action"):
            approved(self.result, self.expected)

    def test_duplicate_or_missing_functional_evidence_is_refused(self):
        self.rows.append(deepcopy(self.rows[0]))
        with self.assertRaisesRegex(Failure, "complete functional evidence"):
            approved(self.result, self.expected)

    def test_gap_policy_only_shrinks(self):
        reduced = deepcopy(self.rule)
        reduced["steps"].clear()
        gap_changes(self.rule, reduced)
        grown = deepcopy(self.rule)
        grown["steps"]["readonly-source"]["signatures"].append({"reason": "another", "detail": ""})
        with self.assertRaises(Failure):
            gap_changes(self.rule, grown)
        added = deepcopy(self.rule)
        added["steps"]["another-step"] = deepcopy(added["steps"]["readonly-source"])
        with self.assertRaisesRegex(Failure, "steps may only shrink"):
            gap_changes(self.rule, added)

    def test_reference_identity_cannot_be_changed_to_reclassify_a_candidate(self):
        changed = {**self.rule, "cli_sha256": "b" * 64}
        with self.assertRaisesRegex(Failure, "identity cannot change"):
            gap_changes(self.rule, changed)

    def test_reference_signatures_qualify_both_owned_slot_ports_only(self):
        self.rows[1]["reason"] = "read https://localhost:34443 or ssh://localhost:34022"
        self.rule["steps"]["readonly-source"]["signatures"] = [signature(self.rows[1])]
        (self.root / "reference-gaps.json").write_text(json.dumps(self.rule))
        self.rows[1]["reason"] = "read https://localhost:35443 or ssh://localhost:35022"
        self.assertEqual(approved(self.result, self.expected), {"readonly-source"})
        self.rows[1]["reason"] = "read https://localhost:36443 or ssh://localhost:35022"
        with self.assertRaisesRegex(Failure, "Unapproved failure"):
            approved(self.result, self.expected)


if __name__ == "__main__":
    unittest.main()
