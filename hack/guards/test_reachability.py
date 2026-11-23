"""Exercise complete production-package reachability with the pinned analyzer."""
import json
from pathlib import Path
import subprocess
from unittest.mock import patch

from test_run import GUARDS, RepositoryFixture


class ReachabilityGuards(RepositoryFixture):
    @classmethod
    def setUpClass(cls):
        result = GUARDS.analyzer("-help")
        if result.returncode:
            raise RuntimeError("deadcode tool unavailable; check hack/tools/deadcode.mod and its module cache:\n"+result.stdout)
        super().setUpClass()

    def fixture(self):
        self.module = "example.invalid/unit"
        self.policy = {"version": 2, "product_command": "cmd/oberth",
                       "production_commands": ["cmd/oberth", "cmd/release", "cmd/absent"],
                       "components": {"C1": ["cmd/**", "internal/**"]},
                       "allow": {"C1": ["internal/**"]}, "packages": {}, "rules": [],
                       "harness": {"trees": ["tools/**"]}}
        self.write("go.mod", f"module {self.module}\n\ngo 1.26.0\n")
        self.write("docs/plans/fixture.md", "# Fixture\n")
        self.write("docs/plans/requirements.json", '{"requirements":[]}')
        self.write("hack/guards/messages.json", (GUARDS.ROOT/"hack/guards/messages.json").read_text())
        self.source("cmd/oberth/main.go", "package main\nfunc main() {}\n")
        self.source("cmd/release/main.go", 'package main\nimport "'+self.module+'/internal/release"\n'
                    'func main() { release.Run() }\n')
        self.source("internal/release/release.go", "package release\nfunc Run() { private() }\nfunc private() {}\n")

    def source(self, name, body):
        self.write(name, "// Package fixture follows its contract.\n// Spec: docs/plans/fixture.md#fixture\n"+body)

    def analyze(self):
        self.write("docs/plans/dependencies.json", json.dumps(self.policy))
        result = subprocess.run([self.binary, self.module], input=self.git("ls-files", "-z"),
                                text=True, capture_output=True, timeout=30)
        self.assertIn(result.returncode, (0, 1), result.stderr)
        report = json.loads(result.stdout)
        self.assertEqual(self.policy["product_command"], report["Product"])
        return report, GUARDS.deadcode(self.module, report)

    def test_unimported_owned_package_is_one_finding_even_with_no_functions(self):
        self.fixture()
        self.source("internal/orphan/value.go", "package orphan\ntype Value string\n")
        self.source("internal/orphan/other.go", "package orphan\nconst Name = 1\n")
        self.source("internal/release/testonly.go", "package release\nfunc TestOnly() {}\n")
        self.write("internal/release/release_test.go", 'package release\nimport "testing"\n'
                   'func TestRelease(t *testing.T) { TestOnly() }\n')
        report, rows = self.analyze()
        self.assertEqual({self.module+"/cmd/oberth", self.module+"/cmd/release"}, set(report["Roots"]))
        reach = [row for row in rows if "[REACH]" in row]
        self.assertEqual(1, len(reach), rows)
        self.assertTrue(reach[0].startswith("internal/orphan:1: [REACH]"), reach)
        self.assertEqual(["TestOnly"], [r.rsplit("Detail: ", 1)[-1] for r in rows if "[DEADCODE]" in r])
        self.assertFalse(any("[GO]" in row for row in rows), rows)

    def test_removing_release_reports_its_private_package_and_command(self):
        self.fixture()
        _, before = self.analyze()
        self.assertFalse(any(GUARDS.RULE.search(row) for row in before), before)
        self.policy["production_commands"].remove("cmd/release")
        _, rows = self.analyze()
        self.assertEqual({"cmd/release", "internal/release"},
                         {row.split(":1:", 1)[0] for row in rows if "[REACH]" in row})
        self.assertEqual(1, sum("[DEP-ROOT]" in row for row in rows), rows)

    def test_test_support_and_harness_packages_are_exempt(self):
        self.fixture()
        for path in ("internal/release/fake", "internal/release/contracttest", "internal/release/runtest",
                     "internal/kit", "tools/fixture"):
            self.source(path+"/value.go", "package fixture\ntype Value string\n")
        self.policy["packages"]["internal/kit"] = {"owner": "C1", "kind": "test_only", "consumers": []}
        report, rows = self.analyze()
        self.assertEqual({self.module+"/cmd/oberth", self.module+"/cmd/release", self.module+"/internal/release"},
                         set(report["Packages"] or []))
        self.assertFalse(any(GUARDS.RULE.search(row) for row in rows), rows)

    def test_shared_package_with_no_component_pattern_is_not_silent(self):
        self.fixture()
        self.policy["components"]["C1"] = ["cmd/**", "internal/release/**"]
        self.policy["shared"] = ["internal/value"]
        self.policy["packages"]["internal/value"] = {"owner": "C1", "kind": "shared", "consumers": []}
        self.source("internal/value/value.go", "package value\ntype Value string\n")
        _, rows = self.analyze()
        self.assertEqual(["internal/value"], [row.split(":1:", 1)[0] for row in rows if "[REACH]" in row])

    def test_unowned_package_is_an_ownership_finding_not_a_product_package(self):
        self.fixture()
        self.source("outside/value.go", "package outside\ntype Value string\n")
        report, rows = self.analyze()
        self.assertNotIn(self.module+"/outside", report["Packages"] or [])
        self.assertTrue(any("[OWNER]" in r for r in rows), rows)
        self.assertFalse(any("[REACH]" in r for r in rows), rows)

    def test_plan_can_mark_release_as_product_without_a_server(self):
        self.fixture()
        self.git("rm", "-f", "cmd/oberth/main.go")
        self.policy["product_command"] = "cmd/release"
        _, rows = self.analyze()
        self.assertFalse(any(GUARDS.RULE.search(r) or "reachability: skipped" in r for r in rows), rows)

    def test_module_root_library_uses_its_canonical_import_path(self):
        self.fixture()
        self.policy["shared"] = ["."]
        self.policy["allow"]["C1"].append(".")
        self.policy["packages"]["."] = {"owner": "C1", "kind": "shared", "consumers": ["C1"]}
        self.source("value.go", "package value\ntype Value string\n")
        report, rows = self.analyze()
        self.assertIn(self.module, report["Packages"] or [])
        self.assertEqual(["."], [r.split(":1:", 1)[0] for r in rows if "[REACH]" in r])
        self.source("cmd/oberth/main.go", 'package main\nimport _ "'+self.module+'"\nfunc main() {}\n')
        _, rows = self.analyze()
        self.assertFalse(any(GUARDS.RULE.search(r) for r in rows), rows)

    def test_absent_product_keeps_tests_and_unlisted_main_detection(self):
        self.fixture()
        self.git("rm", "-f", "cmd/oberth/main.go")
        self.source("cmd/extra/main.go", "package main\nfunc main() {}\n")
        self.source("cmd/extra/other.go", "package main\nconst Other = 1\n")
        with patch.object(GUARDS, "command", wraps=GUARDS.command) as tool:
            _, rows = self.analyze()
        self.assertEqual(1, tool.call_count)
        self.assertIn("-test=true", tool.call_args.args[0])
        self.assertEqual(1, rows.count("reachability: skipped, cmd/oberth is not built yet"), rows)
        self.assertEqual(1, sum("[DEP-ROOT]" in row for row in rows), rows)

    def test_invalid_product_shapes_are_findings_instead_of_skips(self):
        cases = {
            "library": ("cmd/oberth/main.go", "package library\ntype Value string\n"),
            "test only": ("cmd/oberth/main_test.go", 'package main\nimport "testing"\nfunc TestShape(t *testing.T) {}\n'),
            "build tag": ("cmd/oberth/main.go", "//go:build ignore\n\npackage main\nfunc main() {}\n"),
            "missing main": ("cmd/oberth/main.go", "package main\ntype Value string\n"),
        }
        for name, (file, body) in cases.items():
            with self.subTest(name=name):
                self.fixture()
                self.git("rm", "-f", "cmd/oberth/main.go")
                self.write(file, body)
                _, rows = self.analyze()
                self.assertTrue(any("[PRODUCT]" in r or "[GO]" in r for r in rows), rows)
                self.assertFalse(any(r.startswith("reachability: skipped") for r in rows), rows)
                self.git("rm", "-f", file)

    def test_product_directory_symlink_is_a_finding(self):
        self.fixture()
        self.git("rm", "-f", "cmd/oberth/main.go")
        self.source("tools/fixture/main.go", "package main\nfunc main() {}\n")
        Path("cmd/oberth").symlink_to("../tools/fixture", target_is_directory=True)
        self.git("add", "cmd/oberth")
        _, rows = self.analyze()
        self.assertEqual(1, sum("[PRODUCT]" in r for r in rows), rows)
        self.assertFalse(any(r.startswith("reachability: skipped") for r in rows), rows)

    def test_product_source_symlink_is_a_finding(self):
        self.fixture()
        self.git("rm", "-f", "cmd/oberth/main.go")
        self.source("tools/fixture/main.go", "package main\nfunc main() {}\n")
        Path("cmd/oberth").mkdir()
        Path("cmd/oberth/main.go").symlink_to("../../tools/fixture/main.go")
        self.git("add", "cmd/oberth/main.go")
        _, rows = self.analyze()
        self.assertEqual(1, sum("[PRODUCT]" in r for r in rows), rows)

    def test_dangling_product_symlink_is_not_absence(self):
        self.fixture()
        self.git("rm", "-f", "cmd/oberth/main.go")
        Path("cmd/oberth").symlink_to("missing-target", target_is_directory=True)
        self.git("add", "cmd/oberth")
        _, rows = self.analyze()
        self.assertEqual(1, sum("[PRODUCT]" in r for r in rows), rows)
        self.assertFalse(any("reachability: skipped" in r for r in rows), rows)

    def test_ignored_main_cannot_turn_a_library_into_the_product(self):
        self.fixture()
        self.write("cmd/oberth/main.go", "//go:build ignore\n\npackage main\nfunc main() {}\n")
        self.source("cmd/oberth/value.go", "package library\ntype Value string\n")
        _, rows = self.analyze()
        self.assertEqual(1, sum("[GO]" in r for r in rows), rows)
        self.assertFalse(any("reachability: skipped" in r for r in rows), rows)

    def test_analyzer_failure_is_reported_once(self):
        self.fixture()
        self.source("internal/broken/broken.go", "package broken\nfunc Broken() { missing() }\n")
        _, rows = self.analyze()
        failures = [r for r in rows if "[GO]" in r]
        self.assertEqual(1, len(failures), rows)
        self.assertIn("missing", failures[0])

    def test_failed_ast_analysis_is_printed_once_by_the_complete_checker(self):
        failure = "broken.go:1: [GO] fixture analysis failed"
        report = {"Lines": [failure], "Product": "", "Roots": [], "Packages": []}
        self.write(".golangci.yml", "keywords: [NOTE]\n")
        with patch.multiple(GUARDS, analysis=lambda: ("module", report), pins=lambda: [],
                            vocabulary=lambda: [], names=lambda: [], hits=lambda *args: [],
                            text_paths=lambda *args: ["."],
                            command=lambda *args: subprocess.CompletedProcess([], 0, stdout="")):
            self.assertEqual([failure], GUARDS.checks())

    def test_both_analyzer_failure_branches_are_findings(self):
        for mode in ("true", "false"):
            with self.subTest(mode=mode):
                self.fixture()
                original = GUARDS.command
                def command(args, data=None):
                    if "-test="+mode in args:
                        return subprocess.CompletedProcess(args, 1, stdout="analyzer fixture failed\n")
                    return original(args, data)
                with patch.object(GUARDS, "command", side_effect=command):
                    _, rows = self.analyze()
                self.assertEqual(1, sum("[GO]" in r for r in rows), rows)
                self.assertTrue(any("analyzer fixture failed" in r for r in rows), rows)

    def test_build_and_closure_failures_are_findings(self):
        for stage in ("build", "list"):
            with self.subTest(stage=stage):
                self.fixture()
                original = GUARDS.command
                def command(args, data=None):
                    if args[:2] == ["go", stage]:
                        return subprocess.CompletedProcess(args, 1, stdout="fixture command failed\n")
                    return original(args, data)
                with patch.object(GUARDS, "command", side_effect=command):
                    _, rows = self.analyze()
                self.assertEqual(1, sum("[GO]" in r for r in rows), rows)
                self.assertTrue(any("fixture command failed" in r for r in rows), rows)
