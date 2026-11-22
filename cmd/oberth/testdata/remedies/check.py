"""Validate displayed command references without executing any suggested action."""
import ast
from collections import Counter
import importlib.util
import json
import hashlib
from pathlib import Path
import re
import shlex
import sys
import subprocess

ROOT = Path(sys.argv[1]).resolve()
spec = importlib.util.spec_from_file_location("docs_contract", ROOT / "hack/test_docs.py")
docs = importlib.util.module_from_spec(spec)
spec.loader.exec_module(docs)


def allowed_tools():
    return {
        "git": "docs/releasing.md:65 requires Git for the clean checkout.",
        "make": "docs/releasing.md:65 requires make for the release targets.",
        "grep": "docs/releasing.md:58 requires grep for release record comparisons.",
        "go": "docs/releasing.md:65 pins the Go toolchain.",
        "python3": "docs/releasing.md:65 requires Python 3.",
        "curl": "docs/releasing.md:65 requires curl.",
        "cosign": "docs/releasing.md:65 requires the pinned signing tool.",
        "helm": "docs/install.md:170 uses Helm with the selected cluster context.",
        "kubectl": "docs/install.md:195 requires cluster access for explicit cleanup.",
        "sh": "docs/install.md:8 requires a shell for the verified fetcher.",
        "bash": "docs/releasing.md:104 runs the checkout's build scripts.",
        "mkdir": "docs/releasing.md:82 prepares local release directories.",
        "chmod": "docs/releasing.md:83 sets private directory permissions.",
        "umask": "docs/install.md:148 sets private permissions for the recovery Secret export.",
        "cp": "docs/releasing.md:122 copies the local signing key.",
        "cat": "docs/operations.md:63 reads the generated public key using standard shell tools.",
        "rm": "docs/install.md:240 explicitly removes the retained local profile.",
        "ssh-keygen": "docs/operations.md:62 creates a browser key locally.",
        "claude": "docs/install.md:88-94 requires that host for its Agent plugins recipe.",
        "codex": "docs/install.md:102 requires that host for its Agent plugins recipe.",
        "oberth-release": "docs/releasing.md:111 builds the separate release executable.",
    }


def source_texts(rows):
    yield from rows
    for path in sorted((ROOT / "hack/guards").glob("*.py")):
        if path.name.startswith("test_"):
            continue
        tree = ast.parse(path.read_text())
        doc_lines = {node.body[0].lineno for node in ast.walk(tree)
                     if isinstance(node, (ast.Module, ast.FunctionDef, ast.ClassDef)) and node.body
                     and isinstance(node.body[0], ast.Expr) and isinstance(node.body[0].value, ast.Constant)
                     and isinstance(node.body[0].value.value, str)}
        for node in ast.walk(tree):
            if isinstance(node, ast.Constant) and isinstance(node.value, str) and node.lineno not in doc_lines:
                yield dict(file=str(path.relative_to(ROOT)), line=node.lineno, text=node.value)
    for path in [*sorted((ROOT / "hack").glob("*.sh")), ROOT / "agent/skills/oberth/fetch-oberth"]:
        yield from quoted_texts(path)
    for name in ("app.js", "log.js", "issues.js"):
        yield from quoted_texts(ROOT / "internal/api/ui" / name)
    path = ROOT / "hack/guards/messages.json"
    for number, line in enumerate(path.read_text().splitlines(), 1):
        for literal in re.findall(r'"(?:[^"\\]|\\.)*"', line):
            yield dict(file="hack/guards/messages.json", line=number, text=json.loads(literal))


def shell_literals(text):
    lexer = shlex.shlex(text, posix=True)
    lexer.whitespace_split = True
    offset = 0
    while True:
        value = lexer.get_token()
        if value is None:
            return
        end = lexer.instream.tell()
        found = text.find(value, offset, end)
        line = 1 + text.count("\n", 0, found) if found >= 0 else max(1, lexer.lineno - value.count("\n"))
        yield line, value
        offset = end


