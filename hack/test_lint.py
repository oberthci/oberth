#!/usr/bin/env python3
"""Prove lint limits and refusals using isolated disposable modules."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


class LintRefusals(unittest.TestCase):
    def test_check_pins_config_when_another_format_is_present(self):
        root = Path(__file__).resolve().parents[1]
        with tempfile.TemporaryDirectory(dir=os.environ["GOTMPDIR"]) as directory:
            fixture = Path(directory)
            for name in (".golangci.yml", "hack/check.sh", "hack/steps.sh", "hack/lint-version"):
                target = fixture / name
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(root / name, target)
            (fixture / "go.mod").write_text("module lintprobe/config\n\ngo 1.26.0\n")
            (fixture / "bad.go").write_text("package main\n// " + "TO" + "DO: unfinished\nfunc main() {}\n")
            (fixture / ".golangci.json").write_text('{"version":"2","linters":{"default":"none","enable":["govet"]}}')
            result = subprocess.run(["bash", "hack/check.sh", "lint"], cwd=fixture,
                env={**os.environ, "GOLANGCI_LINT": os.environ.get("GOLANGCI_LINT", "golangci-lint")},
                text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False, timeout=60)
            self.assertEqual(1, result.returncode, result.stdout)
            self.assertIn("(godox)", result.stdout)

    def check_lint(self, linter, source, filename="bad.go", *, refused=True):
        root = Path(__file__).resolve().parents[1]
        scratch = Path(os.environ.get("GOTMPDIR", Path.home() / ".cache/go-tmp"))
        scratch.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(dir=scratch) as directory:
            fixture = Path(directory)
            (fixture / "go.mod").write_text(f"module lintprobe/{fixture.name}\n\ngo 1.26.0\n")
            (fixture / filename).write_text("package main\n" + source)
            result = subprocess.run([
                os.environ.get("GOLANGCI_LINT", "golangci-lint"), "run",
                "--config", str(root / ".golangci.yml"), "--path-mode", "abs",
                "--enable-only", linter, "./...",
            ], cwd=fixture, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                check=False)
            self.assertEqual(result.returncode, 1 if refused else 0, result.stdout)
            if refused:
                self.assertIn(f"({linter})", result.stdout)
                self.assertIn(str(fixture / filename), result.stdout)
            self.assertTrue((fixture / filename).is_file())
        self.assertFalse(fixture.exists())

    def test_budget_boundaries_are_allowed(self):
        cases = (
            ("revive", "// line\n" * 498 + "func main() {}\n"),
            ("funlen", "func main() {\nx := 0\n" + "x++\n" * 58 + "_ = x\n}\n"),
            ("gocyclo", "func main() {\nx := 0\n" + "if x > 0 { x-- }\n" * 11 + "}\n"),
            ("gocognit", "func main() {\nx := 0\n" + "if x > 0 {\n" * 5 + "x++\n" + "}\n" * 5
             + "if x > 0 { x-- }\n" * 5 + "}\n"),
            ("revive", "func main() {}\nfunc f(a, b, c, d int) int { return a+b+c+d }\n"),
        )
        for linter, source in cases:
            with self.subTest(linter=linter):
                self.check_lint(linter, source, refused=False)

    def test_test_functions_are_exempt_from_function_length(self):
        self.check_lint("funlen", 'import "testing"\nfunc TestTable(t *testing.T) {\n'
                        + 't.Log("row")\n' * 80 + '}\n', "bad_test.go", refused=False)

    def test_test_files_keep_other_budgets(self):
        self.check_lint("revive", "// line\n" * 499 + "func f() {}\n", "bad_test.go")
        self.check_lint("revive", "func f(a, b, c, d, e int) int { return a+b+c+d+e }\n", "bad_test.go")
        self.check_lint("gocyclo", "func f() {\nx := 0\n" + "if x > 0 { x-- }\n" * 12 + "}\n", "bad_test.go")
        self.check_lint("gocognit", "func f() {\nx := 0\n" + "if x > 0 {\n" * 6 + "x++\n" + "}\n" * 6 + "}\n", "bad_test.go")
        self.check_lint("gochecknoglobals", "var x = 1\n", "bad_test.go")

    def test_standard_error_handling_is_allowed(self):
        self.check_lint("errcheck", 'import ("fmt"; "io"; "os")\nfunc main() {}\n'
                        'func f(w io.Writer, file *os.File) { defer file.Close(); fmt.Fprintf(w, "%s", "ok") }\n',
                        refused=False)

    def test_unhandled_errors_are_refused(self):
        self.check_lint("errcheck", 'import "os"\nfunc main() { os.Chdir(".") }\n')

    def test_file_length_501_is_refused(self):
        self.check_lint("revive", "// line\n" * 499 + "func main() {}\n")

    def test_function_length_61_is_refused(self):
        self.check_lint("funlen", "func main() {\nx := 0\n" + "x++\n" * 59 + "_ = x\n}\n")

    def test_cyclomatic_complexity_13_is_refused(self):
        self.check_lint("gocyclo", "func main() {\nx := 0\n" + "if x > 0 { x-- }\n" * 12 + "}\n")

    def test_cognitive_complexity_21_is_refused(self):
        self.check_lint("gocognit", "func main() {\nx := 0\n" + "if x > 0 {\n" * 6 + "x++\n" + "}\n" * 6 + "}\n")

    def test_five_parameters_are_refused(self):
        self.check_lint("revive", "func main() {}\nfunc f(a, b, c, d, e int) int { return a+b+c+d+e }\n")

    def test_naked_return_is_refused(self):
        self.check_lint("nakedret", "func main() {}\nfunc f() (x int) { return }\n")

    def test_global_variable_is_refused(self):
        self.check_lint("gochecknoglobals", "var x = 1\nfunc main() {}\n")

    def test_sleep_is_refused(self):
        self.check_lint("forbidigo", 'import clock "time"\nfunc main() { clock.Sleep(1) }\n')

    def test_now_is_refused(self):
        self.check_lint("forbidigo", 'import clock "time"\nfunc main() { _ = clock.Now() }\n')

    def test_since_and_until_are_refused(self):
        for call in ("Since", "Until"):
            with self.subTest(call=call):
                self.check_lint("forbidigo", f'import clock "time"\nfunc main() {{ _ = clock.{call}(clock.Time{{}}) }}\n')

    def test_sleep_in_tests_is_refused(self):
        self.check_lint("forbidigo", 'import ("time"; "testing")\nfunc TestBad(t *testing.T) { time.Sleep(1) }\n', "bad_test.go")

    def test_error_wrapping_is_required(self):
        self.check_lint("errorlint", 'import "fmt"\nfunc main() {}\nfunc f(err error) error { return fmt.Errorf("failed: %v", err) }\n')

    def test_inherited_context_is_required(self):
        self.check_lint("contextcheck", 'import "context"\nfunc main() {}\nfunc f(ctx context.Context) { sink(context.Background()) }\nfunc sink(context.Context) {}\n')

    def test_unexplained_suppression_is_refused(self):
        self.check_lint("nolintlint", "//nolint:revive\nfunc main() {}\n")

    def test_leftover_is_refused(self):
        marker = "TO" + "DO"
        self.check_lint("godox", f"// {marker}: unfinished\nfunc main() {{}}\n")


    def test_all_unfinished_markers_are_refused(self):
        for word in ("TO" + "DO", "FIX" + "ME", "X" + "XX", "HA" + "CK"):
            with self.subTest(word=word):
                self.check_lint("godox", "// " + word + ": unfinished change\nfunc main() {}\n")


if __name__ == "__main__":
    os.environ.setdefault("GOTMPDIR", str(Path.home() / ".cache/go-tmp"))
    os.environ.setdefault("GOFLAGS", "-p=2 -buildvcs=false")
    os.environ.setdefault("GOMAXPROCS", "2")
    Path(os.environ["GOTMPDIR"]).mkdir(parents=True, exist_ok=True)
    if not shutil.which(os.environ.get("GOLANGCI_LINT", "golangci-lint")):
        raise SystemExit("pinned linter is required")
    unittest.main(verbosity=2)
