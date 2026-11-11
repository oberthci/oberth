#!/usr/bin/env python3
"""Tests for the migration-sequence check (standard library only)."""

import importlib.util
from pathlib import Path
import unittest

SPEC = importlib.util.spec_from_file_location("check_migration_sequence", Path(__file__).with_name("check-migration-sequence.py"))
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)

A = "20261104022000-migration-ids"
B = "20261105050000-execution-target-custody"
C = "20261109220004-agent-events-" + "5" * 32


class MigrationSequenceTests(unittest.TestCase):
    def test_latest_plus_one_passes(self):
        self.assertEqual(MODULE.check([A, B, C], [A, B, C], base=[A, B]), [])

    def test_repository_tree_passes(self):
        sequence = MODULE.read_sequence((MODULE.ROOT / MODULE.SEQUENCE).read_text(encoding="utf-8"))
        self.assertEqual(MODULE.check(MODULE.migration_files(MODULE.ROOT), sequence), [])

    def test_migration_older_than_main_latest_is_refused(self):
        older = "20261104130000-queue-cap"
        problems = MODULE.check([A, B, C, older], [A, B, C, older])
        self.assertTrue(any("numbered after main's latest" in problem for problem in problems), problems)
        problems = MODULE.check([A, B, C, older], [A, B, older, C], base=[A, B, C])
        self.assertTrue(any("unchanged prefix" in problem for problem in problems), problems)

    def test_collisions_are_refused(self):
        twin = "20261105050000-other-slug"
        problems = MODULE.check([A, B, twin], [A, B, twin])
        self.assertTrue(any("shares timestamp" in problem for problem in problems), problems)
        problems = MODULE.check([A, B], [A, B, B])
        self.assertTrue(any("listed twice" in problem for problem in problems), problems)

    def test_unlisted_and_missing_files_are_refused(self):
        problems = MODULE.check([A, B, C], [A, B])
        self.assertTrue(any("not listed" in problem for problem in problems), problems)
        problems = MODULE.check([A], [A, B])
        self.assertTrue(any("has no migration file" in problem for problem in problems), problems)
        problems = MODULE.check([A, "54"], [A, "54"])
        self.assertTrue(any("not a timestamped migration ID" in problem for problem in problems), problems)


if __name__ == "__main__":
    unittest.main()
