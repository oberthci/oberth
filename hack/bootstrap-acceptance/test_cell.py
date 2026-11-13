import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
from probe import Refused, Probe, scan_logs


class CellContracts(unittest.TestCase):
    def test_both_rest_leaves_run_blocking_wrapper(self):
        root = Path(__file__).resolve().parents[2]
        for name in ('build', 'release'):
            text = (root / '.oberth' / (name + '.yaml')).read_text()
            self.assertIn('hack/bootstrap-acceptance/rest.sh', text)
        wrapper = (root / 'hack/bootstrap-acceptance/rest.sh').read_text()
        self.assertIn('exec /usr/bin/python3 -I -B hack/bootstrap-acceptance/ci.py "$@"', wrapper)
        self.assertIn('set -eu', wrapper)

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
