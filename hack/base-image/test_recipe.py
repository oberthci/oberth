import hashlib
import io
import json
import os
import shlex
import sys
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import build
import layout


class RecipeTests(unittest.TestCase):
    def test_download_is_published_only_after_its_hash_matches(self):
        for valid in [False, True]:
            with self.subTest(valid=valid), tempfile.TemporaryDirectory() as directory:
                destination = Path(directory) / 'input'
                item = {'url': 'https://example.invalid/input',
                        'sha256': hashlib.sha256(b'approved').hexdigest()}
                body = b'approved' if valid else b'changed'
                with patch('urllib.request.urlopen', return_value=io.BytesIO(body)):
                    if valid:
                        build.fetch(item, destination)
                        self.assertEqual(destination.read_bytes(), body)
                        self.assertFalse(destination.with_name('input.part').exists())
                    else:
                        with self.assertRaises(ValueError) as failure:
                            build.fetch(item, destination)
                        self.assertFalse(destination.exists())
                        self.assertEqual(destination.with_name('input.part').read_bytes(), body)
                        self.assertIn('delete', str(failure.exception))
                        self.assertIn('rerun', str(failure.exception))

    def test_reused_context_refuses_unlisted_package_without_removing_it(self):
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory)
            lock, pins = cached_context(workspace)
            extra = workspace / 'contexts/amd64/packages/unlisted.apk'
            extra.write_bytes(b'preserved evidence')
            with patch.object(build, 'build_inputs', return_value=pins), \
                    patch('urllib.request.urlopen', side_effect=AssertionError('network used')):
                with self.assertRaisesRegex(ValueError, 'Unlisted package input'):
                    build.context(workspace, 'amd64', lock)
            self.assertEqual(extra.read_bytes(), b'preserved evidence')

    def test_context_installs_only_the_verified_package_names(self):
        with tempfile.TemporaryDirectory() as directory:
            workspace = Path(directory)
            lock, pins = cached_context(workspace)
            with patch.object(build, 'build_inputs', return_value=pins), \
                    patch('urllib.request.urlopen', side_effect=AssertionError('network used')):
                source = build.context(workspace, 'amd64', lock)
            recipe = (source / 'Dockerfile').read_text()
            self.assertNotIn('*.apk', recipe)
            self.assertEqual(recipe, (build.HERE / 'Dockerfile').read_text())
            self.assertIn('ARG PACKAGE_FILES\n', recipe)
            self.assertIn('RUN set -f;', recipe)
            self.assertIn('add --no-network $PACKAGE_FILES\n', recipe)

    def test_cached_input_never_uses_network(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'source'
            path.write_bytes(b'approved source')
            item = {'url': 'https://example.invalid/source', 'sha256': hashlib.sha256(path.read_bytes()).hexdigest()}
            with patch('urllib.request.urlopen', side_effect=AssertionError('network used')):
                build.fetch(item, path)
                path.write_bytes(b'changed source')
                with self.assertRaisesRegex(ValueError, 'Pinned input differs'):
                    build.fetch(item, path)
            self.assertEqual(path.read_bytes(), b'changed source')

    def test_lock_covers_the_exact_reviewed_package_closure(self):
        lock = json.loads((build.HERE / 'inputs.lock.json').read_text())
        pins = dict(line.split('=', 1) for line in (build.REPOSITORY / 'internal/staticgit/build.env').read_text().splitlines() if line and not line.startswith('#'))
        packages = set(shlex.split(pins['APK_PACKAGES'])[0].split())
        self.assertEqual(set(lock['git']), {'url'})
        self.assertNotIn('patch', lock)
        self.assertEqual(hashlib.sha256((build.HERE / 'git-no-shell.patch').read_bytes()).hexdigest(), pins['GIT_PATCH_SHA256'])
        self.assertEqual(set(lock['platforms']), {'amd64', 'arm64'})
        self.assertRegex(lock['assembler_image'], r'@sha256:[a-f0-9]{64}$')
        self.assertRegex(lock['emulation_image'], r'@sha256:[a-f0-9]{64}$')
        for platform in lock['platforms'].values():
            self.assertRegex(platform['builder'], r'@sha256:[a-f0-9]{64}$')
            self.assertEqual({p['name']+'='+p['version'] for p in platform['packages']}, packages)
            self.assertEqual(len(platform['packages']), len(packages))
            for package in platform['packages']:
                self.assertRegex(package['sha256'], r'^[a-f0-9]{64}$')
                self.assertTrue(package['url'].endswith('/'+package['file']))

    def test_layout_is_independent_of_input_timestamps(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            artifacts = root / 'artifacts'
            for architecture in ['arm64', 'amd64']:
                path = artifacts / architecture
                path.mkdir(parents=True)
                (path / 'git').write_bytes(('compiled '+architecture).encode())
                (path / 'ca-certificates.crt').write_bytes(b'certificate data')
            first = layout.assemble(artifacts, root / 'first')
            for path in artifacts.rglob('*'):
                os.utime(path, (123456789, 123456789))
            second = layout.assemble(artifacts, root / 'second')
            self.assertEqual(first, second)
            index = json.loads((root/'first/index.json').read_text())
            self.assertEqual([d['platform'] for d in index['manifests']], [{'architecture': a, 'os': 'linux'} for a in ['amd64', 'arm64']])
            for descriptor in index['manifests']:
                manifest = json.loads((root/'first/blobs/sha256'/descriptor['digest'][7:]).read_text())
                config = json.loads((root/'first/blobs/sha256'/manifest['config']['digest'][7:]).read_text())
                self.assertEqual(set(config), {'architecture', 'os', 'rootfs'})
                layer = root/'first/blobs/sha256'/manifest['layers'][0]['digest'][7:]
                with tarfile.open(layer) as archive:
                    self.assertEqual({m.name for m in archive if m.isfile()}, {'usr/bin/git', 'etc/ssl/certs/ca-certificates.crt'})
                    self.assertEqual(archive.getmember('usr/bin/git').mode, 0o755)
                    self.assertEqual(archive.getmember('data').uid, 65534)
                    self.assertEqual(archive.getmember('tmp').mode, 0o1777)


def cached_context(workspace):
    lock = json.loads((build.HERE / 'inputs.lock.json').read_text())
    root = workspace / 'contexts/amd64'
    (root / 'packages').mkdir(parents=True)
    for path in [root / 'packages/approved.apk', root / 'git.tar.xz']:
        path.write_bytes(b'pinned fixture')
    item = {'url': 'https://example.invalid/input',
            'sha256': hashlib.sha256(b'pinned fixture').hexdigest()}
    lock['git'] = {'url': item['url']}
    lock['platforms']['amd64']['packages'] = [dict(item, file='approved.apk')]
    pins = build.build_inputs()
    pins['GIT_SOURCE_SHA256'] = item['sha256']
    return lock, pins


if __name__ == '__main__':
    unittest.main()
