#!/usr/bin/env python3
"""Exercise the tracked-file gate using inert temporary Git repositories."""
import hashlib
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
        # Synthetic labels exercise the same policy without publishing real ones.
        self.brand = 'retiredbrand'
        self.instance = 'oldnode'
        digest = lambda value: hashlib.sha256(value.encode()).hexdigest()
        policy = patch.multiple(gate, create=True,
                                BRAND_DIGESTS={len(self.brand): {digest(self.brand)}},
                                INSTANCE_DIGESTS={len(self.instance): {digest(self.instance)}})
        policy.start()
        self.addCleanup(policy.stop)

    def git(self, *args):
        subprocess.run(['git', *args], cwd=self.root, env=self.env, check=True, capture_output=True)

    def write(self, name, body, tracked=True):
        (self.root / name).write_bytes(body)
        if tracked:
            self.git('add', '--', name)

    def test_generic_tracked_files_and_untracked_noise_pass(self):
        self.write('fixture.txt', b'exampleorg/example-api')
        self.write('scratch.txt', self.brand.encode(), tracked=False)
        self.assertEqual(gate.check(self.root), 0)

    def test_mixed_case_substrings_and_binary_contents_fail(self):
        for body in (self.brand.upper().encode(),
                     b'prefix-' + self.brand.title().encode() + b'-suffix',
                     b'\0' + self.brand.encode(),
                     b'\0' + b'.'.join((self.brand[:7].encode(), self.brand[7:].encode()))):
            with self.subTest(body=body):
                self.write('fixture.txt', body)
                self.assertEqual(gate.check(self.root), 1)

    def test_separator_spellings_in_contents_fail(self):
        for separator in ('.', '_', '-', ' ', '..', '._-'):
            spelling = separator.join((self.brand[:7], self.brand[7:])).upper()
            with self.subTest(separator=separator):
                self.write('fixture.txt', b'prefix-' + spelling.encode() + b'-suffix')
                self.assertEqual(gate.check(self.root), 1)

    def test_split_brand_inside_longer_words_fails(self):
        self.write('example.txt', ('prefix' + self.brand[:7] + '-' +
                                   self.brand[7:] + 'suffix').encode())
        self.assertEqual(gate.check(self.root), 1)

    def test_separator_spelling_in_filename_fails(self):
        spelling = '_'.join((self.brand[:7], self.brand[7:]))
        self.write(spelling + '.txt', b'generic content')
        self.assertEqual(gate.check(self.root), 1)

    def test_tracked_filename_fails(self):
        self.write(self.brand.upper() + '.txt', b'generic content')
        self.assertEqual(gate.check(self.root), 1)

    def test_worktree_edit_is_checked(self):
        self.write('fixture.txt', b'generic content')
        self.write('fixture.txt', self.brand.encode(), tracked=False)
        self.assertEqual(gate.check(self.root), 1)

    def test_seeded_checkout_with_different_owner_is_scoped_safe(self):
        self.write('fixture.txt', b'generic content')
        with patch.dict(os.environ, dict(self.env, GIT_TEST_ASSUME_DIFFERENT_OWNER='1'), clear=True):
            refused = subprocess.run(['git', 'ls-files'], cwd=self.root, capture_output=True)
            self.assertEqual(refused.returncode, 128)
            self.assertIn(b'dubious ownership', refused.stderr)
            self.assertEqual(gate.check(self.root), 0)

    def test_installation_labels_in_paths_text_and_binary_fail(self):
        for name, body in (('example.txt', self.instance.encode()),
                           ('example.txt', b'prefix-' + self.instance.upper().encode()),
                           ('example.txt', b'\0' + self.instance.encode()),
                           (self.instance + '.txt', b'generic content'),
                           ('LICENSE', self.instance.encode())):
            with self.subTest(name=name, body=body):
                self.write(name, body)
                self.assertEqual(gate.check(self.root), 1)
                self.git('rm', '-f', '--', name)

    def test_encoded_and_split_labels_fail(self):
        for label in (self.brand, self.instance):
            escaped = ''.join('\\x%02x' % ord(c) for c in label)
            unicode_escaped = ''.join('\\u%04x' % ord(c) for c in label)
            for spelling in (escaped, unicode_escaped,
                             label[:3] + escaped[12:],
                             repr(label[:3]) + ' + ' + repr(label[3:]),
                             repr(label[:3]) + ', ' + repr(label[3:]),
                             '"' + label[:3] + '"\n + "' + label[3:] + '"',
                             label[:3] + '-' + label[3:],
                             repr(label) + ', ' + repr('suffix'),
                             repr(label) + ' ' + repr('suffix')):
                with self.subTest(label=label, spelling=spelling):
                    self.write('example.txt', spelling.encode())
                    self.assertEqual(gate.check(self.root), 1)

    def test_encoded_path_fails(self):
        spelling = ''.join('\\x%02x' % ord(c) for c in self.instance)
        self.write(spelling + '.txt', b'generic content')
        self.assertEqual(gate.check(self.root), 1)

    def test_official_distribution_and_longer_words_remain_allowed(self):
        self.write('example.txt', ('europe-west4-docker.pkg.dev/example/oberth ' +
                                   self.instance + 'main').encode())
        self.assertEqual(gate.check(self.root), 0)

    def test_workstation_tool_paths_fail(self):
        home = b'/home/operator/'
        desktop = b'/Users/operator/'
        for body in (home + b'go/pkg/mod/example',
                     home + b'.local/share/kubebuilder-envtest/v1',
                     home + b'work/project', desktop + b'work/project'):
            with self.subTest(body=body):
                self.write('example.txt', body)
                self.assertEqual(gate.check(self.root), 1)

    def test_neutral_paths_and_deployment_variables_pass(self):
        self.write('example.txt', b'/home/user/.ssh/id_ed25519 $GOMODCACHE $KUBEBUILDER_ASSETS')
        self.assertEqual(gate.check(self.root), 0)

    def test_missing_git_metadata_fails_closed(self):
        with tempfile.TemporaryDirectory() as directory, self.assertRaises(subprocess.CalledProcessError):
            gate.check(directory)


if __name__ == '__main__':
    unittest.main()