def browser_literals(text):
    position = 0
    while position < len(text):
        if text.startswith("//", position):
            position = text.find("\n", position)
            if position < 0:
                return
        elif text.startswith("/*", position):
            end = text.find("*/", position + 2)
            if end < 0:
                raise ValueError("unfinished browser comment")
            position = end + 2
        elif text[position] in "\"'`":
            quote, start = text[position], position
            position += 1
            while position < len(text) and text[position] != quote:
                position += 2 if text[position] == "\\" else 1
            if position >= len(text):
                raise ValueError("unfinished browser string")
            literal = text[start:position + 1]
            if quote == "`":
                value = re.sub(r"\$\{[^}]*\}", "<value>", literal[1:-1])
            else:
                value = ast.literal_eval(literal)
            yield 1 + text.count("\n", 0, start), value
            position += 1
        else:
            position += 1


def quoted_texts(path):
    literals = browser_literals if path.suffix == ".js" else shell_literals
    for line, value in literals(path.read_text()):
        yield dict(file=str(path.relative_to(ROOT)), line=line, text=value)


def markdown_rows():
    for path in [ROOT / "README.md", *sorted((ROOT / "docs").glob("*.md"))]:
        heredoc = ""
        for line, text, complete in docs.examples(path.read_text()):
            if heredoc:
                if text == heredoc:
                    heredoc = ""
                continue
            match = re.search(r"<<\s*['\"]?(\w+)", text)
            if match:
                heredoc = match[1]
            yield dict(file=str(path.relative_to(ROOT)), line=line, text=text, shell=complete)


def snippets(row):
    text = row["text"].replace("<absolute-oberth-path>", "oberth")
    if row.get("help", "").startswith("help:"):
        text = re.sub(r"\n  ([^\n])", r" \1", text)
    if "shell" in row:
        if row["shell"] or text.startswith(("hack/", "./hack/")) or re.match(r"(?:[A-Za-z_]\w*=|[a-z][\w-]*\s|\"?\$RELEASE_TOOL)", text):
            yield text, row["shell"]
        return
    # Match all occurrences, including a second command after "or" in a remedy.
    tools = "|".join(re.escape(tool) for tool in ["oberth", "make", *allowed_tools()])
    pattern = rf"(?:\b(?:Read|Call|Invoke|Refresh|Inspect)\s+)(?=[a-z]\w*_\w+)|(?<![\w/.-])(?:{tools}|(?:\./)?hack/[\w./-]+\.(?:sh|py))\s+|(?:\bRun\s+)(?!(?:it|its|this|that|the|a|an|any|with)\b)(?=[a-z][\w/-]*(?:\s|[.!?]?$))|(?:\b[Uu]se\s+)(?=[a-z./][\w./-]*(?: --|[.!?]?$| (?:status|operator|get|list|setup)\b)|[a-z]\w*_\w+)"
    starts = [m for m in re.finditer(pattern, text)
              if not re.search(r"(?:the|same|another|previous|existing) $", text[:m.start()], re.IGNORECASE)
              and not (m.group().strip() == "oberth" and re.search(r"\b(?:git|helm)\b(?:(?!\bor\b|\bthen\b|[;\n]).)*$", text[:m.start()]))]
    for index, match in enumerate(starts):
        end = starts[index + 1].start() if index + 1 < len(starts) else len(text)
        command = text[match.start():end]
        command = re.sub(r"^(?:[Rr]un|[Uu]se|Read|Call|Invoke|Refresh|Inspect)\s+", "", command)
        command = command.split("\n", 1)[0]
        yield command, True


