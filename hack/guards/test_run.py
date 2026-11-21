"""Exercise text guards against disposable tracked repositories."""
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import shutil
import unittest
from unittest.mock import patch

from hack.guards.test_binary import compiled_guard

SPEC = importlib.util.spec_from_file_location("guards", Path(__file__).with_name("run.py"))
GUARDS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GUARDS)


class RepositoryFixture(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.binary = compiled_guard()

    def setUp(self):
        scratch = Path(os.environ.get("TMPDIR", os.environ.get("GOTMPDIR", Path.home() / ".cache/go-tmp")))
        scratch.mkdir(parents=True, exist_ok=True)
        self.directory = tempfile.TemporaryDirectory(dir=scratch)
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        previous = Path.cwd()
        os.chdir(self.root)
        self.addCleanup(os.chdir, previous)
        self.git("init", "-q", "-b", "main")
        self.git("config", "user.name", "Test User")
        self.git("config", "user.email", "test@example.invalid")
        self.git("commit", "--allow-empty", "-qm", "Start fixture")
        self.paths = [".", *(f":!{p}" for p in ("docs/plans", "docs/releases", "docs/archive", "test/parity/oracle"))]

    def git(self, *args):
        return subprocess.check_output(["git", *args], stderr=subprocess.STDOUT).decode()

    def write(self, name, body):
        path = Path(name)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(body)
        self.git("add", name)

    def register(self, terms):
        self.write("docs/plans/retired.json", json.dumps({
            "version": 1, "rules": [{"id": "R19", "tokens": terms, "replacement": "step"}],
            "exemptions": [{"path": path + "/", "checks": ["retired_vocabulary", "leftovers"]}
                           for path in ("docs/plans", "docs/releases", "docs/archive", "test/parity/oracle")]}))


class TextGuards(RepositoryFixture):
    def test_all_review_spelling_escapes_with_exact_data(self):
        phase = "bu" + "rn"
        field = "Frag" + "ment"
        split_path = 'filepath.Join(".oberth", "ci.yaml")'
        split_env = '"OBERTH_" + "REPO"'
        terms = [phase, field, "failed_" + phase, "Failed" + phase.title(), phase + "s",
                 phase.title(), split_path, split_env]
        self.register(terms)
        cases = [f"phase: {phase}", f"- {phase}", f"type Spec struct{{ {field} string }}",
                 *terms[2:5], f"func {phase.title()}() {{}}", split_path, split_env]
        for index, body in enumerate(cases):
            self.write(f"case{index}.txt", body)
        hits = GUARDS.vocabulary()
        self.assertEqual(len(hits), len(cases), hits)
        self.assertTrue(all("[R19]" in hit and "Retired vocabulary" in hit for hit in hits))

    def test_default_relative_paths_and_exact_exemption_boundaries(self):
        term = "retired_fixture_token"
        self.register([term])
        for directory in ("docs/plans", "docs/releases", "docs/archive", "test/parity/oracle"):
            self.write(directory + "/quoted.txt", term)
        self.assertEqual([], GUARDS.vocabulary())
        self.write("test/parity/oracle-backup/bad.txt", term)
        self.assertEqual(1, len(GUARDS.vocabulary()))
        self.write("test/parity/README.md", term)
        self.assertEqual(2, len(GUARDS.vocabulary()))

    def test_missing_and_malformed_register(self):
        with self.assertRaises(FileNotFoundError):
            GUARDS.vocabulary()
        self.register([""])
        with self.assertRaises(ValueError):
            GUARDS.vocabulary()

    def test_names_use_only_the_external_policy(self):
        self.write("references.txt", "security@oberth.ci\nhttp://oberth.oberth.svc.cluster.local:8080\n"
                   "oberth.local\n10.42.0.0/16\n169.254.169.254\n/go/cache\n"
                   "ssh://git@forge.com/example\nno-more-sessions@openssh.com\n"
                   "git push --force-with-lease\nkubectl get pods --all-namespaces\n")
        with patch.dict(os.environ, {"OBERTH_GUARDS_NAMES_FILE": ""}):
            self.assertIn("[NAME-POLICY]", GUARDS.names()[0])
        with tempfile.NamedTemporaryFile(mode="w", dir=Path(self.directory.name).parent) as external:
            external.write("fixture-screen-name\n"); external.flush()
            with patch.dict(os.environ, {"OBERTH_GUARDS_NAMES_FILE": external.name}):
                self.assertEqual([], GUARDS.names())
                self.write("bad.txt", "fixture-screen-name")
                self.assertEqual(1, len(GUARDS.names()))
                self.git("commit", "-qm", "Remove fixture-screen-name from product text")
                self.assertEqual(2, len(GUARDS.names()))

    def test_names_reject_empty_and_blank_patterns_before_searching(self):
        with tempfile.NamedTemporaryFile(mode="w+", dir=Path(self.directory.name).parent) as external:
            for body in ("", " ", "\n", " \t\n", "example\n\n", "example\n \t\n"):
                with self.subTest(body=repr(body)):
                    external.seek(0); external.truncate(); external.write(body); external.flush()
                    with patch.dict(os.environ, {"OBERTH_GUARDS_NAMES_FILE": external.name}), \
                         patch.object(GUARDS, "grep", side_effect=AssertionError("must not search")):
                        rows = GUARDS.names()
                    self.assertEqual(1, len(rows))
                    self.assertIn("[NAME-POLICY]", rows[0])
                    self.assertIn("blank pattern", rows[0])

    def pin_fixture(self):
        self.write("docs/plans/dependencies.json", '{"shared":["internal/actor","internal/model"]}')
        self.write("hack/plans/policy.py", (GUARDS.ROOT/"hack/plans/policy.py").read_text())
        self.git("commit", "-qm", "Establish pin decisions")
        self.git("checkout", "-qb", "probe")

    def test_pin_changes_are_named_and_plan_data_commits_are_allowed(self):
        self.pin_fixture()
        self.write("docs/plans/dependencies.json", '{"shared":["internal/model","internal/auth"]}')
        self.git("commit", "-qm", "Decide pinned contracts")
        rows = GUARDS.pins()
        self.assertIn("GATE FILES CHANGED:", rows)
        self.assertIn("pinned contract added: internal/auth", rows)
        self.assertIn("pinned contract removed: internal/actor", rows)
        self.assertFalse(any(GUARDS.RULE.search(row) for row in rows))

    def test_history_from_before_the_plan_data_is_not_a_pin_change(self):
        self.git("checkout", "-qb", "probe")
        self.write("old.go", "package fixture")
        self.git("commit", "-qm", "Work that predates the plan data")
        self.git("checkout", "-q", "main")
        self.write("docs/plans/dependencies.json", '{"shared":["internal/actor"]}')
        self.write("hack/plans/policy.py", (GUARDS.ROOT/"hack/plans/policy.py").read_text())
        self.git("commit", "-qm", "Establish pin decisions")
        self.git("checkout", "-q", "probe")
        self.git("merge", "-q", "--no-edit", "main")
        rows = GUARDS.pins()
        self.assertFalse(any(GUARDS.RULE.search(row) for row in rows), rows)

    def test_pin_changes_cannot_share_a_commit_with_code_or_prose(self):
        self.pin_fixture()
        for path in ("internal/auth/auth.go", "docs/plans/README.md", "README.md"):
            with self.subTest(path=path):
                self.git("reset", "--hard", "main")
                self.write("docs/plans/dependencies.json", '{"shared":["internal/actor","internal/auth"]}')
                self.write(path, "fixture")
                self.git("commit", "-qm", "Mix decisions with implementation")
                rows = GUARDS.pins()
                self.assertTrue(any("[PIN-POLICY]" in row for row in rows), rows)
                self.assertIn("pinned contract added: internal/auth", "\n".join(rows))
                self.assertIn("pinned contract removed: internal/model", "\n".join(rows))

    def test_restoring_pins_does_not_hide_a_mixed_commit(self):
        self.pin_fixture()
        self.write("docs/plans/dependencies.json", '{"shared":["internal/actor"]}')
        self.write("bad.go", "package fixture")
        self.git("commit", "-qm", "Mix a pin removal with code")
        self.write("docs/plans/dependencies.json", '{"shared":["internal/actor","internal/model"]}')
        self.git("commit", "-qm", "Restore pinned contract")
        rows = GUARDS.pins()
        self.assertTrue(any("[PIN-POLICY]" in row for row in rows), rows)
        self.assertIn("pinned contract removed: internal/model", "\n".join(rows))
        self.assertIn("pinned contract added: internal/model", "\n".join(rows))

    def test_merging_an_isolated_pin_decision_with_code_is_allowed(self):
        self.pin_fixture()
        self.write("implementation.go", "package fixture")
        self.git("commit", "-qm", "Implement component")
        self.git("checkout", "-qb", "decisions", "main")
        self.write("docs/plans/dependencies.json", '{"shared":["internal/model"]}')
        self.git("commit", "-qm", "Decide contract pins")
        self.git("checkout", "-q", "probe")
        self.git("merge", "--no-ff", "-qm", "Apply pin decision", "decisions")
        rows = GUARDS.pins()
        self.assertIn("pinned contract removed: internal/actor", rows)
        self.assertFalse(any(GUARDS.RULE.search(row) for row in rows), rows)

    def test_gate_names_entry_reports_pins_even_before_source_checks(self):
        self.pin_fixture()
        self.write("docs/plans/dependencies.json", '{"shared":["internal/model"]}')
        self.git("commit", "-qm", "Remove a pinned contract")
        with tempfile.NamedTemporaryFile(mode="w", dir=Path(self.directory.name).parent) as external:
            external.write("unmatched-policy-fixture\n"); external.flush()
            result = subprocess.run(["python3", str(GUARDS.ROOT/"hack/guards/run.py"), "names", "HEAD"],
                                    text=True, capture_output=True,
                                    env={**os.environ, "OBERTH_GUARDS_NAMES_FILE": external.name})
        self.assertEqual(0, result.returncode, result.stdout+result.stderr)
        self.assertIn("GATE FILES CHANGED:\npinned contract removed: internal/actor", result.stdout)

    def test_policy_error_has_one_line_with_key_and_reason(self):
        for key, mutation in (("deny_packages", lambda d: d.update(deny_packages=[])),
                              ("rules.4.effect", lambda d: d["rules"][3].update(effect="typo"))):
            with self.subTest(key=key):
                shutil.copytree(GUARDS.ROOT/"docs/plans", self.root/"docs/plans", dirs_exist_ok=True)
                shutil.copytree(GUARDS.ROOT/"hack/plans", self.root/"hack/plans", dirs_exist_ok=True)
                path = self.root/"docs/plans/dependencies.json"
                data = json.loads(path.read_text()); mutation(data); path.write_text(json.dumps(data))
                result = subprocess.run(["python3", "hack/plans/parity.py", "--check"], text=True, capture_output=True)
                self.assertNotEqual(0, result.returncode)
                self.assertEqual(1, len((result.stdout+result.stderr).splitlines()))
                self.assertIn(key, result.stderr)
                self.assertNotIn("Traceback", result.stderr)
                rows = GUARDS.tool_output("POLICY", result)
                self.assertEqual(1, len(rows))

    def test_names_reject_internal_file_and_external_symlink_to_it(self):
        self.write("names.txt", "fixture-operator")
        expected = [GUARDS.finding("NAME-POLICY", detail="Names file must resolve outside the repository.")]
        with patch.dict(os.environ, {"OBERTH_GUARDS_NAMES_FILE": str(Path("names.txt").resolve())}):
            self.assertEqual(expected, GUARDS.names())
        with tempfile.TemporaryDirectory(dir=Path(self.directory.name).parent) as directory:
            link = Path(directory) / "names"
            link.symlink_to(Path("names.txt").resolve())
            with patch.dict(os.environ, {"OBERTH_GUARDS_NAMES_FILE": str(link)}):
                self.assertEqual(expected, GUARDS.names())

    def test_output_keeps_every_finding(self):
        lines = [f"bad.go:{i}: [DEP-CLOSED] forbidden import; review dependencies.json" for i in range(1, 13)]
        result=subprocess.CompletedProcess([],1,stdout="\n".join(lines))
        self.assertEqual(lines,GUARDS.tool_output("GO",result))

    def test_analysis_preserves_findings_when_go_run_reports_exit_status(self):
        report = {"Lines": ["bad.go:1: [REQ-WALK] remove the marker"], "Roots": None}
        results = [subprocess.CompletedProcess([], 0, stdout="bad.go\0"),
                   subprocess.CompletedProcess([], 0, stdout=""),
                   subprocess.CompletedProcess([], 0, stdout="module"),
                   subprocess.CompletedProcess([], 1, stdout=json.dumps(report)+"\nexit status 1\n")]
        with patch.dict(os.environ), patch.object(GUARDS, "command", side_effect=results):
            module, actual = GUARDS.analysis()
        self.assertEqual("module", module)
        self.assertEqual(report, actual)

    def test_deadcode_uses_its_own_module_from_a_fixture_directory(self):
        with patch.object(GUARDS, "command", return_value=subprocess.CompletedProcess([], 0, stdout="null")) as tool:
            self.assertEqual([], GUARDS.unreachable("module", "true", ["./..."]))
        tool.assert_called_once_with([
            "go", "tool", "-modfile="+str(GUARDS.ROOT/"hack/tools/deadcode.mod"), "deadcode",
            "-json", "-test=true", "-filter", "^module(/|$)", "./..."])

    def test_analyzer_and_linter_have_separate_tool_declarations(self):
        for name, tool in (("go.mod", "github.com/golangci/golangci-lint/v2/cmd/golangci-lint"),
                           ("deadcode.mod", "golang.org/x/tools/cmd/deadcode"),
                           ("crane.mod", "github.com/google/go-containerregistry/cmd/crane")):
            with self.subTest(modfile=name):
                raw = subprocess.check_output(["go", "mod", "edit", "-json",
                                               "-modfile="+str(GUARDS.ROOT/"hack/tools"/name)], cwd=GUARDS.ROOT)
                self.assertEqual([{"Path": tool}], json.loads(raw)["Tool"])

    def test_missing_deadcode_declaration_is_one_named_failure(self):
        self.write("go.mod", "module example.invalid/unit\n\ngo 1.26.0\n")
        self.write("hack/tools/deadcode.mod", "module example.invalid/tools\n\ngo 1.26.0\n")
        with patch.object(GUARDS, "ROOT", self.root):
            rows = GUARDS.deadcode("example.invalid/unit", {
                "Lines": [], "Product": "cmd/oberth", "Roots": ["example.invalid/unit/cmd/oberth"]})
        self.assertEqual(1, len(rows), rows)
        self.assertIn('[GO]', rows[0])
        self.assertIn('no such tool "deadcode"', rows[0])

    def test_deadcode_uses_the_product_closure_without_reclassifying_paths(self):
        calls = []
        def tool(args, data=None):
            calls.append(args)
            return subprocess.CompletedProcess(args, 0, stdout=json.dumps([
                {"Path": "module/internal/runlog/latest", "Funcs": [
                    {"Name": "Absent", "Position": {"File": "absent.go", "Line": 3}}]}]))
        with patch.object(GUARDS, "command", side_effect=tool):
            rows = GUARDS.deadcode("module", {"Lines": [], "Packages": [], "Product": "cmd/oberth", "Roots": ["module/cmd/oberth"]})
        self.assertEqual(1, len(rows))
        self.assertEqual(["module/cmd/oberth"], calls[-1][-1:])
        self.assertIn("-test=true", calls[0])
        self.assertEqual("./...", calls[0][-1])
        self.assertIn("-test=false", calls[-1])
        self.assertTrue(all(args[0] != "make" for args in calls))

    def test_deadcode_uses_the_marked_product_and_all_existing_roots(self):
        roots = ["module/cmd/launch", "module/cmd/oberth-release", "module/cmd/oberth-source-plugin"]
        with patch.object(GUARDS, "command", return_value=subprocess.CompletedProcess([], 0, stdout="[]")) as tool:
            self.assertEqual([], GUARDS.deadcode("module", {"Lines": [], "Packages": [], "Product": "cmd/launch", "Roots": roots}))
        self.assertEqual(4, tool.call_count)
        self.assertEqual(roots, tool.call_args.args[0][-3:])

    def test_deadcode_keeps_test_pass_until_the_marked_product_exists(self):
        for roots in (None, [], ["module/cmd/oberth-release", "module/cmd/oberth-store-backup"]):
            with self.subTest(roots=roots), patch.object(GUARDS, "command", return_value=subprocess.CompletedProcess([], 0, stdout="[]")) as tool:
                rows = GUARDS.deadcode("module", {"Lines": [], "Packages": [], "Product": "cmd/oberth", "Roots": roots})
                self.assertEqual(["reachability: skipped, cmd/oberth is not built yet"], rows)
                self.assertEqual(1, tool.call_count)
                self.assertIn("-test=true", tool.call_args.args[0])

    def test_deadcode_preserves_analysis_errors_when_product_metadata_is_unavailable(self):
        rows = ["bad.go:1: [GO] Source analysis failed."]
        with patch.object(GUARDS, "command") as tool:
            self.assertEqual(rows, GUARDS.deadcode("module", {"Product": "", "Roots": [], "Lines": rows}))
            tool.assert_not_called()

    def test_detail_mentioning_go_does_not_suppress_production_analysis(self):
        note = "walk result note mentions [GO]"
        report = {"Lines": [note], "Packages": [], "Product": "cmd/launch", "Roots": ["module/cmd/launch"]}
        with patch.object(GUARDS, "command", return_value=subprocess.CompletedProcess([], 0, stdout="[]")) as tool:
            self.assertEqual([note], GUARDS.deadcode("module", report))
        self.assertEqual(4, tool.call_count)

    def test_markers_and_empty_skips_have_failing_fixtures(self):
        marker = "TO" + "DO"
        for directory in ("docs/plans", "docs/releases", "docs/archive", "test/parity/oracle"):
            self.write(directory + "/quoted.txt", marker)
        args = ["-nIwE", marker, "--", *self.paths, ":!*.go"]
        self.assertEqual([], GUARDS.grep(args))
        self.write("unfinished.txt", marker)
        self.assertEqual(1, len(GUARDS.grep(args)))
        pattern = r'\.Skip(f|Now)?[[:space:]]*\([[:space:]]*(\)|"[[:space:]]*")'
        cases = ['t.Skip()', 't.SkipNow()', 't.Skipf("")', 't.Skip("   ")']
        self.write("cases_test.go", "\n".join(cases + ['t.Skip("platform prerequisite missing")']))
        self.assertEqual(4, len(GUARDS.grep(["-nIE", pattern, "--", "*_test.go"])))


    def test_array_indices_are_not_rule_ids(self):
        self.assertIsNone(GUARDS.RULE.search("bindings[0] is a source expression"))

    def test_tool_diagnostics_do_not_corrupt_successful_json(self):
        script = 'import sys; print("null"); print("module download", file=sys.stderr)'
        result = GUARDS.command(["python3", "-c", script])
        self.assertEqual(0, result.returncode)
        self.assertIsNone(json.loads(result.stdout))
        self.assertEqual("module download\n", result.stderr)
        failed = GUARDS.command(["python3", "-c", script+"; sys.exit(1)"])
        self.assertNotEqual(0, failed.returncode)
        self.assertIn("module download", GUARDS.tool_output("GO", failed)[0])

    def test_tool_failures_have_only_actionable_findings(self):
        row = "bad.go:7: [DEADCODE] absent function; remove it"
        result = subprocess.CompletedProcess([], 1, stdout=row + "\nexit status 1\n"
            "FAIL (1): helper\nmake[1]: *** [Makefile:13: deadcode] Error 1\n")
        self.assertTrue(GUARDS.tool_output("DEADCODE", result)[0].startswith(row))
        compiler = subprocess.CompletedProcess([], 1,
            stdout="bad.go:4:9: values[0].Missing is undefined\nexit status 1\n")
        rows = GUARDS.tool_output("GO", compiler)
        self.assertIn("[GO] Source analysis failed.", rows[0])
        history = subprocess.CompletedProcess([],1,stdout="commit/"+("a"*40)+"/message:3: [AUTHOR-ATTRIBUTION] attribution line")
        self.assertIn("[HISTORY] Commit identity or message is invalid.",GUARDS.tool_output("HISTORY",history)[0])

    def test_bare_tool_finding_has_no_empty_detail_field(self):
        row = GUARDS.finding("TOOL")
        self.assertEqual(".:1: [TOOL] Guard command failed. Fix the guard input and rerun the guards.", row)
        self.assertNotIn("Detail:", row)

    def test_non_names_tool_errors_keep_their_detail(self):
        self.write("hack/plans/parity.py", "")
        self.write("test/walk/walklib.py", "STEPS = generate_steps()\n")
        result = subprocess.run(["python3", str(GUARDS.ROOT/"hack/guards/run.py")],
                                capture_output=True, text=True, timeout=30)
        self.assertEqual(1, result.returncode)
        self.assertEqual("", result.stdout)
        self.assertEqual(GUARDS.finding("TOOL", detail="Walk needs one literal STEPS table.")+"\n", result.stderr)

    def test_guard_implementation_stays_within_the_line_budget(self):
        files = [p for p in (GUARDS.ROOT/"hack/guards").iterdir()
                 if p.suffix in {".go", ".py"} and not p.name.endswith("_test.go") and not p.name.startswith("test_")]
        self.assertLessEqual(sum(len(p.read_text().splitlines()) for p in files), 723)

    def test_message_catalogue_covers_long_details(self):
        source="\n".join(p.read_text() for p in Path(__file__).parent.glob("*.go") if not p.name.endswith("_test.go"))
        keys={key.rstrip("0123456789") for key in GUARDS.re.findall(r'"([A-Z][A-Z0-9-]*)"',source)}
        self.assertLessEqual(keys,set(GUARDS.MESSAGES))
        for rule, pair in GUARDS.MESSAGES.items():
            self.assertEqual(2,len(pair))
            for value,limit in zip(pair,(80,120)):
                self.assertLessEqual(len(value),limit,(rule,value))
                self.assertTrue(value.endswith("."),rule)
                self.assertNotIn("\\n",value)
            self.assertIn(pair[0],GUARDS.finding(rule,detail="x"*4000))

    def test_real_retired_register_agrees_with_portable_plans_check(self):
        root = Path(__file__).resolve().parents[2]
        subprocess.run(["python3", "hack/plans/parity.py", "--check"], cwd=root,
                       check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        os.chdir(root)
        try:
            data = json.loads(Path("docs/plans/retired.json").read_text())
            self.assertEqual(21, len(data["rules"]))
            hits = GUARDS.vocabulary()
            ids = {rule["id"] for rule in data["rules"]}
            self.assertTrue(all(GUARDS.RULE.search(hit)[1] in ids for hit in hits))
        finally:
            os.chdir(self.root)

    def test_exemptions_are_selected_by_check_from_register(self):
        term = "retired_fixture_token"
        self.register([term])
        data = json.loads(Path("docs/plans/retired.json").read_text())
        data["exemptions"].append({"path": "quoted/", "checks": ["leftovers"]})
        self.write("docs/plans/retired.json", json.dumps(data))
        self.write("quoted/record.txt", term)
        self.assertEqual(1, len(GUARDS.vocabulary()))
        self.assertIn(":!quoted", GUARDS.text_paths("leftovers"))
        self.assertNotIn(":!quoted", GUARDS.text_paths("retired_vocabulary"))

    def test_target_policy_refuses_a_branch_self_grant(self):
        approved=(GUARDS.ROOT/"docs/plans/dependencies.json").read_text()
        self.write("docs/plans/dependencies.json",approved)
        self.write("docs/plans/requirements.json",'{"requirements":[]}')
        self.write("docs/plans/fixture.md","# Fixture")
        self.write("hack/guards/messages.json",(GUARDS.ROOT/"hack/guards/messages.json").read_text())
        self.git("commit","-qm","Establish target policy")
        self.git("checkout","-qb","probe")
        data=json.loads(approved)
        data["allow"]["C2"].append("internal/api")
        data["rules"]=[r for r in data["rules"] if r["id"] not in (2,3)]
        self.write("docs/plans/dependencies.json",json.dumps(data))
        self.write("internal/service/doc.go",'// Package service follows a contract.\n// Spec: docs/plans/fixture.md#fixture\npackage service\nimport _ "example.invalid/unit/internal/api"\n')
        self.git("commit","-qm","Grant branch import")
        args=[self.binary,"example.invalid/unit"]
        own=subprocess.run(args,input=self.git("ls-files","-z"),text=True,capture_output=True,check=False)
        self.assertEqual(0,own.returncode,own.stdout)
        Path("docs/plans/dependencies.json").write_text(self.git("show","main:docs/plans/dependencies.json"))
        target=subprocess.run(args,input=self.git("ls-files","-z"),text=True,capture_output=True,check=False)
        self.assertEqual(1,target.returncode,target.stdout)
        self.assertIn("[DEP02]",target.stdout)

    def test_walk_table_is_read_without_executing_the_harness(self):
        self.write("test/walk/walklib.py",'raise RuntimeError("must not execute")\nSTEPS=(Step("push","Assert source",action),)\n')
        self.assertEqual("push",GUARDS.walk_steps("test/walk/walklib.py"))
        self.assertEqual("",GUARDS.walk_steps("other.py"))
