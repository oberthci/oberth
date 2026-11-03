#!/usr/bin/env python3
"""Migration exports must be complete, read-only and safe to retry."""
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location(
    "export_issues", Path(__file__).with_name("export-issues.py"))
exporter = importlib.util.module_from_spec(spec)
spec.loader.exec_module(exporter)


class Source:
    def __init__(self, count=53):
        self.records = [{"id":i, "kind":"manual", "state":"closed" if i % 2 else "open",
                         "upstream":"forge", "title":f"Issue {i}",
                         "body":"Evidence\n雪\n" + ("x" * 120000 if i == count else "small"),
                         "created_at":"2026-11-01T00:00:00Z",
                         "updated_at":"2026-11-02T00:00:00Z", "occurrences":1,
                         "lock":None, "revision":3}
                        for i in range(count, 0, -1)]
        self.calls = []
        self.change_on_check = False
        self.scan = 0

    def __call__(self, tool, args):
        self.calls.append((tool, args.copy()))
        if tool == "issue_list":
            assert args["kind"] == "manual" and args["state"] == "all"
            assert args["limit"] == 50
            if "before" not in args:
                self.scan += 1
            rows = [r for r in self.records if r["id"] < args.get("before", 2**63)]
            page = [{k:v for k,v in r.items() if k != "body"} for r in rows[:50]]
            if self.change_on_check and self.scan > 1 and page:
                page[0]["updated_at"] = "2026-11-03T00:00:00Z"
            return {"issues":page, "total":len(self.records),
                    **({"next_before":page[-1]["id"]} if len(rows) > 50 else {})}
        if tool == "issue_get_many":
            assert set(args) == {"ids"}
            return {"results":[{"id":i, "error":"response_limit"} if i == len(self.records)
                               else {"id":i,"issue":copy.deepcopy(next(r for r in self.records if r["id"] == i))}
                               for i in args["ids"]]}
        if tool == "issue_get":
            assert set(args) == {"id"}
            return copy.deepcopy(next(r for r in self.records if r["id"] == args["id"]))
        raise AssertionError("mutation or unexpected tool: " + tool)


