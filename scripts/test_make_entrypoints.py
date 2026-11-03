#!/usr/bin/env python3
"""Verify both supported Make targets reach the current native Git gate."""
import os
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
TARGETS = ('test', 'test-gitcache-backends')


class MakeEntrypointTests(unittest.TestCase):
    def setUp(self):
        if shutil.which('make') is None:
            self.skipTest('make is not installed; Make entrypoint checks require make')
        self.env = {key: value for key, value in os.environ.items()
                    if key not in ('MAKEFLAGS', 'MFLAGS', 'MAKEOVERRIDES', 'MAKELEVEL')}

    def test_supported_targets_invoke_existing_native_git_gate(self):
        for target in TARGETS:
            with self.subTest(target=target):
                result = subprocess.run(['make', '--no-print-directory', '-n', target],
                                        cwd=ROOT, env=self.env, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                commands = [shlex.split(line) for line in result.stdout.splitlines()
                            if './hack/test-gitcache' in line]
                self.assertEqual(len(commands), 1, result.stdout)
                script = ROOT / commands[0][1]
                self.assertTrue(script.is_file(), f'Make invokes missing script: {script}')
                self.assertTrue(os.access(script, os.X_OK), f'Not executable: {script}')
                self.assertEqual(commands[0], ['GOWORK=off', './hack/test-gitcache.sh'])
                if target == 'test':
                    self.assertIn('go test -race -count=1 ./...', result.stdout)

    def test_native_git_gate_failure_reaches_make(self):
        # Record the recipe at the Make/SHELL boundary without running the Go
        # suite. The real native gate is separately run as a focused check.
        with tempfile.TemporaryDirectory() as directory:
            shell = Path(directory) / 'shell'
            log = Path(directory) / 'calls'
            shell.write_text('#!/bin/sh\nprintf \'%s\\n\' "$@" > "$OBERTH_MAKE_FIXTURE_LOG"\nexit 17\n')
            shell.chmod(0o755)
            result = subprocess.run([
                'make', '--no-print-directory', 'test-gitcache-backends', f'SHELL={shell}',
            ], cwd=ROOT, env=dict(self.env, OBERTH_MAKE_FIXTURE_LOG=str(log)),
                capture_output=True, text=True)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('Error 17', result.stderr)
            self.assertIn('GOWORK=off ./hack/test-gitcache.sh', log.read_text())


class MakeAvailabilityTests(unittest.TestCase):
    def test_missing_make_reports_skip(self):
        # Use an empty search path so this also works on development hosts
        # that normally have make; invoke Python by its absolute path.
        with tempfile.TemporaryDirectory() as directory:
            result = subprocess.run([
                sys.executable, '-I', '-B', str(Path(__file__).resolve()),
                'MakeEntrypointTests', '-v',
            ], cwd=ROOT, env=dict(os.environ, PATH=directory),
                capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('skipped', result.stderr)
        self.assertIn('make is not installed', result.stderr)
        self.assertIn('skipped=2', result.stderr)


if __name__ == '__main__':
    unittest.main()
