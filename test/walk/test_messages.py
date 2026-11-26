"""Offline deciding checks for distinct message evidence."""
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch
from walkevidence import redact
from walklib import Walk
from walkmessages import collect, messages


class MessageTests(unittest.TestCase):
    def test_wraps_preserve_whole_words_paths_and_paragraph_boundaries(self):
        self.assertEqual(messages(("x" * 80 + "\n  next\n").encode()), ["x" * 80 + " next"])
        text = "Use /tmp/config.json to configure the installation and keep the complete message readable in the evidence table."
        rendered = ("Use\n  /tmp/config.json\n  to configure the installation and keep the complete message readable in the\n"
                    "  evidence table.\n")
        self.assertEqual(messages(rendered.encode()), [text])
        text = "A " + "word " * 14 + "last"
        self.assertEqual(messages((text + "\n  continuation\n\n  indented\n").encode()),
                         [text + " continuation", "  indented"])

    def test_only_cli_is_collected_deduplicated_redacted_and_untruncated(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            commands = []
            for index, program in enumerate(("oberth", "git", "kubectl", str(root / "oberth"))):
                stem = root / str(index)
                stem.with_suffix(".stdout").write_text("é" * 5000 + " sensitive-value\n")
                stem.with_suffix(".stderr").write_text("Refused.\n")
                commands.append({"argv": [program], "capture": str(index), "step": "probe", "exit_code": 1})
            session = SimpleNamespace(private=root, binary=root / "oberth", commands=commands,
                clean=lambda text, **kwargs: redact(text, ["sensitive-value"], [], **kwargs))
            rows = collect(session)
            self.assertEqual(len(rows), 2)
            self.assertEqual(rows[0]["text"], "é" * 5000 + " <private>")
            self.assertEqual(rows[0]["length"], 5016)
            self.assertEqual(rows[0]["utf8_bytes"], 10016)
            self.assertEqual([row["count"] for row in rows], [2, 2])
            self.assertEqual(rows[1]["text"], "Refused.")

    def test_final_publication_writes_the_message_table_before_private_cleanup(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "command.stdout").write_text("Complete message.\n")
            (root / "command.stderr").write_text("")
            session = SimpleNamespace(private=root, secrets=[], commands=[{
                "argv": ["oberth", "install"], "step": "install", "capture": "command", "exit_code": 0}],
                clean=lambda text, **kwargs: redact(text, [], [], **kwargs))
            walk = SimpleNamespace(out=root, s=session, flush=Mock(return_value={"verdict": "PASS"}))
            with patch("walklib.cleanup.close_local"):
                self.assertEqual(Walk.publish(walk)["verdict"], "PASS")
            self.assertIn("Complete message.", (root / "messages.json").read_text())
