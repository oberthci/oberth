"""Offline controls for the pinned guest disk, using actual compressed bytes."""
import gzip
import hashlib
import importlib.util
import json
import subprocess
import sys
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


SPEC = importlib.util.spec_from_file_location("prepare", Path(__file__).with_name("prepare.py"))
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class GuestDiskTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        self.data = b"reviewed raw guest disk" * 100
        self.compressed = gzip.compress(self.data, mtime=0)
        self.archive = self.root / "archive.gz"
        self.archive.write_bytes(self.compressed)
        self.output = self.root / "disk.img"
        self.pin = dict(format="raw.gz", architecture="amd64", url="https://example.invalid/disk.gz",
                        sha256=hashlib.sha256(self.compressed).hexdigest(),
                        sha512=hashlib.sha512(self.compressed).hexdigest(),
                        max_download_bytes=len(self.compressed), max_disk_bytes=len(self.data))

    def test_verifies_and_unpacks_exact_pinned_bytes(self):
        receipt = MODULE.prepare(self.pin, self.output, self.archive)
        self.assertEqual(self.output.read_bytes(), self.data)
        self.assertEqual(receipt["disk_sha256"], hashlib.sha256(self.data).hexdigest())
        self.assertEqual(receipt["disk_bytes"], len(self.data))
        self.assertEqual(sorted(p.name for p in self.root.iterdir()), ["archive.gz", "disk.img"])

    def test_changed_archive_refuses_without_publishing_disk(self):
        self.archive.write_bytes(gzip.compress(b"substituted disk", mtime=0))
        with self.assertRaisesRegex(ValueError, "checksum differs"):
            MODULE.prepare(self.pin, self.output, self.archive)
        self.assertFalse(self.output.exists())

    def test_each_digest_is_required(self):
        for name in ("sha256", "sha512"):
            with self.subTest(name=name):
                with self.assertRaisesRegex(ValueError, "checksum differs"):
                    MODULE.prepare({**self.pin, name: "0" * len(self.pin[name])}, self.output, self.archive)
                self.assertFalse(self.output.exists())

    def test_bounds_compressed_and_expanded_bytes(self):
        for name in ("max_download_bytes", "max_disk_bytes"):
            with self.subTest(name=name):
                with self.assertRaisesRegex(ValueError, "size bound"):
                    MODULE.prepare({**self.pin, name: 1}, self.output, self.archive)
                self.assertFalse(self.output.exists())

    def test_refuses_existing_output_and_symlinks(self):
        self.output.write_bytes(b"existing")
        with self.assertRaisesRegex(ValueError, "output exists"):
            MODULE.prepare(self.pin, self.output, self.archive)
        self.assertEqual(self.output.read_bytes(), b"existing")
        self.output.unlink()
        self.output.symlink_to(self.root / "missing")
        with self.assertRaisesRegex(ValueError, "output exists"):
            MODULE.prepare(self.pin, self.output, self.archive)
        self.assertFalse((self.root / "missing").exists())

    def test_https_fetch_uses_the_same_digest_check(self):
        with self.archive.open("rb") as response:
            response.geturl = lambda: self.pin["url"]
            with patch.object(MODULE.urllib.request, "urlopen", return_value=response) as fetch:
                MODULE.prepare(self.pin, self.output)
        fetch.assert_called_once_with(self.pin["url"], timeout=60)
        self.assertEqual(self.output.read_bytes(), self.data)

    def test_copied_script_uses_the_passed_pin(self):
        # The walk's disk Job runs a copy of this script with the repository pin.
        script = self.root / "prepare.py"
        script.write_bytes(Path(MODULE.__file__).read_bytes())
        for digest, code in ((self.pin["sha256"], 0), ("0" * 64, 1)):
            with self.subTest(code=code):
                pin = self.root / "pin.json"
                pin.write_text(json.dumps({**self.pin, "sha256": digest}))
                run = subprocess.run([sys.executable, "-I", script, "--pin", pin, "--archive", self.archive,
                                      "--output", self.output], capture_output=True, text=True, check=False)
                self.assertEqual(run.returncode, code, run.stderr)
                self.assertEqual(self.output.exists(), code == 0)
                if code == 0:
                    self.assertEqual(json.loads(run.stdout)["disk_bytes"], len(self.data))
                    self.output.unlink()

    def test_repository_pin_is_versioned_and_bounded(self):
        pin = json.loads((Path(__file__).resolve().parents[2] / MODULE.PIN_FILE).read_text())
        self.assertIn("/v" + pin["version"] + "/", pin["url"])
        self.assertRegex(pin["sha256"], r"^[a-f0-9]{64}$")
        self.assertRegex(pin["sha512"], r"^[a-f0-9]{128}$")
        self.assertLessEqual(pin["max_download_bytes"], 512 * 1024**2)
        self.assertLessEqual(pin["max_disk_bytes"], 8 * 1024**3)


if __name__ == "__main__":
    unittest.main()
