#!/usr/bin/env python3
"""Adversarial controls for the #649 website publication helpers.

Covers .oberth/verify-website-inputs.py (source binding, published-tree
exactness, Node pin and staged-input copies) and .oberth/website-readback.py
(#647 helper contract and public-byte convergence) without network access.
"""
import hashlib
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


def load(name, relative):
    spec = importlib.util.spec_from_file_location(name, ROOT / relative)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


verifier = load('website_verifier', '.oberth/verify-website-inputs.py')
readback = load('website_readback', '.oberth/website-readback.py')

GIT_ENV = {'PATH': '/usr/bin:/bin', 'GIT_CONFIG_NOSYSTEM': '1', 'GIT_CONFIG_GLOBAL': '/dev/null'}
AUDIT = ('# --- audit preflight (read-only; before any store mutation) ---\n'
         "python3 -c 'devices = json.load(sys.stdin)'\n"
         '# --- end audit preflight ---\n')


class Repository:
    def __init__(self, root, files):
        self.root = root
        for relative, body in files.items():
            path = root / relative
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(body)
        self.git('init', '-q')
        self.git('add', '.')
        self.git('-c', 'user.name=Test', '-c', 'user.email=test@example.invalid', 'commit', '-qm', 'reviewed')
        self.sha = self.git('rev-parse', 'HEAD').strip().decode()
        self.git('tag', 'v1.2.3')

    def git(self, *args):
        return subprocess.run(['/usr/bin/git', '-C', str(self.root), *args], check=True, capture_output=True,
                              env={**GIT_ENV, 'HOME': str(self.root)}).stdout


def source_files():
    files = {relative: ('reviewed ' + relative + '\n').encode() for relative in verifier.SOURCE_FILES}
    files['.oberth/pins/node.url'] = b'https://nodejs.org/dist/v22.23.2/node-v22.23.2-linux-x64.tar.gz\n'
    files['website/public/index.html'] = b'<!doctype html>\n'
    files['website/public/fonts/a.woff2'] = b'\x00font'
    files['website/public/setup-secretstore.sh'] = ('#!/usr/bin/env bash\n' + AUDIT).encode()
    return files


class SourceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.repo = Repository(Path(self.temp.name), source_files())

    def verify(self, sha=None, tag='v1.2.3'):
        verifier.verify_source(self.repo.root, sha or self.repo.sha, tag)

    def test_exact_admitted_tree_passes(self):
        self.verify()

    def test_untracked_published_file_fails(self):
        (self.repo.root / 'website/public/extra.js').write_text('injected')
        with self.assertRaises(verifier.VerificationError):
            self.verify()

    def test_modified_published_file_with_assume_unchanged_fails(self):
        self.repo.git('update-index', '--assume-unchanged', 'website/public/index.html')
        (self.repo.root / 'website/public/index.html').write_text('<script>tampered</script>')
        self.repo.git('diff', '--exit-code')
        with self.assertRaises(verifier.VerificationError):
            self.verify()

    def test_substituted_publisher_source_fails(self):
        (self.repo.root / 'website/package-lock.json').write_text('{"tampered": true}')
        with self.assertRaises(verifier.VerificationError):
            self.verify()

    def test_symlinked_published_entries_fail(self):
        (self.repo.root / 'website/public/link.html').symlink_to(self.repo.root / 'website/public/index.html')
        with self.assertRaises(verifier.VerificationError):
            self.verify()
        (self.repo.root / 'website/public/link.html').unlink()
        (self.repo.root / 'website/public/dir').symlink_to(self.repo.root / 'website', target_is_directory=True)
        with self.assertRaises(verifier.VerificationError):
            self.verify()

    def test_different_commit_and_tag_fail(self):
        for sha, tag in (('1' * 40, 'v1.2.3'), (None, 'v9.9.9'), ('short', 'v1.2.3'), (None, '--help')):
            with self.subTest(sha=sha, tag=tag), self.assertRaises((verifier.VerificationError, subprocess.CalledProcessError)):
                self.verify(sha, tag)


class InputTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.tarball = b'\x1f\x8b reviewed node tarball'
        self.digest = hashlib.sha256(self.tarball).hexdigest()
        self.source = self.root / 'src'
        (self.source / '.oberth/pins').mkdir(parents=True)
        (self.source / '.oberth/pins/node.url').write_text('https://nodejs.org/dist/v22.23.2/node-v22.23.2-linux-x64.tar.gz\n')
        (self.source / '.oberth/pins/node.sha256').write_text(self.digest + '  /tmp/oberth-website-inputs/node.tar.gz\n')
        self.inputs = self.root / 'inputs'
        (self.inputs / 'npm-cache/_cacache/content-v2').mkdir(parents=True)
        (self.inputs / 'npm-cache/_cacache/content-v2/blob').write_bytes(b'content-addressed')
        (self.inputs / 'node.tar.gz').write_bytes(self.tarball)
        self.private = self.root / 'private'

    def stage(self):
        verifier.stage_private_inputs(self.source, self.inputs, self.private)

    def test_verified_inputs_are_copied_privately(self):
        self.stage()
        self.assertEqual((self.private / 'node.tar.gz').read_bytes(), self.tarball)
        self.assertEqual((self.private / 'npm-cache/_cacache/content-v2/blob').read_bytes(), b'content-addressed')
        self.assertEqual(os.stat(self.private).st_mode & 0o777, 0o700)

    def test_substituted_tarball_fails_and_leaves_no_copy(self):
        (self.inputs / 'node.tar.gz').write_bytes(b'attacker runtime')
        with self.assertRaises(verifier.VerificationError):
            self.stage()
        self.assertFalse((self.private / 'node.tar.gz').exists())

    def test_symlinked_tarball_fails(self):
        (self.inputs / 'real.tar.gz').write_bytes(self.tarball)
        (self.inputs / 'node.tar.gz').unlink()
        (self.inputs / 'node.tar.gz').symlink_to(self.inputs / 'real.tar.gz')
        with self.assertRaises(OSError):
            self.stage()

    def test_links_and_special_files_in_cache_fail(self):
        (self.inputs / 'npm-cache/_cacache/escape').symlink_to('/etc/passwd')
        with self.assertRaises(verifier.VerificationError):
            self.stage()

    def test_fifo_in_cache_fails_without_waiting(self):
        os.mkfifo(self.inputs / 'npm-cache/_cacache/pipe')
        with self.assertRaises(verifier.VerificationError):
            self.stage()

    def test_preexisting_private_scratch_fails(self):
        self.private.mkdir()
        with self.assertRaises(FileExistsError):
            self.stage()

    def test_malformed_pins_fail(self):
        for name, body in (('node.url', 'http://nodejs.org/dist/v22.23.2/node-v22.23.2-linux-x64.tar.gz\n'),
                           ('node.url', 'https://example.com/node-v22.23.2-linux-x64.tar.gz\n'),
                           ('node.sha256', self.digest + '  /tmp/elsewhere/node.tar.gz\n'),
                           ('node.sha256', self.digest + '  /tmp/oberth-website-inputs/node.tar.gz\n' * 2)):
            with self.subTest(name=name, body=body):
                original = (self.source / '.oberth/pins' / name).read_text()
                (self.source / '.oberth/pins' / name).write_text(body)
                with self.assertRaises(verifier.VerificationError):
                    verifier.node_pin(self.source)
                (self.source / '.oberth/pins' / name).write_text(original)


class ReadbackTests(unittest.TestCase):
    TARGETS = (('https://oberth.ci/', 'index.html', 'text/html'),
               ('https://oberth.ci/setup-secretstore.sh', 'setup.sh', 'text/plain'))

    def setUp(self):
        self.expected = {url: hashlib.sha256(url.encode()).hexdigest() for url, _, _ in self.TARGETS}
        self.now = 0.0

    def clock(self):
        return self.now

    def sleep(self, seconds):
        self.now += seconds

    def run_readback(self, responses):
        return readback.readback(self.TARGETS, self.expected, fetcher=responses, clock=self.clock,
                                 sleep=self.sleep, budget=60, interval=15, log=lambda line: None)

    def fresh(self, url):
        return 200, url.encode(), 'text/html; charset=utf-8' if url.endswith('/') else 'text/plain; charset=utf-8'

    def test_converged_site_passes_first_attempt(self):
        self.assertEqual(self.run_readback(self.fresh), 1)

    def test_stale_edge_copy_waits_until_fresh(self):
        def responses(url):
            if url.endswith('.sh') and self.now < 30:
                return 200, b'pre-#647 helper', 'text/plain'
            return self.fresh(url)
        self.assertEqual(self.run_readback(responses), 3)

    def test_never_converging_site_fails(self):
        with self.assertRaises(readback.ReadbackError):
            self.run_readback(lambda url: (200, b'stale', 'text/plain'))

    def test_wrong_content_type_or_redirect_never_matches(self):
        for response in ((200, None, 'application/octet-stream'), (301, b'', ''), (404, b'', '')):
            with self.subTest(response=response):
                self.now = 0.0

                def responses(url, response=response):
                    status, body, content_type = response
                    return status, url.encode() if body is None else body, content_type
                with self.assertRaises(readback.ReadbackError):
                    self.run_readback(responses)

    def test_plain_http_is_refused(self):
        with self.assertRaises(readback.ReadbackError):
            readback.fetch('http://oberth.ci/')


class HelperContractTests(unittest.TestCase):
    def committed(self, canonical, public):
        return {'scripts/setup-secretstore.sh': canonical.encode(),
                'website/public/setup-secretstore.sh': public.encode()}

    def test_identical_semantic_preflight_passes(self):
        digest = readback.verify_helper_sources(self.committed('a\n' + AUDIT + 'b\n', 'c\n' + AUDIT + 'd\n'))
        self.assertEqual(digest, hashlib.sha256(AUDIT.encode()).hexdigest())

    def test_pre_647_grep_fails(self):
        stale = AUDIT + "printf '%s' \"$AUDIT_ENABLED\" | grep -q '\"type\":\"file\"'\n"
        with self.assertRaises(readback.ReadbackError):
            readback.verify_helper_sources(self.committed(AUDIT, stale))

    def test_diverging_or_missing_preflight_fails(self):
        for public in (AUDIT.replace('json.load', 'json.loads'), 'no preflight\n', AUDIT + AUDIT):
            with self.subTest(public=public), self.assertRaises(readback.ReadbackError):
                readback.verify_helper_sources(self.committed(AUDIT, public))

    def test_repository_helpers_satisfy_the_contract(self):
        committed = {name: (ROOT / name).read_bytes() for name in readback.HELPERS}
        readback.verify_helper_sources(committed)


if __name__ == '__main__':
    unittest.main()
