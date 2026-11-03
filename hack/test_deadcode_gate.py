#!/usr/bin/env python3
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location('deadcode_gate', Path(__file__).with_name('check-deadcode.py'))
gate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(gate)


class DeadcodeGateTests(unittest.TestCase):
    def test_new_debt_is_rejected(self):
        report = [{'Path': 'example/pkg', 'Funcs': [{'Name': 'Retired'}]}]
        self.assertEqual(gate.check(report, {}), (['example/pkg.Retired'], []))

    def test_exact_exception_requires_no_extra_debt(self):
        report = [{'Path': 'example/pkg', 'Funcs': [{'Name': 'Old'}, {'Name': 'New'}]}]
        self.assertEqual(gate.check(report, {'example/pkg.Old': 'tracked removal'}), (['example/pkg.New'], []))

    def test_removed_debt_requires_allowlist_cleanup(self):
        self.assertEqual(gate.check([], {'example/pkg.Old': 'tracked removal'}), ([], ['example/pkg.Old']))

    def test_empty_report_is_valid_only_with_empty_allowlist(self):
        self.assertEqual(gate.check([], {}), ([], []))

    def test_bad_input_fails_closed(self):
        for report, allowed in [(None, {}), ({}, {}), ([{}], {}), ([], {'symbol': ''})]:
            with self.assertRaises((ValueError, KeyError)):
                gate.check(report, allowed)


if __name__ == '__main__':
    unittest.main()
