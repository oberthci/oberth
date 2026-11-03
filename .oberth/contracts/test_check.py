"""Negative fixtures prove each contract lint rejects new violations."""
import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import subprocess

SPEC = importlib.util.spec_from_file_location("contract_check", Path(__file__).with_name("check.py"))
CHECK = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CHECK)


class ContractGuards(unittest.TestCase):
    def scan(self, name, source, module=None):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            if module:
                (root / "go.mod").write_text("module " + module + "\n")
            p = root / name
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text(source)
            return CHECK.scan(root, [name], {"entries": {}}, [])

    def test_laws_and_structural_rules(self):
        cases = [
            ("a.go", "// MUST match another value", "law1-confession"),
            ("a.md", "falls back automatically", "law2-retired"),
            ("a.go", 'os.Getenv("NEW_MODE")', "law3-surface"),
            ("a.go", "// temporary workaround", "law4-unregistered"),
            ("a.md", "/home/example", "law5-residue"),
            ("handoffs/note.md", "note", "law5-path"),
            ("a_test.go", 'import "go/parser"', "rule6-test-parser"),
            ("a.go", 'import "unsafe"', "rule7-hygiene"),
            ("a.go", 'os.Getenv("APP_TEST_BYPASS")', "rule8-env-seam"),
            ("a_test.go", 'os.Getenv("RUN_LIVE")', "rule10-gate-setter"),
            ("a.go", "//nolint:gosec", "rule11-suppression"),
            (".golangci.yml", "  - G402", "rule11-security-exclusion"),
            ("a.go", "func huge() {\n" + "// x\n" * 201 + "}\n", "rule6-function-length"),
        ]
        for name, source, rule in cases:
            with self.subTest(rule=rule):
                self.assertIn(rule, [r for r, _, _ in self.scan(name, source)])

    def test_generator_and_specific_explanation(self):
        self.assertEqual(self.scan("a.go", "// MUST match generator tools/generate.go output\n//nolint:gosec -- bounded public input"), [])

    def test_generator_exception_requires_a_named_generator(self):
        self.assertIn('law1-confession', [rule for rule, _, _ in self.scan('a.go', '// MUST match generator output')])

    def test_baseline_does_not_allow_different_violation(self):
        old = CHECK.fingerprint([("law1", "a.go", "MUST match A")])
        new = CHECK.fingerprint([("law1", "a.go", "MUST match B")])
        self.assertTrue(new - old)

    def test_source_archive_without_git_metadata(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root/'a.go').write_text('package main')
            self.assertEqual(CHECK.files(root), ['a.go'])

    def test_git_ownership_is_scoped_to_source(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            (root / '.git').write_text('gitdir: unused')
            with patch.object(CHECK.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, b'a.go\0')) as run:
                self.assertEqual(CHECK.files(root), ['a.go'])
            command = run.call_args.args[0]
            self.assertEqual(command[:3], ['git', '-c', 'safe.directory=' + str(root)])

    def test_pane_residue_does_not_match_modulo(self):
        for source in ('n%10 == 0', 'n %10 == 0', 'n % 10 == 0', 'fmt.Printf("%10d", n)', 'width: 50%20'):
            with self.subTest(source=source):
                self.assertEqual(self.scan('a.go', source), [])
        for source in ('pane %12', 'target=%12', '"%12"', '%12'):
            with self.subTest(source=source):
                self.assertIn('law5-residue', [r for r, _, _ in self.scan('a.md', source)])

    def test_shared_namespace_rules_remain_enforced(self):
        retired = CHECK.PRODUCT_NAMESPACE + '-operator-certs'
        self.assertIn('law2-retired', [r for r, _, _ in self.scan('a.md', retired)])
        annotation = '"' + CHECK.PRODUCT_NAMESPACE + '.io/new-mode"'
        self.assertIn('law3-surface', [r for r, _, _ in self.scan('a.go', annotation)])
        self.assertIn('law3-surface', [r for r, _, _ in self.scan('a.go', '"oberth.ci/new-mode"', module='github.com/oberthci/oberth')])

    def test_expired_sunset_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "surface-registry.json").write_text('{"entries":{}}')
            (root / "deprecations.yaml").write_text('{"deprecations":[{"id":"old","since":"2020-01-01","until":"2020-01-02","owner":"maintainer","replacement":"remove"}]}')
            with self.assertRaisesRegex(ValueError, "expired"):
                CHECK.validate_registries(root)


if __name__ == "__main__":
    unittest.main()
