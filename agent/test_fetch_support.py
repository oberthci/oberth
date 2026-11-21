"""Missing fetch tools must be visible without suppressing installed variants."""
import shutil
import unittest
from unittest.mock import patch

from fetch_support import FetchCase


class ToolSelection(unittest.TestCase):
    def selection(self, names, installed):
        selected = []

        class Selection(unittest.TestCase):
            def runTest(self):
                selected.extend(FetchCase.available(self, *names))

        result = unittest.TestResult()
        with patch("fetch_support.shutil.which", side_effect=lambda name: "/tools/" + name if name in installed else None):
            Selection().run(result)
        return selected, result

    def test_missing_downloader_and_checksum_each_record_a_named_skip(self):
        selected, result = self.selection(("curl", "wget", "sha256sum", "shasum"), {"curl", "shasum"})
        self.assertEqual(selected, ["curl", "shasum"])
        self.assertEqual([reason for _, reason in result.skipped],
                         ["wget is not installed", "sha256sum is not installed"])
        self.assertTrue(all("tool=" in str(test) for test, _ in result.skipped))
        self.assertTrue(result.wasSuccessful())

    def test_all_installed_tools_run_without_skips(self):
        selected, result = self.selection(("curl", "wget"), {"curl", "wget"})
        self.assertEqual(selected, ["curl", "wget"])
        self.assertEqual(result.skipped, [])
        self.assertTrue(result.wasSuccessful())

    def test_no_installed_variant_still_fails_after_named_skips(self):
        selected, result = self.selection(("curl", "wget"), set())
        self.assertEqual(selected, [])
        self.assertEqual(len(result.skipped), 2)
        self.assertEqual(len(result.failures), 1)
        self.assertIn("Fetch contracts need curl or wget", result.failures[0][1])

    def test_repeated_tool_paths_do_not_repeat_variant_skips(self):
        ran = []

        class Selection(FetchCase):
            def runTest(self):
                self.assertEqual(self.available("curl", "wget"), ["curl"])
                self.assertEqual(self.available("sha256sum", "shasum"), ["shasum"])
                first = self.tools()
                self.assertEqual(self.tools(), first)
                ran.append(True)

        which = shutil.which
        result = unittest.TestResult()
        with patch("fetch_support.shutil.which", side_effect=lambda name: None if name in {"wget", "sha256sum"} else which(name)):
            Selection().run(result)
        self.assertEqual(ran, [True])
        self.assertEqual([reason for _, reason in result.skipped],
                         ["wget is not installed", "sha256sum is not installed"])
        self.assertTrue(result.wasSuccessful(), result.errors + result.failures)
