"""Distinguish declared walk inspections from committed candidate evidence."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

import test_run as fixtures

GUARDS = fixtures.GUARDS


class WalkResults(unittest.TestCase):
    setUpClass = classmethod(fixtures.TextGuards.setUpClass.__func__)
    git = fixtures.TextGuards.git
    write = fixtures.TextGuards.write
    def setUp(self):
        fixtures.TextGuards.setUp(self)
        for file in ('dependencies.json',):
            self.write('docs/plans/'+file, (GUARDS.ROOT/'docs/plans'/file).read_text())
        self.write('hack/guards/messages.json', (GUARDS.ROOT/'hack/guards/messages.json').read_text())
        self.write('docs/plans/requirements.json', json.dumps({'requirements': [
            {'id': 'REQ-C1-001', 'component': 'C1', 'inspection': {'kind': 'walk_step', 'name': 'push'}},
        ]}))
        self.commit = 'a'*40
        self.environment = patch.dict(os.environ)
        self.environment.start()
        self.addCleanup(self.environment.stop)

    def table(self, names=('push',)):
        self.write('test/walk/walklib.py', 'raise RuntimeError("do not execute")\nSTEPS = (\n'+
                   ''.join(f'Step({name!r}, "Assertion", action),\n' for name in names)+')\n')

    def result(self, name='push', verdict='pass', commit=None, file='20261116T170000Z.json'):
        row = {'name': name, 'verdict': verdict,
               'evidence_sha256': hashlib.sha256(b'assertion satisfied\n').hexdigest()}
        value = {'commit': commit or self.commit, 'steps': [row]}
        self.write('test/walk/results/'+file, json.dumps(value))
        self.git('commit', '-qm', 'Record candidate evidence')
        return value

    def analyze(self):
        command = GUARDS.command
        def run(args, data=None):
            if args == ['git', 'rev-parse', 'HEAD']:
                return subprocess.CompletedProcess(args, 0, stdout=self.commit+'\n')
            if args[:2] == ['go', 'list']:
                return subprocess.CompletedProcess(args, 0, stdout='example.invalid/unit\n')
            if args[:2] == ['go', 'run']:
                return command([self.binary, 'example.invalid/unit'], data)
            return command(args, data)
        with patch.object(GUARDS, 'command', side_effect=run):
            _, report = GUARDS.analysis()
        return report['Lines']

    def assert_summary(self, due, defined, proven):
        lines = self.analyze()
        expected = (f'requirements: {due} due; 0 covered by tests; '
                    f'{defined} defined in the walk; {proven} proven by a candidate walk')
        self.assertEqual([expected], [line for line in lines if line.startswith('requirements:')])
        return lines

    def test_absent_walk_waits_without_candidate_proof(self):
        self.assert_summary(0, 0, 0)

    def test_missing_definition_fails_due_requirement(self):
        self.table(())
        self.assertTrue(any('[REQ]' in line for line in self.assert_summary(1, 0, 0)))

    def test_definition_does_not_claim_test_coverage_or_candidate_proof(self):
        self.table()
        self.assertFalse(any(GUARDS.RULE.search(line) for line in self.assert_summary(1, 1, 0)))

    def test_current_candidate_pass_proves_defined_step(self):
        self.table()
        self.result()
        self.assert_summary(1, 1, 1)

    def test_nonpassing_candidate_does_not_prove_step(self):
        self.table()
        for verdict in ('fail', 'blocked', 'skipped'):
            with self.subTest(verdict=verdict):
                self.result(verdict=verdict)
                self.assert_summary(1, 1, 0)

    def test_stale_result_is_visible_and_does_not_fail(self):
        self.table()
        self.result(commit='b'*40)
        lines = self.assert_summary(1, 1, 0)
        self.assertTrue(any('stale; ran on '+('b'*40) in line for line in lines))
        self.assertFalse(any(GUARDS.RULE.search(line) for line in lines))

    def test_newest_filename_wins_even_when_older_result_passes(self):
        self.table()
        self.result(file='20261116T170000Z.json')
        self.result(verdict='fail', file='20261116T170001Z.json')
        os.utime('test/walk/results/20261116T170000Z.json', (2000000000, 2000000000))
        self.assert_summary(1, 1, 0)

    def test_staged_and_untracked_results_are_not_checked_in(self):
        self.table()
        self.result()
        path = Path('test/walk/results/20261116T170001Z.json')
        path.write_text('{"invalid": true}')
        self.assert_summary(1, 1, 1)
        self.git('add', str(path))
        self.assert_summary(1, 1, 1)

    def test_working_edit_does_not_replace_committed_result(self):
        self.table()
        self.result()
        Path('test/walk/results/20261116T170000Z.json').write_text('invalid json')
        self.assert_summary(1, 1, 1)

    def test_staged_deletion_does_not_hide_committed_result(self):
        self.table()
        self.result(commit='b'*40)
        self.git('rm', 'test/walk/results/20261116T170000Z.json')
        self.assertTrue(any('stale; ran on' in line for line in self.assert_summary(1, 1, 0)))

    def test_unknown_step_is_refused_in_current_result(self):
        self.table()
        self.result(name='missing')
        with self.assertRaisesRegex(ValueError, 'undefined walk step'):
            self.analyze()

    def test_stale_result_with_removed_step_is_advisory(self):
        self.table()
        self.result(name='removed', commit='b'*40)
        lines = self.assert_summary(1, 1, 0)
        self.assertTrue(any('stale; ran on' in line for line in lines))
        self.assertFalse(any(GUARDS.RULE.search(line) for line in lines))

    def test_stale_result_without_walk_is_advisory(self):
        self.result(commit='b'*40)
        lines = self.assert_summary(0, 0, 0)
        self.assertTrue(any('stale; ran on' in line for line in lines))
        self.assertFalse(any(GUARDS.RULE.search(line) for line in lines))

    def test_result_without_walk_cannot_supply_a_definition(self):
        self.result()
        with self.assertRaisesRegex(ValueError, 'undefined walk step'):
            self.analyze()

    def test_malformed_results_are_refused(self):
        self.table()
        base = self.result()
        values = [None, {**base, 'commit': 'v1'}, {**base, 'steps': {}},
                  {**base, 'extra': True}, {**base, 'steps': base['steps']*2}]
        for field, value in (('verdict', 'PASS'), ('evidence_sha256', ''), ('evidence_sha256', 1)):
            values.append({**base, 'steps': [{**base['steps'][0], field: value}]})
        for index, value in enumerate(values):
            with self.subTest(index=index):
                self.write('test/walk/results/20261116T170000Z.json', json.dumps(value))
                self.git('commit', '-qm', 'Record malformed evidence')
                with self.assertRaisesRegex(ValueError, 'expected commit|invalid, duplicate'):
                    self.analyze()

    def test_table_rejects_duplicate_empty_and_dynamic_entries(self):
        for names in (('push', 'push'), ('',), (' ',)):
            with self.subTest(names=names):
                self.table(names)
                with self.assertRaisesRegex(ValueError, 'nonempty and unique'):
                    self.analyze()
        self.write('test/walk/walklib.py', 'STEPS = build_steps()\n')
        with self.assertRaisesRegex(ValueError, 'literal STEPS'):
            self.analyze()
