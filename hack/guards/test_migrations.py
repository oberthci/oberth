"""Require an actual passing migration coverage test in the release guard."""
import subprocess
from unittest.mock import patch

from test_run import GUARDS, RepositoryFixture


class MigrationGuards(RepositoryFixture):
    def check_output(self, output, status=0):
        self.write(".golangci.yml", "godox: {keywords: [UNFINISHED]}\n")
        def result(args):
            if "^TestMigrationReleaseCoverage$" in args:
                return subprocess.CompletedProcess(args, status, stdout=output)
            return subprocess.CompletedProcess(args, 0, stdout="")
        with patch.object(GUARDS, "command", side_effect=result) as tool, \
                patch.object(GUARDS, "analysis", return_value=("module", {})), \
                patch.object(GUARDS, "pins", return_value=[]), \
                patch.object(GUARDS, "vocabulary", return_value=[]), \
                patch.object(GUARDS, "names", return_value=[]), \
                patch.object(GUARDS, "hits", return_value=[]), \
                patch.object(GUARDS, "text_paths", return_value=[]), \
                patch.object(GUARDS, "deadcode", return_value=[]):
            rows = GUARDS.checks()
        calls = [call.args[0] for call in tool.call_args_list if "^TestMigrationReleaseCoverage$" in call.args[0]]
        self.assertEqual(1, len(calls))
        self.assertIn("./internal/store", calls[0])
        self.assertIn("-v", calls[0])
        return rows

    def test_full_guards_run_the_compiled_migration_check(self):
        rows = self.check_output("schema 7 requires a 6 to 7 step\n", 1)
        self.assertIn("[SCHEMA]", rows[0])
        self.assertIn("schema 7 requires", rows[0])

    def test_missing_renamed_and_skipped_tests_are_refused(self):
        for output in ["ok store [no tests to run]\n", "--- PASS: TestRenamedMigrationReleaseCoverage (0.01s)\n",
                       "--- SKIP: TestMigrationReleaseCoverage (0.00s)\n", "--- PASS: TestMigrationReleaseCoverage/child (0.01s)\n"]:
            with self.subTest(output=output):
                self.assertTrue(any("[SCHEMA]" in row for row in self.check_output(output)))

    def test_named_coverage_test_must_pass_with_successful_exit(self):
        output = "--- PASS: TestMigrationReleaseCoverage (0.01s)\nPASS\n"
        self.assertEqual([], self.check_output(output))
        self.assertTrue(any("[SCHEMA]" in row for row in self.check_output(output, 1)))
