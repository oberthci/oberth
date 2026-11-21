"""Offline candidate handoff; no guest or container is started here."""
import hashlib
import io
import json
from pathlib import Path
import tarfile
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch
from walkassert import Failure
import walkcandidate


class CandidateTests(unittest.TestCase):
    def fixture(self, root):
        directory = root / "dist"
        directory.mkdir()
        archive = io.BytesIO()
        with tarfile.open(fileobj=archive, mode="w:gz") as output:
            body = b"name: oberth\n"
            header = tarfile.TarInfo("oberth/Chart.yaml")
            header.size = len(body)
            output.addfile(header, io.BytesIO(body))
        images = {}
        for name in ("oberth", "oberth-source-plugin"):
            (directory / (name + ".oci")).mkdir()
        for name, body in {"oberth-linux-amd64": b"CLI", "oberth.tgz": archive.getvalue(),
                           "release.json": json.dumps({"version": "v1.2.3", "commit": "a" * 40,
                                                       "images": images}).encode()}.items():
            (directory / name).write_bytes(body)
        (directory / "SHA256SUMS").write_text("".join(
            hashlib.sha256(p.read_bytes()).hexdigest() + "  " + p.name + "\n"
            for p in sorted(directory.iterdir()) if p.is_file()))
        return SimpleNamespace(args=SimpleNamespace(candidate=directory, version=""),
                               private=root, binary=root / "oberth", data=SimpleNamespace(),
                               config={"registry_http": "http://localhost:5001"},
                               run=Mock(return_value=(0, b"v1.2.3", b"")))

    def test_candidate_checks_all_bytes_before_execution_and_uses_local_chart(self):
        with tempfile.TemporaryDirectory() as temporary, patch("walksetup.tools_image"):
            s = self.fixture(Path(temporary))
            result = walkcandidate.target(s)
            self.assertTrue(result["unsigned_local_chart"])
            self.assertEqual(s.binary.read_bytes(), b"CLI")
            self.assertTrue((s.private / "candidate-chart/oberth/Chart.yaml").is_file())
            copies = [c.args[0] for c in s.run.call_args_list if c.args[0][0] == "skopeo"]
            self.assertEqual(len(copies), 2)
            self.assertTrue(all("--preserve-digests" in c and "--all" in c for c in copies))

    def test_candidate_corruption_or_extra_file_stops_before_execution(self):
        for extra in (False, True):
            with self.subTest(extra=extra), tempfile.TemporaryDirectory() as temporary, patch("walksetup.tools_image"):
                s = self.fixture(Path(temporary))
                (s.args.candidate / ("extra" if extra else "oberth-linux-amd64")).write_bytes(b"changed")
                with self.assertRaises(Failure):
                    walkcandidate.target(s)
                s.run.assert_not_called()
                self.assertFalse(s.binary.exists())
