#!/usr/bin/env python3
"""Adversarial source and tool substitution controls for the bootstrap."""
import hashlib
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

spec = importlib.util.spec_from_file_location('verifier', Path(__file__).resolve().parents[1] / '.oberth/verify-release-tools.py')
verifier = importlib.util.module_from_spec(spec)
spec.loader.exec_module(verifier)


class ToolTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name) / 'tools'
        (self.root / 'bin').mkdir(parents=True)
        lines = []
        for name in sorted(verifier.TOOLS):
            body = ('reviewed executable ' + name).encode()
            path = self.root / 'bin' / name
            path.write_bytes(body)
            path.chmod(0o755)
            lines.append(hashlib.sha256(body).hexdigest() + '  ' + name)
        self.manifest = ('\n'.join(lines) + '\n').encode()

    def verify(self):
        verifier.verify_tools(self.root, self.manifest)

    def test_reviewed_bytes_pass(self):
        self.verify()

    def test_changed_tool_fails(self):
        (self.root / 'bin/cosign').write_bytes(b'not reviewed')
        with self.assertRaises(verifier.VerificationError):
            self.verify()

    def test_nonexecutable_fails(self):
        (self.root / 'bin/cosign').chmod(0o600)
        with self.assertRaises(verifier.VerificationError):
            self.verify()

    def test_symlink_to_identical_tool_fails(self):
        path = self.root / 'bin/cosign'
        target = self.root / 'copy'
        path.rename(target)
        path.symlink_to(target)
        with self.assertRaises(OSError):
            self.verify()

    def test_parent_alias_fails(self):
        actual = self.root / 'actual'
        (self.root / 'bin').rename(actual)
        (self.root / 'bin').symlink_to(actual, target_is_directory=True)
        with self.assertRaises(verifier.VerificationError):
            self.verify()

    def test_fifo_fails_without_waiting_for_writer(self):
        path = self.root / 'bin/cosign'
        path.unlink()
        os.mkfifo(path)
        with self.assertRaises(verifier.VerificationError):
            self.verify()

    def test_pin_omission_duplicate_and_unknown_names_fail(self):
        original = self.manifest
        for malformed in (b'', original.split(b'\n', 1)[1],
                          original + original.splitlines()[0] + b'\n',
                          original.replace(b'  cosign', b'  git')):
            with self.subTest(manifest=malformed), self.assertRaises(verifier.VerificationError):
                self.manifest = malformed
                self.verify()


class SourceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for relative in verifier.SOURCE_FILES:
            path = self.root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text('reviewed ' + relative)
        self.git('init', '-q')
        self.git('add', '.')
        self.git('-c', 'user.name=Test', '-c', 'user.email=test@example.invalid', 'commit', '-qm', 'reviewed')
        self.sha = self.git('rev-parse', 'HEAD').strip().decode()
        self.git('tag', 'v1.2.3')

    def git(self, *args):
        return subprocess.run(['/usr/bin/git', '-C', str(self.root), *args], check=True,
                              capture_output=True, env={'PATH': '/usr/bin:/bin',
                                                       'HOME': str(self.root),
                                                       'GIT_CONFIG_NOSYSTEM': '1',
                                                       'GIT_CONFIG_GLOBAL': '/dev/null'}).stdout

    def test_exact_admitted_blobs_pass(self):
        verifier.verify_source(self.root, self.sha, 'v1.2.3')

    def test_dirty_source_with_assume_unchanged_still_fails(self):
        self.git('update-index', '--assume-unchanged', '.oberth/release.sh')
        (self.root / '.oberth/release.sh').write_text('attacker script')
        # Ordinary git diff trusts this bit; direct committed-blob comparison
        # must still detect substituted publisher code.
        self.git('diff', '--exit-code')
        with self.assertRaises(verifier.VerificationError):
            verifier.verify_source(self.root, self.sha, 'v1.2.3')

    def test_different_commit_and_tag_fail(self):
        for sha, tag in (('1' * 40, 'v1.2.3'), (self.sha, 'v9.9.9'),
                         ('short', 'v1.2.3'), (self.sha, '--help')):
            with self.subTest(sha=sha, tag=tag), self.assertRaises((verifier.VerificationError, subprocess.CalledProcessError)):
                verifier.verify_source(self.root, sha, tag)


if __name__ == '__main__':
    unittest.main()