def words_for(text):
    text = re.sub(r"<[^<>\n]+>", "DOC_VALUE", text)
    text = re.sub(r"%[-+0-9.\[\]]*[sqvd]", "DOC_VALUE", text)
    text = re.sub(r"\[(?:flags|[^\[\]]+)\]", "", text)
    lexer = shlex.shlex(text.removeprefix("$ "), posix=True, punctuation_chars=";&|<>()")
    lexer.whitespace_split = True
    lexer.commenters = "#"
    return list(lexer)


def commands_in(text, complete):
    # Check substitutions even when they occur inside an assignment.
    for inner in re.findall(r"\$\(([^()]*)\)", text):
        yield from commands_in(inner, complete)
    text = re.sub(r"\$\([^()]*\)", "DOC_VALUE", text)
    words = words_for(text)
    current = []
    for word in words + [";"]:
        if word in (";", "&&", "||", "|", "(", ")"):
            if current:
                yield current, complete
            current = []
        else:
            current.append(word)


def invocation(words, complete, catalogue, fields=()):
    while words and (re.match(r"^[A-Za-z_]\w*=", words[0]) or words[0] in ("if", "then", "fi", "do", "done", "!", "else", "export")):
        words = words[1:]
    if not words or words[0] in ("[", "]", "{", "}"):
        return []
    if words[0] == "$RELEASE_TOOL":
        words[0] = "oberth-release"
    if words[0] in fields and "tool " + words[0] not in catalogue and not any(w.startswith("-") for w in words[1:]):
        return []
    if "tool " + words[0] in catalogue:
        return ["oberth", "tool", *words]
    if not complete and words[0] not in allowed_tools() and words[0] not in ("oberth", "make") and not words[0].removeprefix("./").startswith("hack/"):
        # Inline prose/field names are not shell commands; options identify an invocation.
        path = " ".join(words)
        if any(path == name or path.startswith(name + " ") for name in catalogue):
            return ["oberth", *words]
        verbs = {"operator", *(word for name in catalogue for word in name.split())}
        if len(words) < 2 or not (words[1].startswith("-") or words[1] in verbs or "_" in words[0]):
            return []
    return words


def trim_prose(words):
    result = []
    for word in words:
        word = word.rstrip(".,:;?")
        if word in ("to", "with", "for", "then", "before", "after", "again", "or", "and", "on", "from", "instead", "if", "when", "works", "command", "commands", "adds", "have", "has", "is", "are", "was", "were"):
            break
        if word in ("<", ">", ">>", "2>"):
            break
        if word:
            result.append(word)
    return result


def script_error(words):
    script = ROOT / words[0]
    if not script.is_file():
        return "script does not exist: " + words[0]
    text = script.read_text()
    # Only argument comparisons and case arms declare accepted script options.
    flags = set(re.findall(r"\$(?:[12]|arg)\W+[-=!]*\s*['\"]?(--[\w-]+)", text))
    flags.update(re.findall(r"(--[\w-]+)(?=[|)])", text))
    for word in words[1:]:
        if word.startswith("-") and word.partition("=")[0] not in flags:
            return "script does not accept flag " + word
    return ""


def make_error(words):
    targets = set()
    for line in (ROOT / "Makefile").read_text().splitlines():
        match = re.match(r"([A-Za-z][\w -]*):(?:\s|$)", line)
        if match:
            targets.update(match[1].split())
    help_text = subprocess.run(["make", "--help"], check=True, capture_output=True,
                               text=True, timeout=10).stdout
    flags = set(re.findall(r"--[a-z][a-z-]*|(?<![\w-])-[A-Za-z](?![A-Za-z])", help_text))
    values = set(re.findall(r"(--[a-z-]+|-[A-Za-z])(?:=| +)[A-Z][A-Z_-]*", help_text))
    remaining = iter(words[1:])
    for word in remaining:
        if word.startswith("-"):
            flag, equal, _ = word.partition("=")
            if flag not in flags:
                return "make does not accept flag " + flag
            if flag in values and not equal and next(remaining, None) is None:
                return "make flag needs a value: " + flag
        elif "=" not in word and word not in targets:
            return "make target does not exist: " + word
    return ""


