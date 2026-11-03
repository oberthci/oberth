#!/usr/bin/env python3
"""Exercise the address gate in temporary Git repositories."""
import contextlib
import importlib.util
import io
import os
from pathlib import Path
import subprocess
import tempfile
import unittest


spec = importlib.util.spec_from_file_location(
    'address_check', Path(__file__).with_name('check-private-addresses.py'))
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)


class AddressCheckTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.env = dict(os.environ, GIT_CONFIG_GLOBAL='/dev/null', GIT_CONFIG_NOSYSTEM='1')
        self.git('init', '-q')

    def git(self, *args):
        subprocess.run(['git', *args], cwd=self.root, env=self.env,
                       check=True, capture_output=True)

    def write(self, name, body, tracked=True):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(body)
        if tracked:
            self.git('add', '--', name)

    def check(self):
        with contextlib.redirect_stderr(io.StringIO()):
            return gate.check(self.root)

    def test_documentation_ranges_and_untracked_noise_pass(self):
        self.write('fixture.txt', b'192.0.2.193 198.51.100.1 203.0.113.5')
        self.write('scratch.txt', gate.PREFIX + b'7.8', tracked=False)
        self.assertEqual(self.check(), 0)

    def test_private_boundaries_and_binary_contents_fail(self):
        for suffix in (b'0.0', b'255.255', b'7.8'):
            for wrapper in (b'%s', b'http://%s:6443', b'\0%s\0', b'%s/24'):
                with self.subTest(suffix=suffix, wrapper=wrapper):
                    self.write('fixture.txt', wrapper % (gate.PREFIX + suffix))
                    self.assertEqual(self.check(), 1)

    def test_private_tracked_filename_fails(self):
        self.write((gate.PREFIX + b'7.8').decode() + '.txt', b'generic')
        self.assertEqual(self.check(), 1)

    def test_worktree_edits_are_checked(self):
        self.write('fixture.txt', b'generic')
        self.write('fixture.txt', gate.PREFIX + b'7.8', tracked=False)
        self.assertEqual(self.check(), 1)

    def test_only_exact_allowlisted_path_and_value_pass(self):
        for name, values in gate.ALLOWLIST.items():
            with self.subTest(name=name):
                self.write(name, b' '.join(values))
                self.assertEqual(self.check(), 0)
                self.write(name, b' '.join(values) + b' ' + gate.PREFIX + b'7.8')
                self.assertEqual(self.check(), 1)
                self.write(name, b'generic')

    def test_allowlisted_value_elsewhere_fails(self):
        for values in gate.ALLOWLIST.values():
            self.write('fixture.txt', b' '.join(values))
            self.assertEqual(self.check(), 1)

    def test_invalid_addresses_and_other_ranges_pass(self):
        self.write('fixture.txt', gate.PREFIX + b'256.1 ' + gate.PREFIX +
                   b'1.256 192.169.1.1 10.0.0.1 172.16.0.1')
        self.assertEqual(self.check(), 0)


if __name__ == '__main__':
    unittest.main()