class ExportTests(unittest.TestCase):
    def test_pages_full_unicode_bodies_and_closed_history_without_mutation(self):
        source = Source()
        archive = exporter.collect(source)
        self.assertEqual(archive["issues"], source.records)
        self.assertEqual(archive["count"], 53)
        self.assertEqual(archive["visibility"], "authenticated uplink only")
        self.assertFalse(archive["database_snapshot"])
        self.assertEqual(archive["body_sha256"]["53"],
                         hashlib.sha256(source.records[0]["body"].encode()).hexdigest())
        self.assertEqual(source.scan, 2)
        self.assertEqual({name for name, _ in source.calls},
                         {"issue_list", "issue_get_many", "issue_get"})

    def test_candidate_metadata_revision_change_refuses_archive(self):
        source = Source(1)
        def read(tool, args):
            reply = source(tool, args)
            if tool == "issue_get":
                reply["revision"] += 1
            return reply
        with self.assertRaises(exporter.ExportError):
            exporter.collect(read)

    def test_scope_change_deletion_and_concurrent_edit_refuse_archive(self):
        for failure in ("changed", "missing", "scope"):
            with self.subTest(failure=failure):
                source = Source(2)
                source.change_on_check = failure == "changed"
                def call(tool, args):
                    result = source(tool, args)
                    if tool == "issue_get_many" and failure == "missing":
                        result["results"][-1] = {"id":1,"error":"not_found"}
                    if tool == "issue_get" and failure == "scope":
                        result["upstream"] = "different"
                    return result
                with self.assertRaises(exporter.ExportError):
                    exporter.collect(call)

    def test_same_second_body_edit_is_not_hidden_by_rounded_wire_timestamps(self):
        source = Source(2)
        def call(tool, args):
            if tool == "issue_list" and "before" not in args and source.scan == 1:
                source.records[0]["body"] = "changed without a new visible timestamp"
            return source(tool, args)
        with self.assertRaises(exporter.ExportError):
            exporter.collect(call)

    def test_bad_cursor_count_and_duplicate_ids_refuse_partial_export(self):
        for failure in ("cursor", "count", "duplicate", "no_cursor"):
            with self.subTest(failure=failure):
                source = Source()
                def call(tool, args):
                    result = source(tool, args)
                    if tool == "issue_list":
                        if failure == "cursor": result["next_before"] = 999
                        if failure == "count": result["total"] += 1
                        if failure == "duplicate": result["issues"][1] = result["issues"][0]
                        if failure == "no_cursor": result.pop("next_before", None)
                    return result
                with self.assertRaises(exporter.ExportError):
                    exporter.collect(call)

    def test_missing_body_truncation_and_mixed_result_ids_are_rejected(self):
        for failure in ("body", "truncated", "wrong_id", "result_ids"):
            with self.subTest(failure=failure):
                source = Source(2)
                def call(tool, args):
                    result = source(tool, args)
                    if tool == "issue_get_many" and failure == "result_ids":
                        result["results"][-1]["id"] = 2
                    if tool == "issue_get":
                        if failure == "body": result.pop("body")
                        if failure == "truncated": result["body"] = "prefix\n[truncated 4096 bytes]"
                        if failure == "wrong_id": result["id"] = 123
                    return result
                with self.assertRaises(exporter.ExportError):
                    exporter.collect(call)

    def test_export_limits_fail_instead_of_returning_a_prefix(self):
        with self.assertRaises(exporter.ExportError):
            exporter.collect(Source(), max_issues=52)
        with self.assertRaises(exporter.ExportError):
            exporter.collect(Source(), max_bytes=1024)

    def test_empty_scope_is_explicit_and_verified(self):
        source = Source(0)
        archive = exporter.collect(source)
        self.assertEqual(archive["issues"], [])
        self.assertEqual(archive["count"], 0)
        self.assertEqual(source.scan, 2)

    def test_file_is_private_and_existing_file_or_symlink_is_not_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "issues.json"
            archive = exporter.collect(Source(2))
            exporter.write_archive(path, archive)
            original = path.read_bytes()
            self.assertEqual(json.loads(original), archive)
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(FileExistsError):
                exporter.write_archive(path, {"replacement":True})
            self.assertEqual(path.read_bytes(), original)
            alias = Path(directory) / "alias.json"
            alias.symlink_to(path)
            with self.assertRaises(FileExistsError):
                exporter.write_archive(alias, archive)
            self.assertEqual(path.read_bytes(), original)
            self.assertEqual(sorted(p.name for p in Path(directory).iterdir()),
                             ["alias.json", "issues.json"])

    def test_transport_is_read_only_and_never_echoes_remote_output(self):
        with patch.object(exporter.subprocess, "run") as run:
            run.return_value = SimpleNamespace(returncode=0, stdout=b'{"id": 1}')
            self.assertEqual(exporter.cli_call("/a path/oberth", "issue_get", {"id":1}), {"id":1})
            command = run.call_args.args[0]
            self.assertEqual(command[:4], ["/a path/oberth", "mcp", "call", "issue_get"])
            self.assertEqual(json.loads(command[4]), {"id":1})
            for tool in ("issue_close", "issue_update", "issue_lock"):
                with self.assertRaises(exporter.ExportError):
                    exporter.cli_call("oberth", tool, {"id":1})
            self.assertEqual(run.call_count, 1)
            for code, output in [(1, b"PRIVATE credentials"), (0, b'{"id":1,"id":2}'), (0, b"PRIVATE text")]:
                run.return_value = SimpleNamespace(returncode=code, stdout=output)
                with self.assertRaises(exporter.ExportError) as caught:
                    exporter.cli_call("oberth", "issue_get", {"id":1})
                self.assertNotIn("PRIVATE", str(caught.exception))

    def test_cli_creates_only_a_complete_archive(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            record = Source(1).records[0]
            fixture = root / "fixture.json"
            fixture.write_text(json.dumps(record))
            command = root / "fake oberth"
            command.write_text("#!" + sys.executable + "\n" + '''
import json, pathlib, sys
record = json.loads(pathlib.Path(__file__).with_name("fixture.json").read_text())
assert sys.argv[1:3] == ["mcp", "call"]
tool, args = sys.argv[3], json.loads(sys.argv[4])
if tool == "issue_list":
    value = {"total":1, "issues":[{k:v for k,v in record.items() if k != "body"}]}
elif tool == "issue_get_many":
    value = {"results":[{"id":1,"issue":record}]}
else:
    raise SystemExit("unexpected command")
print(json.dumps(value))
''')
            command.chmod(0o700)
            output = root / "archive.json"
            args = [sys.executable, str(Path(__file__).with_name("export-issues.py")),
                    "--oberth", str(command), "--output", str(output)]
            result = subprocess.run(args, capture_output=True, text=True, check=False)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(json.loads(output.read_text())["issues"], [record])
            original = output.read_bytes()
            result = subprocess.run(args, capture_output=True, text=True, check=False)
            self.assertNotEqual(result.returncode, 0)
            self.assertEqual(output.read_bytes(), original)
            output.unlink()
            del record["body"]
            fixture.write_text(json.dumps(record))
            result = subprocess.run(args, capture_output=True, text=True, check=False)
            self.assertNotEqual(result.returncode, 0)
            self.assertFalse(output.exists())


if __name__ == "__main__":
    unittest.main()
