"""Pinned-source reproduction with reference paths supplied outside the tree."""
import json
import os
from pathlib import Path
import runpy
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

PARITY = runpy.run_path(str(Path(__file__).with_name("parity.py")))


class RegenerationTests(unittest.TestCase):
    def test_external_source_aliases_match_neutral_disposition_selectors(self):
        data = {"aliases": {"source-addon": "agent", "fixtureclient": "agentclient"},
                "public_replacements": {}, "client_paths": {}, "source_folders": []}
        with tempfile.TemporaryDirectory() as directory:
            file = Path(directory)/"source-map.json"
            file.write_text(json.dumps(data))
            with patch.dict(os.environ, {"OBERTH_PARITY_SOURCE_MAP": str(file)}):
                neutral = PARITY["neutral_source"]
                for name in ("agent/skills/client.md", "agentclient", "AgentClient"):
                    self.assertEqual(name, neutral(name))
                self.assertEqual("agent/path", neutral("source-addon/path"))
                self.assertEqual("Agentclient", neutral("FixtureClient"))
                self.assertEqual("agentclient", neutral("fixtureclient"))

    def test_source_map_must_resolve_outside_the_repository(self):
        with tempfile.TemporaryDirectory() as directory:
            file = Path(directory)/"source-map.json"
            file.write_text("{}")
            with patch.dict(PARITY["source_map"].__globals__, {"ROOT": Path(directory)}):
                with self.assertRaisesRegex(AssertionError, "outside the repository"):
                    PARITY["read_source_map"](str(file))

    @unittest.skipUnless(all(os.environ.get(key) for key in
                         ("OBERTH_PARITY_REFERENCE", "OBERTH_PARITY_REWRITE", "OBERTH_PARITY_SOURCE_MAP")),
                         "supply reference, rewrite and external source map for the pinned oracle")
    def test_pinned_sources_reproduce_committed_inventory(self):
        root = PARITY["ROOT"]
        options = SimpleNamespace(
            reference=Path(os.environ["OBERTH_PARITY_REFERENCE"]),
            rewrite=Path(os.environ["OBERTH_PARITY_REWRITE"]),
            dispositions=root / "docs/plans/dispositions.json",
            sweeps=root / "docs/plans/sweeps.json", sweep_dir=None,
        )
        actual, *_ = PARITY["regenerate_inventory"](options)
        self.assertEqual((root / "docs/plans/PARITY.md").read_text(), actual)


if __name__ == "__main__":
    unittest.main()