def command_error(words, catalogue, complete, example=True):
    if not words:
        return ""
    words = [words[0].removeprefix("./"), *words[1:]]
    tool = words[0]
    if "/" in tool and not tool.startswith("hack/"):
        tool = Path(tool).name
        words[0] = tool
    if tool == "oberth":
        return oberth_error(words, catalogue, complete, example)
    if tool in ("sh", "bash") and len(words) > 1 and words[1].removeprefix("./").startswith("hack/"):
        return script_error([words[1].removeprefix("./"), *words[2:]])
    if tool == "fetch-oberth":
        return script_error(["agent/skills/oberth/fetch-oberth", *words[1:]])
    if tool.startswith("hack/"):
        return script_error(words)
    if tool == "make":
        return make_error(words)
    if tool not in allowed_tools():
        return "tool is not on the documented prerequisite list: " + tool
    return ""


def oberth_error(words, catalogue, complete, example):
    if len(words) == 1:
        return ""
    prefix, rest = [], words[1:]
    while rest and rest[0].partition("=")[0] in catalogue["tool"]["flags"] and rest[0] not in ("--help", "-h"):
        flag = rest.pop(0)
        prefix.append(flag)
        if catalogue["tool"]["flags"][flag.partition("=")[0]] and "=" not in flag:
            if not rest:
                return "missing global flag value: " + flag
            prefix.append(rest.pop(0))
    if prefix:
        words = ["oberth", *rest, *prefix]
    if len(words) == 1:
        return ""
    if words[1] in ("--help", "-h"):
        root = {"root": {"args": [0, 0], "flags": catalogue["tool"]["flags"]}}
        return docs.command_error(["oberth", "root", *words[1:]], root, True)
    group = " ".join(word for word in words[1:] if word not in ("--help", "-h"))
    if any(word in ("--help", "-h") for word in words) and any(name.startswith(group + " ") for name in catalogue):
        return ""
    if words[1] == "help":
        return "" if len(words) == 2 else command_error(["oberth", *words[2:], "--help"], catalogue, False)
    if words[1] == "tool" and len(words) > 2 and words[2] != "DOC_VALUE" and "tool " + words[2] not in catalogue:
        return "unknown raw tool: " + words[2]
    # Rendered help expands the catalogue's dynamic command path templates.
    if words[1] == "DOC_VALUE":
        return ""
    paths = [name for name in catalogue if words[1:1 + len(name.split())] == name.split()]
    if paths:
        name = max(paths, key=lambda path: len(path.split()))
        tail = words[1 + len(name.split()):]
        if catalogue[name].get("stdin_json") and tail == ["-"]:
            return ""
        supplied, index = set(), 0
        while index < len(tail):
            flag, equal, _ = tail[index].partition("=")
            if flag == "--":
                break
            if flag in catalogue[name]["flags"]:
                supplied.add(flag)
                index += catalogue[name]["flags"][flag] if not equal else 0
            index += 1
        syntax = dict(catalogue[name])
        positions = syntax.get("position_flags", {})
        syntax["args"] = [syntax["args"][0] - sum(positions[f] for f in supplied if f in positions), syntax["args"][1] - sum(f in positions for f in supplied)]
        catalogue = {**catalogue, name: syntax}
    words = ["DOC_VALUE" if word == "-" else word for word in words] if paths else words
    error = docs.command_error(words, catalogue, complete and not any(w in ("--help", "-h") for w in words))
    if error:
        return error
    if paths and complete and example and not any(w in ("--help", "-h") for w in words):
        missing = set(syntax.get("required_flags", [])) - supplied
        if missing:
            return name + ": missing required flags " + ", ".join(sorted(missing))
    return ""


