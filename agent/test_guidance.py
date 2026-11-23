"""Check first-run commands and the server's single loop authority for drift."""
import copy
import json
import functools
import os
import subprocess
import tempfile
from pathlib import Path
import re
import shlex
import unittest


ROOT = Path(__file__).resolve().parent.parent
FIELD_NAMES = set("after_id next_after_id timeout_s failed_step latest_id object_sha commit_sha timed_out retry_hint".split())


@functools.lru_cache(maxsize=1)
def command_table():
    executable = os.environ.get("OBERTH_CLI")
    if executable:
        return json.loads(subprocess.check_output([executable, "help", "-o", "json"], text=True))["commands"]
    with tempfile.TemporaryDirectory(prefix="oberth-guidance-") as directory:
        executable = str(Path(directory) / "oberth")
        subprocess.run(["go", "build", "-p=1", "-o", executable, "./cmd/oberth"], cwd=ROOT, check=True)
        return json.loads(subprocess.check_output([executable, "help", "-o", "json"], text=True))["commands"]


def server_instructions():
    return (ROOT / "internal/api/rpc.go").read_text().split("const agentInstructions = `", 1)[1].split("`", 1)[0]


def check_command(command, commands):
    names = [name for name in commands if command == name or command.startswith(name + " ")]
    if not names:
        raise ValueError("unknown command: " + command)
    name = max(names, key=len)
    row = commands[name]
    args = shlex.split(command[len(name):])
    positionals = 0
    while args:
        token = args.pop(0)
        if token == "<":
            break
        if token.startswith("--"):
            flag, equal, _ = token.partition("=")
            if flag not in row["flags"]:
                raise ValueError("unknown flag for " + name + ": " + flag)
            if row["flags"][flag] and not equal:
                if not args or args.pop(0).startswith("--"):
                    raise ValueError("flag value missing: " + flag)
        else:
            positionals += 1
    if not row["args"][0] <= positionals <= row["args"][1]:
        raise ValueError("invalid argument count for " + name)


def check_commands(text, commands):
    reference = r"(?<![\w/-])oberth[ \t]+([a-z][a-z-]*(?:[ \t]+[a-z][a-z-]*)?)"
    for code in re.findall(r"`([^`]+)`", text):
        match = re.search(reference, code)
        if match:
            check_command(code[match.start() + len("oberth "):], commands)
    prose = re.sub(r"`[^`]+`", "", text)
    for match in re.finditer(reference, prose):
        remainder = prose[match.start():].split("\n", 1)[0]
        if "<" in remainder and ">" in remainder:
            check_command(remainder[len("oberth "):remainder.rfind(">") + 1], commands)
        words = match.group(1).split()
        if words[0] not in commands and not any(name.startswith(words[0] + " ") and words == name.split() for name in commands):
            raise ValueError("unknown command in prose: " + match.group(0))
    for line in text.splitlines():
        # Commands own their inline flags; prose must identify that command too.
        prose_line = re.sub(r"`[^`]*\boberth\b[^`]*`|`sh [^`]*fetch-oberth[^`]*`", "", line)
        flags = re.findall(r"--[a-z][a-z-]*", prose_line)
        if not flags:
            continue
        context = re.search(r"(?:on|to|for) (install|repo add)\b", prose_line)
        if not context or any(flag not in commands[context.group(1)]["flags"] for flag in flags):
            raise ValueError("unknown or unattributed prose flag")


REGISTRATION_RULES = (
    "absolute executable path printed by fetch-oberth",
    "Reject a checkout executable or a relative path.",
    "register exactly its absolute executable path",
)


def check_registration(skill):
    if any(rule not in skill for rule in REGISTRATION_RULES):
        raise ValueError("registration must use the verified absolute executable")


def check_guidance(skill, instructions, commands=None):
    check_registration(skill)
    commands = command_table() if commands is None else commands
    declarations = "\n".join(p.read_text() for p in (ROOT / "internal/apicontract").glob("*.go"))
    tools = set(re.findall(r'tool:"([a-z_]+)"', declarations))
    enums = (ROOT / "internal/stepmodel/enums.go").read_text()
    actions = set(re.findall(r'Retry\w+\s+RetryAction\s+=\s+"([a-z_]+)"', enums))
    for text in (skill, instructions):
        check_commands(text, commands)
        references = set(re.findall(r"\b[a-z][a-z0-9]*(?:_[a-z0-9]+)+\b", text))
        if references - tools - FIELD_NAMES - actions:
            raise ValueError("unknown tool or field: " + str(references - tools - FIELD_NAMES - actions))
        if re.search(r"\bsleep\b|\bpoll(?:ing)?\b|every\s+(?:\d+\s*s\b|minute\b)|until (?:it passes|green)", text, re.I):
            raise ValueError("timer or repeated snapshot guidance")
    if "events_wait" not in instructions or any(action not in instructions for action in actions):
        raise ValueError("missing event wait or retry action")
    if "once per code change" not in instructions or "then stop and tell the person" not in instructions:
        raise ValueError("missing rerun bound")
    if any(tool in skill for tool in ("events_wait", "run_rerun", "run_hold_list")):
        raise ValueError("duplicated server loop")


