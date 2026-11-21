"""Check user command examples against the executable's own catalogue."""
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[1]


def examples(text):
    """Read fenced commands and inline command references, including continuations."""
    fenced, pending, start = False, "", 0
    for number, line in enumerate(text.splitlines(), 1):
        stripped = line.strip()
        if stripped.startswith("```"):
            if pending:
                raise ValueError("unfinished command continuation")
            fenced = not fenced
            continue
        if fenced:
            if not pending:
                start = number
            pending += stripped.removesuffix("\\") + " "
            if stripped.endswith("\\"):
                continue
            yield start, pending.strip(), True
            pending = ""
        else:
            for match in re.finditer(r"`([^`\n]+)`", line):
                yield number, match[1], False
    if fenced or pending:
        raise ValueError("unfinished fenced example")


def command_words(example):
    example = example.removeprefix("$ ")
    example = example.replace("<absolute-oberth-path>", "oberth")
    if not re.match(r"(?:[A-Za-z_]\w*=|oberth\s)", example):
        return []
    example = re.sub(r"<[a-zA-Z][\w/-]*>", "DOC_VALUE", example)
    words = shlex.split(example, comments=True)
    while words and re.match(r"^[A-Za-z_]\w*=", words[0]):
        words.pop(0)
    return words if words[:1] == ["oberth"] else []


def command_error(words, catalogue, complete):
    paths = [name for name in catalogue if words[1:1 + len(name.split())] == name.split()]
    if not paths:
        return "unknown command: " + " ".join(words)
    name = max(paths, key=lambda path: len(path.split()))
    syntax = catalogue[name]
    args, index = 0, len(name.split()) + 1
    while index < len(words):
        word = words[index]
        if word == "--":
            args += len(words) - index - 1
            break
        if word in ("<", ">", ">>", "2>"):
            index += 2
            continue
        if word in (";", "&&", "||", "|"):
            return "use one command per example line"
        if word.startswith("-"):
            flag, equal, _ = word.partition("=")
            if flag not in syntax["flags"]:
                return f"{name}: unknown flag {flag}"
            arity = syntax["flags"][flag]
            if arity and not equal:
                index += 1
                if index == len(words):
                    return f"{name}: missing value for {flag}"
        else:
            args += 1
        index += 1
    minimum, maximum = syntax["args"]
    if complete and (args < minimum or (maximum >= 0 and args > maximum)):
        return f"{name}: {args} positional arguments outside {minimum}..{maximum}"
    return ""


def quoted_messages(text):
    for line in text.splitlines():
        if line.startswith("> "):
            yield line[2:]
        reason = re.search(r"reason is `([^`]+)`", line)
        if reason:
            yield reason[1]
        for match in re.finditer(r"`([^`\n]+)`", line):
            message = match[1]
            if message[:1].isupper() and message.endswith(".") and " " in message:
                yield message


def document_checks(text, catalogue, binary):
    errors, commands, messages = [], 0, 0
    for line, example, complete in examples(text):
        words = command_words(example)
        if words:
            commands += 1
            error = command_error(words, catalogue, complete)
            if error:
                errors.append(f"line {line}: {error}")
    for message in quoted_messages(text):
        messages += 1
        if message.encode() not in binary:
            errors.append("quoted message absent from binary: " + message)
    return errors, commands, messages


class DocumentationCommands(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        scratch = Path(os.environ.get("GOTMPDIR", Path.home() / ".cache/go-tmp"))
        scratch.mkdir(parents=True, exist_ok=True)
        cls.directory = tempfile.TemporaryDirectory(prefix="oberth-docs-", dir=scratch)
        cls.addClassCleanup(cls.directory.cleanup)
        binary = Path(cls.directory.name) / "oberth"
        subprocess.run(["go", "build", "-p=1", "-buildvcs=false", "-o", str(binary), "./cmd/oberth"],
                       cwd=ROOT, check=True, capture_output=True, timeout=180)
        result = subprocess.run([str(binary), "help", "-o", "json"], cwd=ROOT,
                                check=True, capture_output=True, text=True, timeout=15)
        cls.catalogue = json.loads(result.stdout)["commands"]
        cls.binary = binary.read_bytes()

    def test_documented_commands_and_messages_exist_in_binary(self):
        commands, messages = 0, 0
        for path in sorted((ROOT / "docs").glob("*.md")) + [ROOT / "README.md"]:
            text = path.read_text()
            errors, command_count, message_count = document_checks(text, self.catalogue, self.binary)
            commands += command_count
            messages += message_count
            with self.subTest(file=path.name):
                self.assertEqual(errors, [])
        self.assertGreater(commands, 50, "command extraction lost the guide examples")
        self.assertGreater(messages, 5, "message extraction lost the guide quotations")

    def test_guard_refuses_command_flag_and_message_drift(self):
        for text in ("oberth schedules list", "oberth upgrade --chart local", "oberth secretstore setup --print", "oberth install --context",
                     "OBERTH_PROFILE=uplink oberth repo list --profile uplink",
                     "OBERTH_PROFILE=uplink oberth repo add forge-url --profile uplink"):
            with self.subTest(example=text):
                self.assertTrue(command_error(command_words(text), self.catalogue, True))
        errors, _, _ = document_checks("`Secret documentation drift must fail.`", self.catalogue, self.binary)
        self.assertEqual(errors, ["quoted message absent from binary: Secret documentation drift must fail."])
        self.assertEqual(list(quoted_messages("For a branch containing `{{`, the reason is `schedule ref contains template syntax`:")),
                         ["schedule ref contains template syntax"])

    def test_joined_checkout_section_checks_profile_commands_and_messages(self):
        text = (ROOT / "docs/agents.md").read_text().split("## Connect the tools", 1)[0]
        errors, commands, messages = document_checks(text, self.catalogue, self.binary)
        self.assertEqual(errors, [])
        self.assertGreaterEqual(commands, 7)
        self.assertGreaterEqual(messages, 2)
        for action in ("list", "add <forge-url>"):
            example = "OBERTH_PROFILE=<uplink-profile> oberth repo " + action
            self.assertIn(example, text)
            words = command_words(example)
            self.assertEqual(words[:2], ["oberth", "repo"])
            self.assertEqual(command_error(words, self.catalogue, True), "")

    def test_shell_examples_keep_quotes_continuations_and_redirects_as_data(self):
        text = "```sh\noberth secret put --repo 'forge/team/app' \\\n  --file .oberth/release.yaml --step release key < <value-file>\n```"
        rows = list(examples(text))
        self.assertEqual(len(rows), 1)
        self.assertEqual(command_error(command_words(rows[0][1]), self.catalogue, True), "")
        for text in ("```sh\noberth init \\", "```sh\noberth init\n"):
            with self.subTest(example=text), self.assertRaises(ValueError):
                list(examples(text))
