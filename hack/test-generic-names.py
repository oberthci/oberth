#!/usr/bin/env python3
"""Exercise the tracked-file gate using inert temporary Git repositories."""
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('name_check', Path(__file__).with_name('check-generic-names.py'))
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)


class NameCheckTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.env = dict(os.environ, GIT_CONFIG_GLOBAL='/dev/null', GIT_CONFIG_NOSYSTEM='1')
        self.git('init', '-q')

    def git(self, *args):
        subprocess.run(['git', *args], cwd=self.root, env=self.env, check=True, capture_output=True)

    def write(self, name, body, tracked=True):
        (self.root / name).write_bytes(body)
        if tracked:
            self.git('add', '--', name)

    def test_generic_tracked_files_and_untracked_noise_pass(self):
        self.write('fixture.txt', b'exampleorg/example-api')
        self.write('scratch.txt', gate.FORBIDDEN.encode(), tracked=False)
        self.assertEqual(gate.check(self.root), 0)

    def test_mixed_case_substrings_and_binary_contents_fail(self):
        for body in (gate.FORBIDDEN.upper().encode(),
                     b'prefix-' + gate.FORBIDDEN.title().encode() + b'-suffix',
                     b'\0' + gate.FORBIDDEN.encode(),
                     b'\0' + b'.'.join((gate.FORBIDDEN[:5].encode(), gate.FORBIDDEN[5:].encode()))):
            with self.subTest(body=body):
                self.write('fixture.txt', body)
                self.assertEqual(gate.check(self.root), 1)

    def test_separator_spellings_in_contents_fail(self):
        for separator in ('.', '_', '-', ' ', '..', '._-'):
            spelling = separator.join((gate.FORBIDDEN[:5], gate.FORBIDDEN[5:])).upper()
            with self.subTest(separator=separator):
                self.write('fixture.txt', b'prefix-' + spelling.encode() + b'-suffix')
                self.assertEqual(gate.check(self.root), 1)

    def test_separator_spelling_in_filename_fails(self):
        spelling = '_'.join((gate.FORBIDDEN[:5], gate.FORBIDDEN[5:]))
        self.write(spelling + '.txt', b'generic content')
        self.assertEqual(gate.check(self.root), 1)

    def test_tracked_filename_fails(self):
        self.write(gate.FORBIDDEN.upper() + '.txt', b'generic content')
        self.assertEqual(gate.check(self.root), 1)

    def test_worktree_edit_is_checked(self):
        self.write('fixture.txt', b'generic content')
        self.write('fixture.txt', gate.FORBIDDEN.encode(), tracked=False)
        self.assertEqual(gate.check(self.root), 1)

    def test_runtime_assembled_literals_fail(self):
        first, second = gate.PARTS
        encoded = ''.join('\\x%02x' % ord(char) for char in first)
        for source in (
                'name = "".join((%r, %r))' % (first, second),
                'name = %r + %r' % (first, second),
                'name = "%s" "%s"' % (first, second),
                'name = "".join(("%s", "%s"))' % (encoded, second)):
            with self.subTest(source=source):
                self.write('fixture.py', source.encode())
                self.assertEqual(gate.check(self.root), 1)

    def test_seeded_checkout_with_different_owner_is_scoped_safe(self):
        self.write('fixture.txt', b'generic content')
        with patch.dict(os.environ, dict(self.env, GIT_TEST_ASSUME_DIFFERENT_OWNER='1'), clear=True):
            refused = subprocess.run(['git', 'ls-files'], cwd=self.root, capture_output=True)
            self.assertEqual(refused.returncode, 128)
            self.assertIn(b'dubious ownership', refused.stderr)
            self.assertEqual(gate.check(self.root), 0)

    def test_missing_git_metadata_fails_closed(self):
        with tempfile.TemporaryDirectory() as directory, self.assertRaises(subprocess.CalledProcessError):
            gate.check(directory)


if __name__ == '__main__':
    unittest.main()
