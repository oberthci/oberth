"""Measured headless UX budgets and explicit remedies have negative controls."""
from copy import deepcopy
import json
from pathlib import Path
import tempfile
import unittest
from walkassert import HERE
from walkcommands import COMMANDS
from walkux import COUNTS, UX, baseline_changes
from walkevidence import redact


class UXTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.path = Path(temporary.name) / "baseline.json"
        self.now = 100.0
        self.blank = {"schema_version": 3, "profiles": {"reference": {"fresh": None, "warm": None}}}
        self.path.write_text(json.dumps(self.blank))
        self.ux = UX(self.path, "fresh", lambda: self.now)
        self.ux.phase = "first_green"

    def emit(self, step, argv, code, seconds=0):
        self.ux.command(
            {"step": step, "operation": "install", "argv": argv, "exit_code": code, "duration_seconds": seconds}
        )

    def measured(self):
        self.ux.mark("cluster_ready")
        self.ux.phase = "first_green"
        self.emit("install", ["oberth", "install", "--yes"], 0, 10)
        self.now += 10
        self.ux.mark("first_green")
        self.ux.phase = "first_release"
        self.emit("release", ["git", "push", "oberth", "tag"], 0, 10)
        self.now += 10
        self.ux.mark("first_release")
        value = deepcopy(self.blank)
        value["profiles"]["reference"]["fresh"] = {
            "release": "v0.23.4",
            "evidence": "green",
            **deepcopy(self.ux.completed),
        }
        return value

    def test_unmeasured_baseline_and_partial_milestones_never_pass(self):
        self.emit("install", ["oberth", "install"], 0)
        report = self.ux.report()
        self.assertEqual(report["status"], "incomplete")
        self.assertIsNone(report["milestones"]["first_green"]["seconds"])
        self.assertFalse(report["milestones"]["first_green"]["complete"])
        self.assertFalse(report["baseline_recorded"])

    def test_real_milestone_times_and_prefixes_are_frozen(self):
        baseline = self.measured()
        self.ux.baseline = baseline
        self.emit("release", ["late", "command"], 0)
        self.now += 100
        report = self.ux.report()
        self.assertEqual(report["status"], "pass")
        self.assertEqual(report["milestones"]["first_green"]["counts"]["commands"], 1)
        self.assertEqual(report["milestones"]["first_green"]["seconds"], 10)
        self.assertEqual(report["milestones"]["first_release"]["seconds"], 20)
        self.assertNotIn("prompts", report)
        self.assertNotIn("prompts", report["milestones"]["first_green"]["counts"])

    def test_time_limit_is_exactly_measured_baseline_plus_twenty_five_percent(self):
        baseline = self.measured()
        baseline["profiles"]["reference"]["fresh"]["first_green"]["seconds"] = 8
        baseline["profiles"]["reference"]["fresh"]["first_release"]["seconds"] = 16
        self.ux.baseline = baseline
        self.assertEqual(self.ux.report()["status"], "pass")
        self.ux.completed["first_green"]["seconds"] = 10.001
        self.assertIn("first_green.seconds", self.ux.report()["regressions"][0])
        self.assertEqual(self.ux.report()["status"], "fail")

    def test_unfinished_milestone_fails_when_its_elapsed_lower_bound_exceeds_the_time_limit(self):
        baseline = self.measured()
        self.path.write_text(json.dumps(baseline))
        self.now = 100.0
        observed = UX(self.path, "fresh", lambda: self.now)
        observed.mark("cluster_ready")
        observed.phase = "first_green"
        observed.command(
            {
                "step": "install",
                "operation": "install",
                "argv": ["oberth", "install"],
                "exit_code": 0,
                "duration_seconds": 13,
            }
        )
        report = observed.report()
        self.assertEqual(report["status"], "fail")
        self.assertIsNone(report["milestones"]["first_green"]["seconds"])
        self.assertEqual(report["milestones"]["first_green"]["elapsed_lower_bound_seconds"], 13)

    def test_every_count_increase_fails_against_a_measured_baseline(self):
        baseline = self.measured()
        self.ux.baseline = baseline
        for count in COUNTS:
            with self.subTest(count=count):
                snapshot = self.ux.completed["first_green"]["counts"]
                snapshot[count] += 1
                self.assertEqual(self.ux.report()["status"], "fail")
                snapshot[count] -= 1

    def test_distinct_full_arguments_are_not_retries(self):
        self.emit("fixture", ["ssh-keygen", "-f", "admin"], 1)
        self.emit("fixture", ["ssh-keygen", "-f", "agent"], 0)
        self.assertEqual(self.ux.commands[-1]["attempt"], 1)
        self.assertEqual(self.ux.snapshot("first_green")["counts"]["retry_steps"], 0)
        self.emit("fixture", ["ssh-keygen", "-f", "admin"], 0)
        self.assertEqual(self.ux.commands[-1]["attempt"], 2)
        self.assertEqual(self.ux.snapshot("first_green")["counts"]["retry_steps"], 1)

    def test_all_explicit_negative_refusal_expectations_are_gated(self):
        expectations = json.loads((HERE / "expectations.json").read_text())
        for name in ("invalid-yaml", "ungranted-secret", "readonly-source"):
            with self.subTest(name=name):
                self.assertIn(name, expectations)
                self.assertFalse(self.ux.expect(name, "run failed", expectations[name]))
        self.assertEqual(self.ux.report()["status"], "fail")
        self.assertEqual(len(self.ux.report()["missing_remedies"]), 3)

    def test_review_counterexamples_cannot_satisfy_any_step_remedy(self):
        expectations = json.loads((HERE / "expectations.json").read_text())
        for text in (
            "oberth: run failed",
            "Error: git push failed",
            "Error: use of closed network connection",
            "Error: see above",
        ):
            for name, expectation in expectations.items():
                with self.subTest(text=text, step=name):
                    self.assertFalse(self.ux.expect(name, text, expectation))

    def test_baseline_increase_requires_a_reason(self):
        baseline = self.measured()
        updated = json.loads(json.dumps(baseline))
        updated["profiles"]["reference"]["fresh"]["first_green"]["counts"]["commands"] += 1
        with self.assertRaisesRegex(ValueError, "recorded reason"):
            baseline_changes(baseline, updated)
        updated["profiles"]["reference"]["fresh"][
            "reason"
        ] = "Measured additional operator authorization, evidence green-2"
        baseline_changes(baseline, updated)

    def test_recorded_baseline_cannot_be_erased_to_bypass_the_increase_guard(self):
        baseline = self.measured()
        removed = {**baseline, "profiles": {"reference": {"fresh": None, "warm": None}}}
        with self.assertRaisesRegex(ValueError, "cannot be removed"):
            baseline_changes(baseline, removed)

    def test_missing_remedy_prevents_recording_even_when_both_runs_are_green(self):
        self.ux.baseline = self.measured()
        self.ux.expect(
            "ungranted-secret", "ungranted", {"required": ["grant request"], "description": "Request exact grant"}
        )
        self.assertEqual(self.ux.report()["status"], "fail")

    def test_exports_redact_both_command_arguments_and_product_messages(self):
        self.emit("install", ["oberth", "install", "private-dir/credential"], 1)
        self.ux.expect(
            "install", "Error: value-at-risk at private-dir", {"required": ["missing"], "description": "Correct it"}
        )
        output = json.dumps(self.ux.report(["value-at-risk"], ["private-dir"]))
        self.assertNotIn("value-at-risk", output)
        self.assertNotIn("private-dir", output)
        self.assertIn("<private>", output)
        self.assertNotIn("body", redact("-----BEGIN PRIVATE KEY-----body-----END PRIVATE KEY-----", [], []))

    def test_harness_and_verification_never_enter_product_counts_or_seconds(self):
        self.ux.mark("cluster_ready")
        for name, command in COMMANDS.items():
            if command.role == "product":
                continue
            self.ux.command({"step": "install", "operation": name, "argv": ["probe", "--many-flags"],
                             "exit_code": 0, "duration_seconds": 100})
        value = self.ux.snapshot("first_green", True)
        self.assertEqual(value["counts"]["commands"], 0)
        self.assertEqual(value["counts"]["flags"], 0)
        self.assertEqual(value["seconds"], 0)
        self.assertGreater(len(self.ux.commands), 0)

    def test_only_declared_harness_flags_are_excluded_from_user_effort(self):
        for operation in ("install", "upstream", "wait", "promotion"):
            self.ux.commands.clear()
            argv = ["oberth", operation, "--json", "--yes", "--context=owned"]
            if operation == "wait":
                argv += ["--until", "finished", "--timeout=60"]
            self.ux.command({"step": operation, "operation": operation, "argv": argv,
                             "exit_code": 0, "duration_seconds": 1})
            self.assertEqual(self.ux.commands[0]["flags"], ["--yes", "--context"])
            self.assertIn("--json", self.ux.commands[0]["harness_flags"])
        self.ux.command({"step": "repo", "operation": "repo", "argv": ["oberth", "repo", "--json"],
                         "exit_code": 0, "duration_seconds": 1})
        self.assertEqual(self.ux.commands[-1]["flags"], ["--json"])

    def test_candidate_uses_owner_target_instead_of_reference_command_ceiling(self):
        self.ux.baseline = self.measured()
        target = json.loads((HERE / "golden-path.json").read_text())["candidate_target"]
        self.ux.baseline["candidate_target"] = target
        for name in ("first_green", "first_release"):
            self.ux.baseline["profiles"]["reference"]["fresh"][name]["counts"]["commands"] = 100
            self.ux.baseline["profiles"]["reference"]["fresh"][name]["counts"]["flags"] = 100
            self.ux.completed[name]["counts"].update(target[name])
        self.assertEqual(self.ux.report(kind="candidate")["regressions"], [])
        for name, count in (("first_green", "commands"), ("first_green", "flags"), ("first_release", "commands")):
            self.ux.completed[name]["counts"][count] += 1
            with self.subTest(name=name, count=count):
                self.assertTrue(any(name + "." + count in item
                                    for item in self.ux.report(kind="candidate")["failures"]))
                self.assertEqual(self.ux.report(kind="reference")["regressions"], [])
            self.ux.completed[name]["counts"][count] -= 1

    def test_candidate_target_relaxation_requires_a_new_owner_reason(self):
        baseline = self.measured()
        baseline["candidate_target"] = json.loads((HERE / "golden-path.json").read_text())["candidate_target"]
        changed = deepcopy(baseline)
        changed["candidate_target"]["first_green"]["commands"] += 1
        with self.assertRaisesRegex(ValueError, "new owner reason"):
            baseline_changes(baseline, changed)


if __name__ == "__main__":
    unittest.main()
