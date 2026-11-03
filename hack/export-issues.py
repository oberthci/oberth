#!/usr/bin/env python3
"""Export visible manual issue history before #1318 tracking migration.

Uses only existing read-only MCP tools via the operator-configured Oberth CLI.
This is a migration aid, not a backup or a transactionally consistent snapshot.
"""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile


class ExportError(Exception):
    pass


READ_TOOLS = {"issue_list", "issue_get_many", "issue_get"}
MAX_ISSUES = 10000
MAX_BYTES = 128 << 20
MAX_REPLY_BYTES = 20 << 20
TRUNCATION = re.compile(r"\[truncated \d+ bytes\]\s*$")


def integer(value):
    return isinstance(value, int) and not isinstance(value, bool)


def metadata(row):
    if not isinstance(row, dict) or not integer(row.get("id")) or row["id"] <= 0:
        raise ExportError("invalid issue metadata")
    for field in ("title", "upstream", "updated_at", "created_at"):
        if not isinstance(row.get(field), str):
            raise ExportError("missing issue metadata: " + field)
    if not row["updated_at"] or row.get("kind") != "manual" or row.get("state") not in ("open", "closed"):
        raise ExportError("unexpected issue kind, state or revision")
    # Locks are transient coordination state, not a body/title revision. Reading
    # neither acquires nor renews them; the detail record retains what was seen.
    return {key:value for key, value in row.items() if key not in ("lock", "body")}


def scan(call, max_issues):
    rows = []
    cursor = None
    total = None
    while True:
        args = {"kind":"manual", "state":"all", "limit":50}
        if cursor is not None:
            args["before"] = cursor
        page = call("issue_list", args)
        if not isinstance(page, dict) or not isinstance(page.get("issues"), list):
            raise ExportError("invalid issue list response")
        count = page.get("total")
        if not integer(count) or count < 0 or count > max_issues:
            raise ExportError("issue count absent, invalid or above export limit")
        if total is not None and count != total:
            raise ExportError("issue count changed during export; retry in a quiet window")
        total = count
        if len(page["issues"]) > 50:
            raise ExportError("server exceeded requested page size")
        previous = cursor
        for row in page["issues"]:
            meta = metadata(row)
            if previous is not None and meta["id"] >= previous:
                raise ExportError("issue cursor repeated or did not move backwards")
            previous = meta["id"]
            rows.append(meta)
        if len(rows) > total:
            raise ExportError("issue page count exceeds reported total")
        next_cursor = page.get("next_before")
        if next_cursor in (None, 0):
            if len(rows) != total:
                raise ExportError("incomplete issue list; refusing a partial export")
            return rows
        if not integer(next_cursor) or not page["issues"] or next_cursor != previous or len(rows) >= total:
            raise ExportError("invalid issue continuation cursor")
        cursor = next_cursor


def details(call, listing, max_bytes):
    size = 0
    for offset in range(0, len(listing), 50):
        page = listing[offset:offset + 50]
        ids = [row["id"] for row in page]
        reply = call("issue_get_many", {"ids":ids})
        results = reply.get("results") if isinstance(reply, dict) else None
        if not isinstance(results, list) or len(results) != len(ids):
            raise ExportError("incomplete issue_get_many response")
        for expected, result in zip(page, results):
            if not isinstance(result, dict) or result.get("id") != expected["id"]:
                raise ExportError("issue_get_many returned unexpected IDs")
            if result.get("error") == "response_limit":
                record = call("issue_get", {"id":expected["id"]})
            elif result.get("error"):
                raise ExportError("issue unavailable during export; retry with unchanged visibility")
            else:
                record = result.get("issue")
            observed = metadata(record)
            if any(observed.get(key) != value for key, value in expected.items()):
                raise ExportError("issue changed during export; retry in a quiet window")
            body = record.get("body")
            if not isinstance(body, str) or record.get("truncated") or TRUNCATION.search(body):
                raise ExportError("issue body absent or marked truncated; refusing partial history")
            encoded = json.dumps(record, ensure_ascii=False).encode("utf-8")
            size += len(encoded)
            if size > max_bytes:
                raise ExportError("issue history exceeds export byte limit")
            yield record


def collect(call, *, max_issues=MAX_ISSUES, max_bytes=MAX_BYTES):
    started = datetime.datetime.now(datetime.timezone.utc).isoformat()
    listing = scan(call, max_issues)
    records = list(details(call, listing, max_bytes))
    if scan(call, max_issues) != listing:
        raise ExportError("issue list changed during export; retry in a quiet window")
    # The wire timestamps have second precision. Re-read full bodies as well:
    # a same-second edit need not change list metadata. This still cannot make
    # separate API reads a transactionally consistent database snapshot.
    for before, after in zip(records, details(call, listing, max_bytes)):
        comparable = lambda row: {key:value for key, value in row.items() if key != "lock"}
        if comparable(before) != comparable(after):
            raise ExportError("issue content changed during export; retry in a quiet window")
    body_hashes = {str(row["id"]): hashlib.sha256(row["body"].encode("utf-8")).hexdigest()
                   for row in records}
    return {"format":"oberth-manual-issues-v1", "visibility":"authenticated uplink only",
            "database_snapshot":False, "started_at":started,
            "finished_at":datetime.datetime.now(datetime.timezone.utc).isoformat(),
            "count":len(records), "body_sha256":body_hashes, "issues":records}


def write_archive(path, archive):
    path = Path(path)
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=path.parent,
                                         prefix=".oberth-issues-", delete=False) as stream:
            temporary = Path(stream.name)
            os.fchmod(stream.fileno(), 0o600)
            json.dump(archive, stream, ensure_ascii=False, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        # Same-directory hard link publishes the complete file atomically and
        # fails if the destination already exists, including a dangling symlink.
        os.link(temporary, path)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def unique_keys(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ExportError("duplicate JSON field in MCP response")
        result[key] = value
    return result


def cli_call(executable, tool, args):
    if tool not in READ_TOOLS:
        raise ExportError("export only permits read tools")
    try:
        result = subprocess.run([executable, "mcp", "call", tool, json.dumps(args)],
                                capture_output=True, timeout=120, check=False)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise ExportError("Oberth read command failed or timed out") from error
    # Do not echo CLI output: it can contain private issue text or auth errors.
    if result.returncode != 0 or len(result.stdout) > MAX_REPLY_BYTES:
        raise ExportError("Oberth read failed or exceeded reply limit: " + tool)
    try:
        value = json.loads(result.stdout, object_pairs_hook=unique_keys)
    except (ValueError, UnicodeError) as error:
        raise ExportError("invalid JSON from Oberth read: " + tool) from error
    if not isinstance(value, dict):
        raise ExportError("expected object from Oberth read: " + tool)
    return value


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--oberth", default="oberth", help="path to operator-configured Oberth CLI")
    parser.add_argument("--output", type=Path, required=True, help="new private JSON file; never overwritten")
    args = parser.parse_args()
    try:
        if os.path.lexists(args.output):
            raise ExportError("output already exists; choose a new path")
        archive = collect(lambda tool, params: cli_call(args.oberth, tool, params))
        write_archive(args.output, archive)
    except (ExportError, OSError) as error:
        parser.exit(1, "issue export: " + str(error) + "\n")
    print(f"Exported {archive['count']} visible manual issues to {args.output}; no server changes.")


if __name__ == "__main__":
    main()
