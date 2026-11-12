import hashlib
import io
import json
from pathlib import Path
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
import machine_tools
from probe import Refused, Probe, scan_logs
from roundtrip import finish_hold


class CellContracts(unittest.TestCase):
    def test_both_rest_leaves_run_blocking_wrapper(self):
        root = Path(__file__).resolve().parents[2]
        for name in ('build', 'release'):
            text = (root / '.oberth' / (name + '.yaml')).read_text()
            self.assertIn('hack/bootstrap-acceptance/rest.sh', text)
        wrapper = (root / 'hack/bootstrap-acceptance/rest.sh').read_text()
        self.assertIn('exec /usr/bin/python3 -I -B hack/bootstrap-acceptance/ci.py "$@"', wrapper)
        self.assertIn('set -eu', wrapper)
        for name in ('build', 'release'):
            self.assertNotIn('fresh_machine.py', (root / '.oberth' / (name + '.yaml')).read_text())

    def test_download_checks_hash_before_tool_use(self):
        body = b'verified executable fixture'

        class Response(io.BytesIO):
            url = 'https://tools.example.invalid/pinned'
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for label, digest, bound, good in [
                    ('good', hashlib.sha256(body).hexdigest(), 1000, True),
                    ('tampered', '0' * 64, 1000, False),
                    ('oversize', hashlib.sha256(body).hexdigest(), 2, False)]:
                with patch('urllib.request.urlopen', return_value=Response(body)):
                    if good:
                        machine_tools.verified_download(Response.url, root / label, digest, bound)
                    else:
                        with self.assertRaises(Refused):
                            machine_tools.verified_download(Response.url, root / label, digest, bound)
            self.assertEqual((root / 'good').read_bytes(), body)

    def test_helm_only_reads_one_regular_member(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for variant in ('good', 'symlink', 'duplicate', 'missing'):
                path = root / (variant + '.tgz')
                with tarfile.open(path, 'w:gz') as tar:
                    for index in range(2 if variant == 'duplicate' else 1):
                        entry = tarfile.TarInfo('../outside' if variant == 'missing' else 'linux-amd64/helm')
                        if variant == 'symlink':
                            entry.type = tarfile.SYMTYPE
                            entry.linkname = '/etc/passwd'
                            tar.addfile(entry)
                        else:
                            entry.size = 5
                            tar.addfile(entry, io.BytesIO(b'bytes'))
                if variant == 'good':
                    self.assertEqual(machine_tools.helm_binary(path, 'amd64'), b'bytes')
                else:
                    with self.assertRaises(Refused):
                        machine_tools.helm_binary(path, 'amd64')
            self.assertFalse((root / 'outside').exists())

    def test_all_capture_streams_are_scanned_including_failed_stderr(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            runner = Probe(root, root, 'fixture', root / 'unused')
            value = b'synthetic-private-capture-marker'
            with self.assertRaises(Refused):
                runner.run('failure', ['/bin/sh', '-c', 'cat >&2; exit 1'], stdin=value)
            with self.assertRaisesRegex(Refused, 'artifacts withheld'):
                scan_logs(runner.logs, [value])
            self.assertEqual(len(runner.logs), 2)

    def test_exact_sha_is_checked_before_patch_and_assisted_pass_is_not_evidence(self):
        class Cell:
            def __init__(self, sha, reason='assisted: not evidence'):
                self.calls = []
                self.sha, self.reason = sha, reason
            def tool(self, name, args):
                self.calls.append(name)
                if name == 'run_get':
                    return {'run': {'sha': self.sha, 'status': 'passed', 'assisted': True,
                                    'display_reason': self.reason}}
                if name == 'run_hold_list':
                    return {'holds': [{'patch_capable': True, 'step': 'test'}]}
                return {}
        wrong = Cell('different')
        with self.assertRaises(Refused):
            finish_hold(wrong, 'run', 'expected')
        self.assertEqual(wrong.calls, ['run_get'])
        cell = Cell('expected')
        self.assertEqual(finish_hold(cell, 'run', 'expected'), {'sha': 'expected', 'run_id': 'run'})
        self.assertIn('run_hold_patch', cell.calls)
        self.assertIn('run_continue', cell.calls)
        with self.assertRaises(Refused):
            finish_hold(Cell('expected', 'ordinary pass'), 'run', 'expected')

class FreshHostBoundary(unittest.TestCase):
    def test_existing_context_or_state_is_refused_without_writes(self):
        import fresh_machine
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory) / 'new'
            with patch('platform.system', return_value='Linux'), patch('os.geteuid', return_value=0), \
                 patch('shutil.which', return_value='/usr/bin/fixture'), \
                 patch.object(Path, 'exists', return_value=False):
                with patch.dict('os.environ', {}, clear=True):
                    fresh_machine.require_fresh_host(work)
                    with self.assertRaisesRegex(Refused, 'outside the checkout'):
                        fresh_machine.require_fresh_host(Path(__file__).resolve().parent / 'private-state')
                with patch.dict('os.environ', {'KUBECONFIG': '/existing/config'}, clear=True):
                    with self.assertRaises(Refused):
                        fresh_machine.require_fresh_host(work)
            self.assertFalse(work.exists())

    def test_vm_only_flags_are_documented_and_resume_is_root_private(self):
        root = Path(__file__).resolve().parent
        script = (root / 'fresh_machine.py').read_text()
        doc = (root / 'README.md').read_text()
        self.assertIn('--confirm-disposable-host', doc)
        self.assertIn('stat.S_IMODE(info.st_mode) != 0o700', script)
        self.assertIn("state['boot_id'] ==", script)
        self.assertIn('pins.json', doc)


    def test_recovery_requires_changed_boot_before_any_probe(self):
        import fresh_machine
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory)
            (work / 'state.json').write_text(json.dumps({'version': 1, 'boot_id': 'same-boot'}))
            original = Path.read_text
            def read(path, *args, **kwargs):
                if str(path) == '/proc/sys/kernel/random/boot_id':
                    return 'same-boot\n'
                return original(path, *args, **kwargs)
            class Cell:
                def __init__(self):
                    self.work = work
                def run(self, *args):
                    raise AssertionError('recovery probe ran without a reboot')
            with patch.object(Path, 'read_text', read):
                with self.assertRaisesRegex(Refused, 'actual new boot'):
                    fresh_machine.resume(Cell())

