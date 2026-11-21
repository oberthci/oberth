"""Mechanical checks for the walk's maintained scope and baseline policy."""
import ast
from copy import deepcopy
import json
import subprocess
import sys
import unittest
from walkassert import Failure, HERE
from walklib import STEPS
from walkux import baseline_changes
from walkreference import gap_changes
from walkio import kubectl_read


def git_read(*args):
    return subprocess.run(["git", *args], cwd=HERE.parents[1], capture_output=True, text=True)


def policy_base(name):
    path = "test/walk/" + name
    result = git_read("show", "main:" + path)
    if result.returncode == 0:
        return json.loads(result.stdout)
    base = git_read("merge-base", "main", "HEAD")
    if base.returncode != 0:
        raise AssertionError("Policy comparison requires the main merge base")
    revision = base.stdout.strip()
    result = git_read("show", revision + ":" + path)
    if result.returncode == 0:
        print(name + ": main has no copy; comparing merge base " + revision)
        return json.loads(result.stdout)
    anchor = "reference-restart.json" if name == "reference-gaps.json" else "reference-gaps.json"
    history = git_read("log", "--reverse", "--format=%H", revision + "..HEAD", "--", "test/walk/" + anchor)
    for commit in history.stdout.splitlines():
        result = git_read("show", commit + ":" + path)
        if result.returncode == 0:
            print(name + ": main and merge base have no copy; comparing first " + anchor + " policy at " + commit)
            return json.loads(result.stdout)
    raise AssertionError("No committed policy exists after the merge base: " + name)


class PolicyTests(unittest.TestCase):
    def test_checked_in_baseline_does_not_increase_main_without_a_reason(self):
        current = json.loads((HERE / "golden-path.json").read_text())
        baseline_changes(policy_base("golden-path.json"), current)
        self.assertEqual(set(current["profiles"]["reference"]), {"fresh", "warm"})

    def test_walk_imports_only_standard_library_and_its_own_modules(self):
        local = {p.stem for p in HERE.glob("*.py")}
        for path in HERE.glob("*.py"):
            for node in ast.walk(ast.parse(path.read_text())):
                if isinstance(node, ast.Import):
                    names = [alias.name.split(".")[0] for alias in node.names]
                elif isinstance(node, ast.ImportFrom):
                    names = [(node.module or "").split(".")[0]]
                else:
                    continue
                self.assertTrue(all(n in local or n in sys.stdlib_module_names for n in names), (path.name, names))

    def test_one_step_table_has_unique_names_and_only_preceding_dependencies(self):
        preceding = set()
        for step in STEPS:
            self.assertNotIn(step.name, preceding)
            self.assertTrue(set(step.needs) <= preceding, step.name)
            preceding.add(step.name)

    def test_fixture_has_no_artificial_wait(self):
        for path in (HERE / "fixture").glob("*.sh"):
            self.assertNotIn("sleep ", path.read_text())

    def test_every_step_has_documented_proof_and_explicit_phase(self):
        readme = (HERE / "README.md").read_text()
        for step in STEPS:
            self.assertEqual(readme.count("| " + step.name + " |"), 1)
            self.assertIn(step.phase, ("", "first_green", "first_release", "checks"))
        self.assertTrue(
            all(not s.phase for s in STEPS if s.name in ("preflight", "target", "cluster", "cleanup", "ux-gate"))
        )

    def test_reference_gap_policy_only_shrinks_and_uses_requirement_ids(self):
        current = json.loads((HERE / "reference-gaps.json").read_text())
        gap_changes(policy_base("reference-gaps.json"), current)
        ids = {
            r["id"] for r in json.loads((HERE.parents[1] / "docs/plans/requirements.json").read_text())["requirements"]
        }
        for rule in current["steps"].values():
            self.assertTrue(rule["requirements"] and set(rule["requirements"]) <= ids)

    def test_step_modules_cannot_hide_user_commands_in_diagnostic_scripts(self):
        for name in ("walksetup.py", "walkruns.py"):
            for node in ast.walk(ast.parse((HERE / name).read_text())):
                if not isinstance(node, ast.Call) or not isinstance(node.func, ast.Attribute):
                    continue
                self.assertFalse(any(k.arg in ("user", "operation") for k in node.keywords), (name, node.lineno))
                if node.func.attr == "run" and node.args and isinstance(node.args[0], ast.List):
                    command = node.args[0].elts
                    if isinstance(command[0], ast.Constant):
                        self.assertIn(command[0].value, ("curl", "cosign", "k3d"), (name, node.lineno))
                    else:
                        self.assertTrue(
                            len(command) == 2 and isinstance(command[1], ast.Constant) and command[1].value == "version"
                        )
                if node.func.attr == "k":
                    prefix = []
                    for arg in node.args:
                        if not isinstance(arg, ast.Constant):
                            break
                        prefix.append(arg.value)
                    self.assertTrue(kubectl_read(prefix), (name, node.lineno))
                if node.func.attr == "http":
                    self.assertEqual(len(node.args), 1, (name, node.lineno))
                    self.assertFalse(node.keywords, (name, node.lineno))

    def test_published_reference_budget_is_derived_from_two_complete_samples(self):
        data = json.loads((HERE / "golden-path.json").read_text())
        for mode, samples in data["samples"].items():
            profile = data["profiles"]["reference"][mode]
            if len(samples) < 2:
                self.assertIsNone(profile)
                continue
            self.assertEqual(len(samples), 2)
            for name in ("first_green", "first_release"):
                self.assertEqual(profile[name], min((s[name] for s in samples), key=lambda m: m["seconds"]))
                self.assertTrue(profile[name]["complete"] and profile[name]["seconds"] is not None)

    def test_added_gap_fails_against_committed_policy_before_main_integration(self):
        previous = policy_base("reference-gaps.json")
        changed = deepcopy(previous)
        changed["steps"]["new-gap"] = deepcopy(next(iter(previous["steps"].values())))
        with self.assertRaisesRegex(Failure, "steps may only shrink"):
            gap_changes(previous, changed)

    def test_growth_fails_against_committed_budget_before_main_integration(self):
        previous = policy_base("golden-path.json")
        changed = deepcopy(previous)
        changed["profiles"]["reference"]["fresh"]["first_green"]["counts"]["commands"] += 1
        changed["profiles"]["reference"]["fresh"]["reason"] = ""
        with self.assertRaisesRegex(ValueError, "recorded reason"):
            baseline_changes(previous, changed)

if __name__ == "__main__":
    unittest.main()
