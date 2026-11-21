import contextlib
import hashlib
import io
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import build
import layout


class DriverTests(unittest.TestCase):
    def test_offline_commands_candidate_pin_and_hash_mismatch(self):
        for mismatch in [False, True]:
            with self.subTest(mismatch=mismatch), tempfile.TemporaryDirectory() as directory:
                self.check_driver(Path(directory), mismatch)

    def check_driver(self, root, mismatch):
        repository, workspace = root / 'repository', root / 'work'
        records = fixture_records(repository)
        reviewed = records.read_bytes()
        canonical = repository / '.oberth/pins/base-image.digest'
        canonical.parent.mkdir(parents=True)
        canonical.write_text('reviewed canonical pin\n')
        commands = []
        lock = json.loads((build.HERE / 'inputs.lock.json').read_text())

        def command(args, **kwargs):
            self.assertIn('--network=none', args)
            self.assertTrue(kwargs['check'])
            commands.append(args)
            destination = Path(next(a.removeprefix('--output=type=local,dest=') for a in args
                                    if a.startswith('--output=type=local,dest=')))
            if Path(args[-1]).name == 'assembly-context':
                self.assertIn('--build-arg=ASSEMBLER_IMAGE=' + lock['assembler_image'], args)
                layout.assemble(workspace / 'artifacts', destination)
            else:
                architecture = next(a.removeprefix('--platform=linux/') for a in args
                                    if a.startswith('--platform=linux/'))
                platform = lock['platforms'][architecture]
                self.assertIn('--build-arg=BUILDER_IMAGE=' + platform['builder'], args)
                names = sorted(package['file'] for package in platform['packages'])
                expected = '--build-arg=PACKAGE_FILES=' + ' '.join('/inputs/packages/' + name for name in names)
                self.assertIn(expected, args)
                self.assertNotIn('*', expected)
                destination.mkdir(parents=True)
                for name, body in compiled_files(architecture).items():
                    if mismatch and architecture == 'arm64' and name == 'git':
                        body = b'changed compiler output'
                    (destination / name).write_bytes(body)
            return subprocess.CompletedProcess(args, 0)

        with contextlib.chdir(repository), contextlib.redirect_stdout(io.StringIO()), \
                patch.object(build, 'REPOSITORY', repository), \
                patch.object(build, 'context', side_effect=lambda w, a, lock: w / ('context-' + a)), \
                patch.object(build.subprocess, 'run', side_effect=command):
            if mismatch:
                with self.assertRaisesRegex(SystemExit, 'Build differs from reviewed artifacts'):
                    build.run(workspace)
            else:
                build.run(workspace)
        self.assertEqual(canonical.read_text(), 'reviewed canonical pin\n')
        candidate = workspace / 'candidate.digest'
        self.assertEqual(set(root.rglob('*.digest')), {canonical, candidate})
        index = (workspace / 'base-image/index.json').read_bytes()
        self.assertEqual(candidate.read_text(), 'sha256:' + hashlib.sha256(index).hexdigest() + '\n')
        self.assertEqual(records.read_bytes(), reviewed)
        self.assertEqual([a[3] for a in commands[:2]], ['--platform=linux/amd64', '--platform=linux/arm64'])
        self.assertEqual(len(commands), 3)
        report = json.loads((workspace / 'build-report.json').read_text())
        self.assertTrue(report['amd64']['matches'])
        self.assertEqual(report['arm64']['matches'], not mismatch)


def compiled_files(architecture):
    return {name: (name + architecture).encode() for name in ['git', 'musl.a', 'zlib.a', 'ca-certificates.crt']}


def fixture_records(repository):
    records = {}
    for architecture in ['amd64', 'arm64']:
        files = compiled_files(architecture)
        records[architecture] = dict(architecture=architecture, **{
            key: hashlib.sha256(files[name]).hexdigest() for key, name in [
                ('git_sha256', 'git'), ('musl_archive_sha256', 'musl.a'), ('zlib_archive_sha256', 'zlib.a')]})
    path = repository / 'internal/staticgit/qualified-artifacts.json'
    path.parent.mkdir(parents=True)
    path.write_text(json.dumps(records))
    return path


if __name__ == '__main__':
    unittest.main()