class GuidanceContract(unittest.TestCase):
    def test_install_summary_keeps_the_printed_web_signin_line(self):
        skill = (ROOT / "agent/skills/oberth/SKILL.md").read_text()
        apply = next(line for line in skill.splitlines() if "Apply after consent" in line)
        self.assertIn("Show the printed web-view sign-in line verbatim.", apply)

    def test_registration_requires_a_verified_absolute_executable(self):
        skill = (ROOT / "agent/skills/oberth/SKILL.md").read_text()
        check_registration(skill)
        for phrase in REGISTRATION_RULES:
            with self.subTest(missing=phrase), self.assertRaises(ValueError):
                check_registration(skill.replace(phrase, "use any executable"))

    def test_commands_and_flags_match_real_cli(self):
        skill = (ROOT / "agent/skills/oberth/SKILL.md").read_text()
        self.assertLess(len(skill.splitlines()), 100)
        self.assertTrue(skill.startswith("---\nname: oberth\ndescription:"))
        try:
            check_guidance(skill, server_instructions())
        except ValueError as error:
            self.fail(str(error))

    def test_reviewer_drifts_are_rejected(self):
        skill = (ROOT / "agent/skills/oberth/SKILL.md").read_text()
        instructions = server_instructions()
        drifts = {
            "extra subcommand": (skill + "\n`oberth run watch <run>`", instructions),
            "wrong prose flag": (skill + "\nPass `--yes` to repo add.", instructions),
            "retired subscription": (skill + "\nevents_subscribe", instructions),
            "retired history": (skill + "\nevents_since", instructions),
            "server history rename": (skill, instructions.replace("events_wait", "events_since")),
            "server wait rename": (skill, instructions.replace("the one wait", "the one events_subscribe")),
            "timer snapshot": (skill + "\nSleep 30 seconds and call run_get again until it passes", instructions),
            "minute loop": (skill + "\nevery minute until green", instructions),
            "rerun loop": (skill + "\nrerun again until green", instructions),
            "unquoted command": (skill + "\nRun oberth imaginary now.", instructions),
            "unquoted subcommand": (skill + "\nRun oberth run watch <run>.", instructions),
            "runs flag": (skill + "\n`oberth runs --follow`", instructions),
            "status flag": (skill + "\n`oberth status --watch`", instructions),
            "unknown prose flag": (skill + "\nUse --now.", instructions),
        }
        for name, (changed_skill, changed_instructions) in drifts.items():
            with self.subTest(drift=name), self.assertRaises(ValueError):
                check_guidance(changed_skill, changed_instructions)
        for name in ("removed command", "renamed flag"):
            changed = copy.deepcopy(command_table())
            if name == "removed command":
                del changed["join"]
            else:
                changed["install"]["flags"]["--cluster"] = changed["install"]["flags"].pop("--context")
            with self.subTest(drift=name), self.assertRaises(ValueError):
                check_guidance(skill, instructions, changed)

    def test_server_owns_wait_retry_and_assistance_rules(self):
        text = server_instructions()
        for phrase in ("next_after_id", "timed_out", "run.held", "run.finished", "retry_hint", "live availability", "run_hold_exec", "push the fix", "release with run_hold_abandon", "Never replay an exec", "unassisted", "verbatim", "unknown hint", "cancel and restart the one wait"):
            self.assertIn(phrase, text)
        for removed in ("run_continue", "run_hold_patch", "abandon_at"):
            self.assertNotIn(removed, text)
        for field in ("kinds", "own", "after_id", "timeout_s"):
            self.assertIn('json:"' + field, (ROOT / "internal/apicontract/observation_args.go").read_text())

    def test_release_origin_snapshot_matches_canonical_constant(self):
        result = subprocess.run(["python3", str(ROOT / "hack/release-origin.py"), "--check"], cwd=ROOT, text=True, capture_output=True, timeout=10)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_origin_generator_rejects_duplicate_assignments_and_missing_constant(self):
        script = (ROOT / "agent/skills/oberth/fetch-oberth").read_text()
        for suffix in ("\norigin=other\n", "\n  origin=other\n", "\n\torigin=${RELEASE_ORIGIN}\n", "\nversion_pattern=other\n", None):
            with self.subTest(suffix=suffix), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                for name in ("hack/release-origin.py", "internal/releaseverify/origin.go", "internal/workflowpolicy/release-version.pattern"):
                    target = root / name
                    target.parent.mkdir(parents=True, exist_ok=True)
                    target.write_text((ROOT / name).read_text())
                target = root / "agent/skills/oberth/fetch-oberth"
                target.parent.mkdir(parents=True)
                target.write_text(script + (suffix or ""))
                if suffix is None:
                    (root / "internal/releaseverify/origin.go").unlink()
                result = subprocess.run(["python3", str(root / "hack/release-origin.py"), "--check"], text=True, capture_output=True, timeout=10)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn("Traceback", result.stderr)
                self.assertLessEqual(len(result.stderr.strip()), 80)

    def test_message_catalogue_is_complete_and_short(self):
        script = (ROOT / "agent/skills/oberth/fetch-oberth").read_text()
        pairs = re.findall(r"fail '([^']*)' '([^']*)'", script)
        self.assertGreater(len(pairs), 15)
        self.assertEqual(script.count("fail '"), len(pairs))
        for reason in re.findall(r"lock_failed '([^']*)'", script):
            pairs.append((reason, "Check ~/.local/bin/.oberth-fetch-lock; retry after its owner exits."))
        for tool in ("openssl", "awk", "uname"):
            pairs.append(("Missing " + tool + ".", "Install it, then retry."))
        for reason, remedy in pairs:
            self.assertLessEqual(len(reason), 80)
            self.assertLessEqual(len(remedy), 120)
            self.assertFalse(reason.endswith(("...", "…")))
            self.assertFalse(remedy.endswith(("...", "…")))


if __name__ == "__main__":
    unittest.main()