class FreshHostRefusalControls(unittest.TestCase):
    def test_every_existing_configuration_state_and_unit_path_is_refused(self):
        import fresh_machine
        for existing in fresh_machine.fresh_host_paths():
            with self.subTest(path=str(existing)), tempfile.TemporaryDirectory() as directory:
                work = Path(directory) / 'new'
                with patch('platform.system', return_value='Linux'), patch('os.geteuid', return_value=0), \
                     patch('shutil.which', return_value='/usr/bin/fixture'), \
                     patch.dict('os.environ', {}, clear=True), \
                     patch.object(Path, 'exists', lambda path: path == existing), \
                     patch.object(Path, 'is_symlink', return_value=False):
                    with self.assertRaises(Refused):
                        fresh_machine.require_fresh_host(work)
                self.assertFalse(work.exists())

    def test_units_never_replace_files_or_symlinks(self):
        import fresh_machine
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            target = root / 'target'
            target.write_text('keep original')
            paths = [target, root / 'link', root / 'dangling']
            paths[1].symlink_to(target)
            paths[2].symlink_to(root / 'absent')
            for path in paths:
                with self.subTest(path=path.name), self.assertRaises(Refused):
                    fresh_machine.write_new_unit(path, 'replacement')
            self.assertEqual(target.read_text(), 'keep original')
            self.assertFalse((root / 'absent').exists())
            unit = root / 'new.service'
            fresh_machine.write_new_unit(unit, 'new unit')
            self.assertEqual(unit.read_text(), 'new unit')

