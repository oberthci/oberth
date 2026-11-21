"""A fetched client preserves another installation's executable."""
import unittest

from fetch_support import FetchCase, serve


class FetchPreservation(FetchCase):
    def test_foreign_executable_is_refused_then_preserved_by_force(self):
        target = self.home / ".local/bin/oberth"
        target.parent.mkdir(parents=True)
        target.write_bytes(b"#!/bin/sh\nexit 42\n")
        target.chmod(0o751)
        before = target.read_bytes()
        with serve(self.files, self.requests) as origin:
            refused = self.fetch(origin)
            self.assertNotEqual(refused.returncode, 0)
            self.assertIn(b"Another oberth, not installed by this fetcher", refused.stdout)
            self.assertEqual(self.requests, [])
            self.assertEqual(list(target.parent.iterdir()), [target])
            self.assertEqual(target.read_bytes(), before)
            installed = self.fetch(origin, "--force")
        self.assertEqual(installed.returncode, 0, installed.stderr)
        previous = target.with_name("oberth.previous")
        self.assertTrue(previous.is_file(), "forced fetch discarded the previous executable")
        self.assertEqual(previous.read_bytes(), before)
        self.assertEqual(previous.stat().st_mode & 0o777, 0o751)
        self.assertIn(str(previous).encode(), installed.stdout)
        self.assertTrue(target.is_symlink())
        self.assertEqual(target.read_bytes(), self.binary)

    def test_existing_previous_copy_refuses_before_any_write(self):
        target = self.home / ".local/bin/oberth"
        target.parent.mkdir(parents=True)
        target.write_bytes(b"another executable")
        previous = target.with_name("oberth.previous")
        previous.write_bytes(b"saved executable")
        before = {file.name: file.read_bytes() for file in target.parent.iterdir()}
        with serve(self.files, self.requests) as origin:
            result = self.fetch(origin, "--force")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"Move oberth.previous aside", result.stderr)
        self.assertEqual(self.requests, [])
        self.assertEqual({file.name: file.read_bytes() for file in target.parent.iterdir()}, before)


if __name__ == "__main__":
    unittest.main()