def inspect(row, catalogue, fields=()):
    errors, count = [], 0
    for text, complete in snippets(row):
        try:
            for words, _ in commands_in(text, complete):
                if row.get("help", "").startswith("help:") and words and words[0].startswith("-"):
                    words = ["oberth", *row["help"][5:].split(), *words]
                if "shell" not in row:
                    words = trim_prose(words)
                literal = complete and "shell" in row
                if len(words) == 1 and re.search(r"\b(?:Run|Use)\s+" + re.escape(words[0]) + r"[.!?]?(?:\s|$)", row["text"]):
                    literal = True
                words = invocation(words, literal, catalogue, fields)
                if not words:
                    continue
                count += 1
                full = complete or bool(re.match(r"(?:[A-Za-z_]\w*=\S+\s+)*oberth\s", text))
                if not full and words[0] == "oberth":
                    for name in sorted(catalogue, key=lambda name: -len(name)):
                        if words[1:1+len(name.split())] == name.split() and words[-1] in catalogue[name]["flags"] and catalogue[name]["flags"][words[-1]]:
                            words.append("DOC_VALUE")
                            break
                # A bare name in prose is a reference, including sentence-final
                # mentions; fenced commands still have to supply their arguments.
                if not row.get("shell") and words[0] == "oberth" and " ".join(words[1:]) in catalogue:
                    full = False
                error = command_error(words, catalogue, full, not row.get("help"))
                if error:
                    errors.append(dict(file=row["file"], line=row["line"], text=text, error=error, help=row.get("help", "")))
        except ValueError as error:
            errors.append(dict(file=row["file"], line=row["line"], text=text, error=str(error)))
    return errors, count


def parser_controls(catalogue):
    valid = ["oberth --help", "oberth -h", "oberth help", "oberth help run", "oberth run --help", "oberth version --help", "oberth run get '<run>'", "oberth run get --id '<run>'", "oberth --json repo list", "sh hack/release-pin.sh --replace key.pub", "make release-bytes"]
    invalid = ["oberth restore archive", "oberth upgrade --missing-flag", "oberth run get first second", "oberth upgrade --version", "sh hack/release-pin.sh --check key.pub", "sh hack/missing-script.sh", "make missing-target", "make --missing-flag", "missing-tool --repair"]
    for text in valid:
        errors, count = inspect(dict(file="control", line=1, text=text, shell=True), catalogue)
        assert count and not errors, (text, errors)
    for text in invalid:
        errors, _ = inspect(dict(file="control", line=1, text=text, shell=True), catalogue)
        assert errors, text
    for text in ["Run missing-tool --repair.", "Use missing-tool --repair.", "Run missing-tool.", "Use missing-tool.", "Use missing-tool status.", "Use missing-tool operator unseal.", "missing-tool status"]:
        row = dict(file="control", line=1, text=text)
        if text.startswith("missing"):
            row["shell"] = False
        assert inspect(row, catalogue)[0], text
    row = dict(file="control", line=1, text="Use next_after_id even after an empty page.")
    assert not inspect(row, catalogue, {"next_after_id"})[0]
    assert inspect(dict(row, text="Use next_after_id --repair."), catalogue, {"next_after_id"})[0]
    for text in ["Run oberth join.", "Check with oberth run get before retrying.", "oberth secret put"]:
        assert not inspect(dict(file="control", line=1, text=text), catalogue)[0], text
    for text in ["Check with oberth audit list.", "Run oberth audit --missing-flag.", "Run oberth run get first second."]:
        assert inspect(dict(file="control", line=1, text=text), catalogue)[0], text
    assert inspect(dict(file="control", line=1, text="oberth join", shell=True), catalogue)[0]
    known = [dict(file="fixture", text="oberth missing-command", error="unknown")]
    approved = {failure_key(known[0])}
    moved = dict(known[0], line=999, error="different parser wording")
    assert failure_key(moved) == failure_key(known[0])
    assert not debt_errors(known, known, approved, len(known))
    changed = dict(known[0], text="oberth another-missing-command")
    assert debt_errors([changed, *known[1:]], [changed, *known[1:]], approved, len(known))
    assert debt_errors(known[1:], known, approved, len(known))
    assert not debt_errors(known[1:], known[1:], approved, len(known)-1)
    shell = "# a comment's quote is not a message\nfail 'Reason.' \\n  'Run missing-tool --repair.'\n"
    assert any("Run missing-tool" in value for _, value in shell_literals(shell))
    browser = "// a comment's quote\nconst message = 'Run missing-tool --repair.';"
    assert any("Run missing-tool" in value for _, value in browser_literals(browser))
    for text in ["Run it with git push -o oberth.run=1.", "Run oberth upgrade or oberth rollback."]:
        errors, count = inspect(dict(file="control", line=1, text=text), catalogue)
        assert not errors and count, (text, errors)
    for text in ["Another oberth occupies the install path.", "The previous oberth copy already exists.",
                 "The existing oberth could not be kept.", "Kept previous oberth at %s/oberth.previous."]:
        row = dict(file="control", line=1, text=text)
        assert inspect(row, catalogue) == ([], 0), text
        assert inspect(dict(row, text=text + " Run oberth missing-command."), catalogue)[0], text
    text = "Inspect helm history oberth -n <namespace>; preserve the data volume before repair."
    row = dict(file="control", line=1, text=text)
    assert inspect(row, catalogue) == ([], 1), text
    assert inspect(dict(row, text=text + "; Run oberth missing-command."), catalogue)[0], text
    row = dict(file="control", line=1, text="(umask 077; kubectl get secret oberth-ca > /private/secrets.yaml)", shell=True)
    assert inspect(row, catalogue) == ([], 2), row


