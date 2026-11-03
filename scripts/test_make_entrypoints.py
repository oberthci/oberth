#!/usr/bin/env python3
"""Verify Make targets resolve and invoke existing repository scripts."""
import os
import re
from pathlib import Path
import shlex
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
TARGETS = ('test', 'test-gitcache', 'test-gitcache-backends')


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

    def test_every_declared_target_resolves_and_scripts_exist(self):
        # Parse target declarations, including .PHONY, then let make expand
        # variables and prerequisites. Dry runs never build, clean or install.
        source = (ROOT / 'Makefile').read_text()
        targets = set()
        for line in source.splitlines():
            if not line or line[0].isspace() or ':=' in line:
                continue
            match = re.match(r'^([A-Za-z0-9_. -]+):([^=]*)$', line)
            if match:
                names, prerequisites = match.groups()
                targets.update(prerequisites.split() if names == '.PHONY' else names.split())
        self.assertTrue(targets)
        for target in sorted(targets):
            with self.subTest(target=target):
                result = subprocess.run(['make', '--no-print-directory', '-n', target],
                                        cwd=ROOT, env=self.env, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assert_scripts_exist(result.stdout)

    def assert_scripts_exist(self, recipes):
        scripts = []
        for line in recipes.splitlines():
            for word in shlex.split(line, comments=True):
                if word.endswith(('.sh', '.py', '.awk')):
                    scripts.append(word)
                    self.assertTrue((ROOT / word).is_file(), f'Make invokes missing script: {word}')
        return scripts

    def test_script_check_detects_retired_backend_entrypoint(self):
        # Mutation control for the original regression, including quoted paths
        # and scripts outside hack/. Check every script, not just the native gate.
        for script in ('./hack/test-gitcache-backends.sh', './scripts/absent.py'):
            with self.subTest(script=script), self.assertRaises(AssertionError):
                self.assert_scripts_exist(f'GOWORK=off bash "{script}"')

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
        self.assertIn('skipped=4', result.stderr)


if __name__ == '__main__':
    unittest.main()
