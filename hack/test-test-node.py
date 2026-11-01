#!/usr/bin/env python3
"""Exercise the test runtime's cache and failure paths without network or Node."""
import hashlib
import io
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest

LAUNCHER = Path(__file__).resolve().with_name('with-test-node.sh')


class TestNode(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        pins = self.root / '.oberth/pins'
        pins.mkdir(parents=True)
        fakebin = self.root / 'fakebin'
        fakebin.mkdir()
        self.archive = self.root / 'upstream.tar.gz'
        binary = b'#!/bin/sh\necho fixture-node\n'
        with tarfile.open(self.archive, 'w:gz') as archive:
            member = tarfile.TarInfo('node-v0.0.0-linux-x64/bin/node')
            member.size = len(binary)
            archive.addfile(member, io.BytesIO(binary))
        self.digest = hashlib.sha256(self.archive.read_bytes()).hexdigest()
        (pins / 'node.url').write_text('https://nodejs.org/dist/v0.0.0/node-v0.0.0-linux-x64.tar.gz\n')
        (pins / 'node.sha256').write_text(self.digest + '  unused\n')
        (pins / 'node-bin.sha256').write_text(hashlib.sha256(binary).hexdigest() + '  unused\n')
        self.calls = self.root / 'curl-calls'
        for name, body in {
            'uname': 'case "$1" in -s) echo Linux;; -m) echo x86_64;; esac',
            'curl': '''echo called >> "$TEST_CURL_CALLS"
test "${TEST_OFFLINE:-0}" = 0 || exit 99
while [ "$1" != --output ]; do shift; done
cp "$TEST_UPSTREAM" "$2"''',
        }.items():
            path = fakebin / name
            path.write_text('#!/bin/sh\nset -eu\n' + body + '\n')
            path.chmod(0o700)
        self.cache = self.root / 'cache/oberth-test-node' / (self.digest + '.tar.gz')
        self.env = dict(os.environ, PATH=str(fakebin) + os.pathsep + os.environ['PATH'],
                        GOCACHE=str(self.root / 'cache'), TEST_UPSTREAM=str(self.archive),
                        TEST_CURL_CALLS=str(self.calls))
        self.env.pop('OBERTH_TEST_NODE_ARCHIVE', None)

    def run_node(self, command=None, **env):
        return subprocess.run(['/bin/sh', str(LAUNCHER), *(command or ['node', '--version'])],
                              cwd=self.root, env=dict(self.env, **env),
                              capture_output=True, text=True, timeout=20)

    def test_cold_then_offline_warm_cache(self):
        cold = self.run_node()
        self.assertEqual(cold.returncode, 0, cold.stderr)
        self.assertIn('verified download cached', cold.stdout)
        self.assertEqual(self.cache.read_bytes(), self.archive.read_bytes())
        warm = self.run_node(TEST_OFFLINE='1')
        self.assertEqual(warm.returncode, 0, warm.stderr)
        self.assertIn('verified cache hit', warm.stdout)
        self.assertEqual(self.calls.read_text(), 'called\n')

    def test_corrupt_cache_repaired(self):
        self.cache.parent.mkdir(parents=True)
        self.cache.write_bytes(b'corrupt')
        result = self.run_node()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(self.cache.read_bytes(), self.archive.read_bytes())

    def test_unavailable_or_bad_download_fails_before_child(self):
        for offline in ('1', '0'):
            with self.subTest(offline=offline):
                self.archive.write_bytes(b'wrong bytes')
                result = self.run_node(TEST_OFFLINE=offline)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn('fixture-node', result.stdout)
                self.assertFalse(self.cache.exists())

    def test_release_input_needs_no_network_and_bad_input_fails(self):
        result = self.run_node(OBERTH_TEST_NODE_ARCHIVE=str(self.archive), TEST_OFFLINE='1')
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse(self.calls.exists())
        self.archive.write_bytes(b'corrupt release input')
        result = self.run_node(OBERTH_TEST_NODE_ARCHIVE=str(self.archive))
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('fixture-node', result.stdout)
        self.assertFalse(self.calls.exists())

    def test_binary_pin_checked_before_execution(self):
        (self.root / '.oberth/pins/node-bin.sha256').write_text('0' * 64 + '  unused\n')
        result = self.run_node()
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn('fixture-node', result.stdout)

    def test_child_failure_propagates_and_scratch_is_removed(self):
        receipt = self.root / 'node-path'
        result = self.run_node(['/bin/sh', '-c', 'command -v node > "$1"; exit 37', 'child', str(receipt)])
        self.assertEqual(result.returncode, 37, result.stderr)
        self.assertFalse(Path(receipt.read_text().strip()).exists())


if __name__ == '__main__':
    unittest.main()
