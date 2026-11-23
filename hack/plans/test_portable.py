"""Negative controls for a reference-free plan bundle."""
import json
import os
from pathlib import Path
import runpy
import tempfile
import unittest
from unittest import mock

import portable


PARITY = runpy.run_path(str(Path(__file__).with_name("parity.py")))


class PortableTests(unittest.TestCase):
    def test_go_runner_uses_tool_modfile_without_changing_product_environment(self):
        with mock.patch.dict(os.environ, {"OBERTH_TOOLS_MODFILE": "/reviewed/main.mod",
                                        "GOFLAGS": "-mod=vendor", "GOWORK": "/branch/go.work"}):
            with mock.patch.object(PARITY["subprocess"], "check_output", return_value="[]") as run:
                PARITY["run"]("go", "run", "-p=1", "scan.go")
            args, options = run.call_args
            self.assertEqual(["go", "run", "-modfile=/reviewed/main.mod", "-p=1", "scan.go"], args[0])
            self.assertEqual("off", options["env"]["GOWORK"])
            self.assertEqual("-mod=readonly -p=2 -buildvcs=false", options["env"]["GOFLAGS"])
            self.assertEqual("-mod=vendor", os.environ["GOFLAGS"])

    def test_standalone_check_never_invokes_git_or_go(self):
        with mock.patch.object(PARITY["subprocess"], "check_output", side_effect=AssertionError("external command")):
            result = portable.check(PARITY)
        self.assertEqual("portable", result["mode"])
        self.assertGreater(result["requirements"]["requirements"], 0)

    def parity_fixture(self, current="RETIRED", entry=None):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        plans = root / "docs/plans"
        plans.mkdir(parents=True)
        (plans / "PARITY.md").write_text(
            "## Store tables\n\n| queue_cap | C7 | Retired scheduling | " + current + " | migration | |\n")
        delta = {"rows": [entry] if entry else []}
        (plans / "dispositions.json").write_text(json.dumps({
            "baseline": {"commit": "a" * 40, "dispositions": {"Store tables": {"queue_cap": "RETIRED"}}},
            "deltas": [delta],
        }))
        return dict(PARITY, ROOT=root)

    def test_retired_row_cannot_become_keep_without_a_ruling(self):
        api = self.parity_fixture("KEEP")
        with self.assertRaisesRegex(AssertionError, "unruled parity disposition change"):
            portable.parity_contracts(api)

    def test_inherited_retirement_passes(self):
        self.assertEqual(0, portable.parity_contracts(self.parity_fixture())["ruled_changes"])

    def test_a_declared_ruling_can_change_disposition(self):
        entry = {"category": "Store tables", "name": "queue_cap", "disposition": "KEEP"}
        for item in (-1, 0, 1, 34, 35, 46, 47, 81, 1000):
            with self.subTest(item=item):
                entry["item"] = item
                self.assertEqual(1, portable.parity_contracts(self.parity_fixture("KEEP", entry))["ruled_changes"])
        del entry["item"]
        with self.assertRaisesRegex(AssertionError, "unruled parity disposition change"):
            portable.parity_contracts(self.parity_fixture("KEEP", entry))

    def test_ruling_item_must_be_an_integer(self):
        for item in (True, "81", 1.5):
            entry = {"category": "Store tables", "name": "queue_cap", "disposition": "KEEP", "item": item}
            with self.subTest(item=item), self.assertRaisesRegex(AssertionError, "invalid ruling item"):
                portable.parity_contracts(self.parity_fixture("KEEP", entry))

    def test_baseline_row_cannot_disappear(self):
        api = self.parity_fixture()
        (api["ROOT"] / "docs/plans/PARITY.md").write_text("")
        with self.assertRaisesRegex(AssertionError, "missing baseline inventory row"):
            portable.parity_contracts(api)

    def test_retirement_without_selector_fails_closed(self):
        data = json.loads((PARITY["ROOT"] / "docs/plans/retired.json").read_text())
        data["rules"][0]["tokens"] = []
        with mock.patch.object(portable.json, "loads", return_value=data):
            with self.assertRaisesRegex(AssertionError, "selectors missing"):
                portable.retired_contracts(PARITY["ROOT"])

    def test_bare_english_retirement_selector_is_rejected(self):
        data = json.loads((PARITY["ROOT"] / "docs/plans/retired.json").read_text())
        data["rules"][0]["tokens"] = ["workspace"]
        with mock.patch.object(portable.json, "loads", return_value=data):
            with self.assertRaisesRegex(AssertionError, "bare English"):
                portable.retired_contracts(PARITY["ROOT"])

    def test_bare_word_permission_is_exact_policy_data(self):
        data = json.loads((PARITY["ROOT"] / "docs/plans/retired.json").read_text())
        rule = next(rule for rule in data["rules"] if rule.get("bare_words"))
        rule["tokens"].append("unlistedword")
        with mock.patch.object(portable.json, "loads", return_value=data):
            with self.assertRaisesRegex(AssertionError, "bare English"):
                portable.retired_contracts(PARITY["ROOT"])

    def test_harness_is_excluded_from_production_graph(self):
        data = json.loads((PARITY["ROOT"] / "docs/plans/dependencies.json").read_text())
        graph = PARITY["intended_dependency_graph"](data, ["hack/tool", "test/parity"])
        self.assertNotIn("hack/tool", graph)
        self.assertNotIn("test/parity", graph)

    def test_small_check_never_reads_external_evidence(self):
        read_text, read_bytes = Path.read_text, Path.read_bytes
        forbidden = {"evidence.json", "requirements-evidence.json"}

        def text(path, *args, **kwargs):
            self.assertNotIn(path.name, forbidden)
            return read_text(path, *args, **kwargs)

        def binary(path, *args, **kwargs):
            self.assertNotIn(path.name, forbidden)
            return read_bytes(path, *args, **kwargs)

        with mock.patch.object(Path, "read_text", text), mock.patch.object(Path, "read_bytes", binary):
            self.assertEqual("portable", portable.check(PARITY)["mode"])


if __name__ == "__main__":
    unittest.main()