def known_failures():
    # All original real invocation defects are fixed; new debt is forbidden.
    return []


def approved_failure_keys():
    return set()


def failure_key(row):
    identity = [row['file'], row['text']]
    return hashlib.sha256(json.dumps(identity, ensure_ascii=True).encode()).hexdigest()


def debt_errors(failures, known, approved, limit):
    keys = {failure_key(row) for row in known}
    if len(keys) != len(known) or len(keys) != limit:
        return ["Known-failure count must match its frozen limit; only lower the limit when removing entries."]
    if keys - approved:
        return ["Known-failure permissions may only shrink; a new exception is forbidden."]
    seen = {failure_key(row) for row in failures}
    stale = [row for row in known if failure_key(row) not in seen]
    return ["Resolved known failure must be removed: " + repr(row) for row in stale]


def main():
    payload = json.load(sys.stdin)
    parser_controls(payload["catalogue"])
    fields = {row["text"] for row in payload["texts"] if row.get("help", "").startswith("field:")}
    texts = [row for row in payload["texts"] if not row.get("help", "").startswith("field:")]
    rows = [*source_texts(texts), *markdown_rows()]
    failures, counts = [], Counter()
    for row in rows:
        errors, count = inspect(row, payload["catalogue"], fields)
        failures.extend(errors)
        suffix = Path(row["file"]).suffix
        source = "help" if row.get("help") else {".go": "go", ".md": "markdown", ".js": "browser", ".py": "guards", ".json": "guards", ".sh": "shell"}.get(suffix, "fetcher")
        counts["texts"] += 1
        counts[source + "_texts"] += 1
        counts[source + "_commands"] += count
        counts["commands"] += count
        counts[row["file"].split("/")[0]] += count
    known = known_failures()
    policy = debt_errors(failures, known, approved_failure_keys(), 0)
    permitted = {failure_key(row) for row in known}
    unexpected = [row for row in failures if failure_key(row) not in permitted]
    inventory = "--inventory" in sys.argv[2:]
    print(json.dumps(dict(counts=counts, known_failures=known, failures=failures if inventory else unexpected, policy_errors=policy), indent=2))
    return bool(failures if inventory else unexpected) or bool(policy)


if __name__ == "__main__":
    sys.exit(main())
