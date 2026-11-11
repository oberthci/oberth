#!/usr/bin/env python3
"""Regression tests for independent migration generation (standard library only)."""

from datetime import datetime, timezone
import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("new_migration", Path(__file__).with_name("new-migration.py"))
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class MigrationGeneratorTests(unittest.TestCase):
    def test_same_second_same_slug_across_branches(self):
        with tempfile.TemporaryDirectory() as root, patch.object(MODULE, "datetime") as clock:
            clock.now.return_value = datetime(2026, 11, 4, 2, 20, tzinfo=timezone.utc)
            one = MODULE.create_migration(Path(root) / "branch-a", "add-run-index")
            two = MODULE.create_migration(Path(root) / "branch-b", "add-run-index")
            self.assertNotEqual(one.name, two.name)
            self.assertRegex(one.name, r"^20261104022000-add-run-index-[a-f0-9]{32}\.sql$")
            clock.now.assert_called_with(timezone.utc)
            self.assertIn("-- Purpose: add run index.", one.read_text())
            self.assertIn("-- Rollback:", one.read_text())

    def test_local_collision_never_overwrites(self):
        with tempfile.TemporaryDirectory() as root, patch.object(MODULE, "datetime") as clock:
            clock.now.return_value = datetime(2026, 11, 4, 2, 20, tzinfo=timezone.utc)
            with patch.object(MODULE.secrets, "token_hex", side_effect=["a" * 32, "a" * 32, "b" * 32]):
                first = MODULE.create_migration(root, "add-index")
                first.write_text("already committed\n")
                second = MODULE.create_migration(root, "add-index")
            self.assertNotEqual(first, second)
            self.assertEqual(first.read_text(), "already committed\n")

    def test_sequence_appends_the_new_id(self):
        with tempfile.TemporaryDirectory() as root:
            sequence = Path(root) / "migration-sequence.txt"
            sequence.write_text("20261104022000-migration-ids\n")
            migration = MODULE.create_migration(Path(root) / "migrations", "add-index")
            MODULE.append_sequence(sequence, migration)
            self.assertEqual(sequence.read_text().splitlines(), ["20261104022000-migration-ids", migration.stem])

    def test_invalid_slugs_make_no_files(self):
        for slug in ("", "../escape", "x/y", "54", "Upper", "two words", "-leading", "trailing-", "a--b", "x" * 41):
            with self.subTest(slug=slug), tempfile.TemporaryDirectory() as root:
                target = Path(root) / "migrations"
                with self.assertRaises(ValueError):
                    MODULE.create_migration(target, slug)
                self.assertFalse(target.exists())


if __name__ == "__main__":
    unittest.main()
