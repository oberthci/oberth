"""Collect distinct, complete CLI messages separately from command transcripts."""


def messages(raw):
    """Undo the CLI formatter's two-space, 80-column continuation lines."""
    result, previous = [], ""
    for line in raw.decode("utf-8", errors="replace").split("\n"):
        continuation = line.startswith("  ") and result and previous.strip()
        word = line[2:].split(" ", 1)[0]
        if continuation and len(previous) + 1 + len(word) > 80:
            content = previous.removeprefix("  ") if previous.startswith("  ") else previous
            width = 78 if previous.startswith("  ") else 80
            separator = "" if len(content) == width and " " not in content else " "
            result[-1] += separator + line[2:]
        elif line:
            result.append(line)
        previous = line
    return result


def collect(session):
    rows, by_text = [], {}
    for command in session.commands:
        program = command["argv"][0] if command["argv"] else None
        if program not in ("oberth", str(getattr(session, "binary", ""))):
            continue
        stem = session.private / command["capture"]
        # stdout and stderr have independent framing; never join their wrap lines.
        for suffix in (".stdout", ".stderr"):
            for original in messages(stem.with_suffix(suffix).read_bytes()):
                text = session.clean(original, limit=None)
                length, utf8_bytes = len(original), len(original.encode("utf-8"))
                if text in by_text:
                    by_text[text]["count"] += 1
                    by_text[text]["length"] = max(by_text[text]["length"], length)
                    by_text[text]["utf8_bytes"] = max(by_text[text]["utf8_bytes"], utf8_bytes)
                    continue
                row = {"step": command["step"], "text": text, "length": length,
                       "utf8_bytes": utf8_bytes, "count": 1,
                       "exit_code": command["exit_code"], "capture": command["capture"]}
                rows.append(row)
                by_text[text] = row
    return rows
