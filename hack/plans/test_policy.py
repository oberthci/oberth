"""Refuse misspelled policy fields, references and roles."""
import copy
import json
from pathlib import Path
import runpy
import unittest

SCHEMA = runpy.run_path(str(Path(__file__).with_name("policy.py")))
ROOT = Path(__file__).resolve().parents[2]

class PolicySchema(unittest.TestCase):
    def test_unknown_policy_vocabulary_reports_the_offending_key(self):
        original = json.loads((ROOT/"docs/plans/dependencies.json").read_text())
        SCHEMA["dependencies"](original)
        mutations = (
            ("effect", lambda d: d["rules"][3].update(effect="allow-if-listed")),
            ("deny_packages", lambda d: d.update(deny_packages=[])),
            ("@c3", lambda d: d["rules"][3]["from"].append("@c3")),
            ("kind", lambda d: d["packages"]["internal/model"].update(kind="adapter")),
            ("owner", lambda d: d["packages"]["internal/model"].update(owner="C99")),
            ("product_imports", lambda d: d["harness"].update(product_imports=[])),
            ("C99", lambda d: d["allow"].update(C99=[])),
            ("form", lambda d: d["rules"][0].update(form=[])),
            ("test_support", lambda d: d.update(test_support={"names":["fake","contracttest"], "suffix":"test"})),
        )
        for key, mutate in mutations:
            with self.subTest(key=key):
                data=copy.deepcopy(original); mutate(data)
                with self.assertRaisesRegex(AssertionError,key):
                    SCHEMA["dependencies"](data)

    def test_retirement_has_no_exception_schema_or_padded_tokens(self):
        original=json.loads((ROOT/"docs/plans/retired.json").read_text())
        for key,mutate in (
            ("exceptions",lambda d:d.update(exceptions=[])),
            ("tokens",lambda d:d["rules"][0]["tokens"].append(" padded ")),
            ("checks",lambda d:d["exemptions"][0]["checks"].append("unknown")),
        ):
            data=copy.deepcopy(original); mutate(data)
            with self.assertRaisesRegex(AssertionError,key):
                SCHEMA["retired"](data)

    def test_owner_approval_is_limited_to_exact_promote_command(self):
        approve = runpy.run_path(str(ROOT/"hack/plans/parity.py"))["approve_new"]
        approve("CLI commands", "oberth promote", "owner decision 53")
        for category, name in (("CLI commands", "oberth train"),
                               ("MCP tools", "promote"),
                               ("CLI commands", "oberth promote-many")):
            with self.subTest(category=category, name=name):
                with self.assertRaisesRegex(AssertionError, "outside approved scope"):
                    approve(category, name, "owner decision 53")

    def test_issue_import_approval_is_limited_to_the_manual_file_surface(self):
        approve = runpy.run_path(str(ROOT/"hack/plans/parity.py"))["approve_new"]
        for category, name in (("CLI commands", "oberth issue import"),
                               ("CLI flags", "--dry-run [importIssues]"),
                               ("MCP tools", "issue_import"),
                               ("MCP arguments", "issue_import.jsonl"),
                               ("MCP arguments", "issue_import.dry_run")):
            with self.subTest(category=category, name=name):
                approve(category, name, "owner decisions 115 and 120")
        for category, name in (("CLI commands", "oberth issue import-ci"),
                               ("CLI flags", "--overwrite [importIssues]"),
                               ("MCP arguments", "issue_import.kind")):
            with self.subTest(category=category, name=name):
                with self.assertRaisesRegex(AssertionError, "outside approved scope"):
                    approve(category, name, "owner decisions 115 and 120")

    def test_issue_upstream_approval_adds_only_the_explicit_mapping_input(self):
        approve = runpy.run_path(str(ROOT/"hack/plans/parity.py"))["approve_new"]
        for category, name in (("CLI flags", "--upstream [importIssues]"),
                               ("MCP arguments", "issue_import.upstream")):
            approve(category, name, "owner decision 126")
        for category, name in (("MCP tools", "issue_import_upstream"),
                               ("MCP arguments", "issue_import.guess_upstream"),
                               ("CLI flags", "--upstream [otherCommand]")):
            with self.subTest(category=category, name=name):
                with self.assertRaisesRegex(AssertionError, "outside approved scope"):
                    approve(category, name, "owner decision 126")

    def test_plugin_and_install_approvals_do_not_add_product_tools(self):
        approve = runpy.run_path(str(ROOT/"hack/plans/parity.py"))["approve_new"]
        for name in ("/oberth-runs", "/oberth-watch"):
            approve("Agent client commands", name, "owner decision 114")
        for name in ("pipeline-namespace", "ssh-node-port", "https-node-port"):
            approve("CLI flags", "--"+name+" [installationFlags]", "owner decision 115")
        for category, name, decision in (("MCP tools", "runs", "owner decision 114"),
                                         ("Agent client commands", "/oberth:runs", "owner decision 114"),
                                         ("CLI flags", "--uninstall [installationFlags]", "owner decision 115")):
            with self.subTest(category=category, name=name):
                with self.assertRaisesRegex(AssertionError, "outside approved scope"):
                    approve(category, name, decision)
