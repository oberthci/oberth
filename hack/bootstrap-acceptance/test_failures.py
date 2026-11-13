import json
from pathlib import Path
import signal
import tempfile
import time
import unittest
from unittest.mock import patch

import ci
from probe import Probe, Refused, diagnostic, scan_go_events, verify_result


class PlanResultSemantics(unittest.TestCase):
    def result(self, **changes):
        return dict(version=1, ok=True, context='selected', checks=[
            dict(code='cluster_ca', ok=True),
            dict(code='network_policy', ok=False, warning=True),
        ]) | changes

    def test_advisory_policy_check_does_not_reject_a_green_plan(self):
        verify_result(self.result(), 'selected')

    def test_hard_failure_is_named_despite_advisory_warnings(self):
        result = self.result()
        result['checks'].append(dict(code='node_ready/selected-node', ok=False,
                                     reason='private diagnostic', remedy='private remedy'))
        result['ok'] = False
        with self.assertRaises(Refused) as caught:
            verify_result(result, 'selected')
        self.assertEqual(caught.exception.check, 'node_ready')
        self.assertNotIn('private', str(caught.exception))

    def test_warning_flags_cannot_override_aggregate_failure_or_malformed_checks(self):
        cases = [self.result(ok=False), self.result(checks=[dict(code='tls', ok=False)]),
                 self.result(checks=[dict(code='tls', ok=False, warning='true')]),
                 self.result(checks=[dict(code='tls', ok='true', warning=True)]),
                 self.result(checks=[dict(code=[], ok=True)]), self.result(checks=[None])]
        for result in cases:
            with self.subTest(result=result), self.assertRaises(Refused):
                verify_result(result, 'selected')

    def test_nonzero_exit_cannot_be_hidden_by_a_green_plan_result(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            probe = Probe(root, root, 'selected', root / 'unused')
            probe.last_returncode = 1
            result = self.result() | dict(identity=dict(context='selected'))
            with patch.object(probe, 'run', return_value=json.dumps(result).encode()):
                with self.assertRaises(Refused) as caught:
                    ci.verify_plan(probe, 'plan-selected', [], 'selected')
            self.assertEqual((caught.exception.stage, caught.exception.check), ('plan-selected', 'plan-exit'))

    def test_malformed_cli_plan_keeps_stage_package_and_schema_check(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            probe = Probe(root, root, 'selected', root / 'unused')
            for raw, check in [(b'{', 'plan-json'), (b'[]', 'plan-schema'), (b'{}', 'plan-schema')]:
                with self.subTest(check=check), patch.object(probe, 'run', return_value=raw):
                    with self.assertRaises(Refused) as caught:
                        ci.verify_plan(probe, 'plan-selected', [], 'selected')
                self.assertEqual((caught.exception.stage, caught.exception.check, caught.exception.package),
                                 ('plan-selected', check, ci.CLI_PACKAGE))

    def test_hard_plan_control_preserves_unexpected_failure_identity(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            probe = Probe(root, root, 'selected', root / 'unused')
            original = Refused('time budget exceeded', stage='plan-node-refusal',
                               check='command-timeout', package=ci.CLI_PACKAGE)
            with patch.object(ci, 'verify_plan', side_effect=original):
                with self.assertRaises(Refused) as caught:
                    ci.require_node_refusal(probe, [])
            self.assertIs(caught.exception, original)


class RestrictedPlanFixture(unittest.TestCase):
    def test_every_mutating_method_is_refused_including_scratch_creation(self):
        ci.assert_no_mutations([('GET', '/api/v1/nodes')])
        for method in ('POST', 'PUT', 'PATCH', 'DELETE'):
            with self.subTest(method=method), self.assertRaises(Refused):
                ci.assert_no_mutations([(method, '/api/v1/namespaces')])

    def test_plan_preserves_choice_and_discloses_deferred_enforcement(self):
        result = dict(review=dict(network_policy='true'), checks=[dict(
            code='network_policy', ok=False, warning=True, reason='unverified',
            remedy='NetworkPolicy enforcement check will run first at install, after approval.')])
        ci.require_deferred_policy(result, 'true')
        for change in [dict(ok=True), dict(warning=False), dict(warning='true'),
                       dict(reason='enforced'), dict(remedy=''), dict(evidence='measured')]:
            with self.subTest(change=change), self.assertRaises(Refused):
                ci.require_deferred_policy(result | dict(checks=[result['checks'][0] | change]), 'true')
        with self.assertRaises(Refused):
            ci.require_deferred_policy(result | dict(review=dict(network_policy='false')), 'true')


class FixtureIdentity(unittest.TestCase):
    def passes(self):
        return [dict(Package=package, Test=test, Action='pass') for package, test in ci.TESTS]

    def check(self, events):
        ci.require_pass_events('\n'.join(map(json.dumps, events)).encode())

    def test_names_under_another_package_cannot_satisfy_the_contract(self):
        events = self.passes()
        events[0]['Package'] = 'github.com/oberthci/oberth/unrelated'
        with self.assertRaises(Refused) as caught:
            self.check(events)
        self.assertEqual((caught.exception.package, caught.exception.test), ci.TESTS[0])

    def test_last_terminal_event_must_pass_the_expected_pair(self):
        self.check(self.passes())
        events = self.passes()
        events.append(events[0] | dict(Action='skip'))
        events.append(events[0] | dict(Package='github.com/oberthci/oberth/unrelated'))
        with self.assertRaises(Refused):
            self.check(events)

    def test_package_and_subtest_failures_are_named_without_output(self):
        package, test = ci.TESTS[0]
        for identity in [dict(Package=package), dict(Package=package, Test=test + '/private-label')]:
            with self.subTest(identity=identity), self.assertRaises(Refused) as caught:
                self.check(self.passes() + [identity | dict(Action='fail', Output='private output')])
            line = diagnostic(caught.exception)
            self.assertIn('stage=rest-race', line)
            self.assertIn('package=' + package, line)
            self.assertNotIn('private', line.removesuffix('; private artifacts withheld'))
            self.assertIn('check=go-', line)


class FailurePathBudget(unittest.TestCase):
    def test_original_plan_failure_keeps_its_stage_after_final_scan(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            probe = Probe(root, root, 'selected', root / 'unused')
            def fail(probe, command):
                probe.stage = 'plan-selected'
                verify_result(dict(version=1, ok=False, context='selected',
                                   checks=[dict(code='node_ready/selected-node', ok=False)]), 'selected')
            with patch.object(ci, 'run_cell', side_effect=fail), patch.object(ci, 'final_scan') as scan:
                with self.assertRaises(Refused) as caught:
                    ci.checked_cell(probe, [])
            scan.assert_called_once_with(probe)
            self.assertIn('stage=plan-selected check=node_ready', diagnostic(caught.exception))

    def test_expired_cell_alarm_gets_an_independent_bounded_failure_scan(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            probe = Probe(root, root, 'selected', root / 'unused')
            def expire(probe, command):
                probe.stage = 'plan-selected'
                probe.budget(0.03)
                signal.setitimer(signal.ITIMER_REAL, 0.03)
                time.sleep(1)
            previous = signal.getsignal(signal.SIGALRM)
            started = time.monotonic()
            with patch.object(ci, 'run_cell', side_effect=expire), \
                 patch.object(ci, 'final_scan', side_effect=lambda probe: time.sleep(1)), \
                 patch.object(ci, 'FAILURE_SCAN_BUDGET', 0.03):
                with self.assertRaises(Refused) as caught:
                    ci.checked_cell(probe, [])
            self.assertLess(time.monotonic() - started, 0.5)
            self.assertEqual(caught.exception.stage, 'plan-selected')
            self.assertEqual(caught.exception.check, 'cell-timeout')
            cleanup = caught.exception.cleanup_failure
            self.assertEqual((cleanup.stage, cleanup.check), ('secret-scan', 'failure-scan-timeout'))
            self.assertEqual(signal.getsignal(signal.SIGALRM), previous)
            self.assertEqual(signal.getitimer(signal.ITIMER_REAL), (0.0, 0.0))


class FailureDiagnostics(unittest.TestCase):
    def test_only_valid_identity_metadata_can_reach_the_failure_line(self):
        failure = Refused('private message', stage='plan-selected', check='disk',
                          package='github.com/oberthci/oberth/internal/installer', test='TestFixture')
        line = diagnostic(failure)
        self.assertIn('stage=plan-selected check=disk package=', line)
        self.assertIn('test=TestFixture', line)
        self.assertNotIn('private message', line)
        failure.stage = 'private\nmessage'
        failure.package = 'external/private'
        failure.test = 'TestFixture/private-label'
        line = diagnostic(failure)
        self.assertIn('stage=unavailable', line)
        self.assertIn('package=unavailable test=unavailable', line)

    def test_generated_credential_exposure_identifies_go_channel_package_and_test(self):
        package, test = ci.TESTS[0]
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'go-events'
            path.write_text(json.dumps(dict(Package=package, Test=test, Output='generated-private-value')))
            with self.assertRaises(Refused) as caught:
                scan_go_events(path, [b'generated-private-value'])
            failure = caught.exception
            failure.stage = 'secret-scan'
            line = diagnostic(failure)
            self.assertIn('check=credential-exposure', line)
            self.assertIn('package=' + package, line)
            self.assertIn('test=' + test + ' channel=go-output', line)
            self.assertNotIn('generated-private-value', line)