class ReleaseRuntimeControls(unittest.TestCase):
    def test_release_limits_and_noninteractive_git_survive_child_environment(self):
        import os
        values = dict(GOMEMLIMIT='4GiB', OBERTH_TEST_TIMEOUT_SCALE='4',
                      GOPATH='/private/gopath', GIT_TERMINAL_PROMPT='0')
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, values):
            root = Path(directory)
            runner = Probe(root, root, 'selected', root / 'unused')
            for name, value in values.items():
                self.assertEqual(runner.env[name], value)

    def test_own_budget_stops_a_command_and_reports_only_stage(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            runner = Probe(root, root, 'selected', root / 'unused')
            runner.budget(0.05)
            with self.assertRaises(Refused) as failure:
                runner.run('slow', [sys.executable, '-c', 'import time; time.sleep(60)'])
            self.assertEqual(failure.exception.stage, 'slow')
            self.assertEqual(failure.exception.check, 'command-timeout')

    def test_missing_fixture_event_reports_exact_required_test(self):
        from ci import TESTS, require_pass_events
        raw = '\n'.join(json.dumps({'Action': 'pass', 'Package': package, 'Test': name}) for package, name in TESTS[1:]).encode()
        with self.assertRaises(Refused) as failure:
            require_pass_events(raw)
        self.assertEqual((failure.exception.package, failure.exception.test), TESTS[0])

    def test_decoded_go_output_is_scanned_across_event_boundaries(self):
        from probe import scan_go_events
        value = b'private-fixture-body\nsecond-private-fixture-line'
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'events'
            path.write_text('\n'.join(json.dumps({'Output': part}) for part in
                                      ('private-fixture-', 'body\nsecond-private-fixture-line')))
            with self.assertRaisesRegex(Refused, 'decoded Go output'):
                scan_go_events(path, [value])

class SafeGoDiagnostics(unittest.TestCase):
    def test_timeout_identity_comes_from_events_not_captured_output(self):
        from probe import active_go_identity
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'events'
            path.write_text('\n'.join(json.dumps(event) for event in [
                {'Action': 'run', 'Test': 'TestActionable'},
                {'Action': 'output', 'Test': 'TestActionable/unsafe secret', 'Output': 'private-output'},
                {'Action': 'run', 'Test': 'TestCompleted'},
                {'Action': 'pass', 'Test': 'TestCompleted'},
            ]))
            self.assertEqual(active_go_identity(path), (None, 'TestActionable'))

class CellFailurePrivacy(unittest.TestCase):
    def test_final_scan_failure_has_stage_and_no_captured_output(self):
        import ci
        import signal
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            runner = Probe(root, root, 'selected', root / 'unused')
            runner.secrets.write_bytes(b'FIXTURE=private-capture-marker\n')
            path = root / 'capture'
            path.write_bytes(b'private-capture-marker')
            runner.logs.append(path)
            previous = signal.getsignal(signal.SIGALRM)
            with patch.object(ci, 'run_cell', return_value=0), self.assertRaises(Refused) as error:
                ci.checked_cell(runner, [])
            self.assertEqual(error.exception.stage, 'secret-scan')
            self.assertEqual(error.exception.check, 'credential-exposure')
            self.assertNotIn('private-capture-marker', str(error.exception))
            self.assertEqual(signal.getsignal(signal.SIGALRM), previous)

class AssertionEventPrivacy(unittest.TestCase):
    def test_failing_assertion_emits_only_its_identity(self):
        import assertions
        class Broken(unittest.TestCase):
            def test_fixture(self):
                self.fail('private diagnostic must stay captured')
        output = io.StringIO()
        self.assertFalse(assertions.run_suite(unittest.TestSuite([Broken('test_fixture')]), output))
        records = [json.loads(line) for line in output.getvalue().splitlines()]
        self.assertTrue(any(record['action'] == 'fail' and record['test'].endswith('.Broken.test_fixture') for record in records))
        self.assertNotIn('private diagnostic', output.getvalue())


if __name__ == '__main__':
    unittest.main()
