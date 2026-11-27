"""Real merges keep independent plan reviews and reject unreviewed inputs."""
import hashlib
import json
import os
from pathlib import Path
import runpy
import shlex
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[2]


class PlanMergeTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        shutil.copytree(ROOT / "docs/plans", self.root / "docs/plans")
        (self.root / "hack/plans").mkdir(parents=True)
        (self.root / "hack/tools").mkdir()
        for name in ("merge.py", "parity.py", "portable.py", "policy.py"):
            shutil.copyfile(ROOT / "hack/plans" / name, self.root / "hack/plans" / name)
        shutil.copyfile(ROOT / "hack/tools/go_tool.py", self.root / "hack/tools/go_tool.py")
        for name in ("Makefile", ".gitattributes"):
            shutil.copyfile(ROOT / name, self.root / name)
        for name in ("check.sh", "steps.sh"):
            shutil.copyfile(ROOT / "hack" / name, self.root / "hack" / name)
        self.git("init", "-q", "-b", "main")
        for field in ("user.name", "user.email"):
            self.git("config", field, self.root.name)
        self.git("config", "commit.gpgsign", "false")
        self.git("config", "core.hooksPath", "/dev/null")
        self.run_command("make", "setup")
        self.commit("Base")
        self.api = runpy.run_path(str(self.root / "hack/plans/parity.py"))
        self.manifest = self.root / "docs/plans/dispositions.json"

    def run_command(self, *args, check=True):
        result = subprocess.run(args, cwd=self.root, capture_output=True, text=True,
                                env={**os.environ, "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": "/dev/null"})
        if check:
            self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        return result

    def git(self, *args, check=True):
        return self.run_command("git", *args, check=check)

    def commit(self, message):
        self.git("add", ".")
        self.git("commit", "-qm", message)

    def edit(self, index, word, padding=False):
        data = json.loads(self.manifest.read_text())
        old = data["deltas"][index]
        path, number = old["spec"].rsplit(":", 1)
        file = self.root / path
        lines = file.read_text().splitlines(keepends=True)
        # Different paragraphs in the same plan; one side also moves later lines.
        line = int(number) - 1
        lines[line] = lines[line].rstrip("\n") + " " + word + "\n"
        if padding:
            lines[line:line] = ["\n", "\n"]
        file.write_text("".join(lines))
        expected = self.api["spec_deltas"]()
        updated = [key for key in expected if key.rsplit(":", 1)[0] == path]
        affected = [delta for delta in data["deltas"] if delta["spec"].rsplit(":", 1)[0] == path]
        for delta, source in zip(affected, updated):
            old_basis = delta["basis"]
            delta["spec"] = source
            delta["basis"] = expected[source]["text"]
            for row in delta["rows"] + delta.get("predicates", []):
                if row.get("basis") == old_basis:
                    row["basis"] = delta["basis"]
        data["spec_sha256"][path] = hashlib.sha256(file.read_bytes()).hexdigest()
        self.manifest.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n")
        parity = self.root / "docs/plans/PARITY.md"
        text = parity.read_text()
        header = next(line for line in text.splitlines() if line.startswith("Spec input:"))
        digest = hashlib.sha256(self.manifest.read_bytes()).hexdigest()
        parity.write_text(text.replace(header, f"Spec input: docs/plans/dispositions.json (override --dispositions); SHA256 {digest}; {len(data['deltas'])} marked Delta locations."))

    def branches(self, same_clause=False):
        self.git("checkout", "-qb", "side")
        self.edit(0, "Reviewed-side.", padding=True)
        self.commit("Review first section")
        self.git("checkout", "-q", "main")
        self.edit(0 if same_clause else 3, "Reviewed-main.")
        self.commit("Review second section")

    def assert_witnesses(self):
        check = self.run_command("python3", "-I", "hack/plans/parity.py", "--check")
        self.assertIn('"mode": "portable"', check.stdout)
        parity = (self.root / "docs/plans/PARITY.md").read_text()
        self.assertIn(hashlib.sha256(self.manifest.read_bytes()).hexdigest(), parity)
        self.assertEqual("", self.git("status", "--porcelain").stdout)
        text = "\n".join(path.read_text() for path in (self.root / "docs/plans").glob("c[0-9]*-*.md"))
        self.assertIn("Reviewed-side.", text)
        self.assertIn("Reviewed-main.", text)

    def test_independent_sections_merge_without_conflicts(self):
        self.branches()
        self.git("merge", "--no-edit", "side")
        self.assert_witnesses()

    def test_independent_sections_rebase_without_conflicts(self):
        self.branches()
        self.git("checkout", "side")
        self.git("rebase", "main")
        self.assert_witnesses()

    def test_linked_worktree_rebase_without_conflicts(self):
        self.branches()
        linked = self.root / "linked"
        self.git("worktree", "add", str(linked), "side")
        self.git("-C", str(linked), "rebase", "main")
        self.run_command("python3", "-I", str(linked / "hack/plans/parity.py"), "--check")
        self.assertEqual("", self.git("-C", str(linked), "status", "--porcelain").stdout)

    def test_rebase_replays_multiple_reviews(self):
        self.branches()
        self.git("checkout", "side")
        self.edit(1, "Reviewed-again.")
        self.commit("Review another section")
        self.git("rebase", "main")
        self.assert_witnesses()

    def test_independent_plan_files_merge_without_conflicts(self):
        self.git("checkout", "-qb", "side")
        self.edit(0, "Reviewed-side.", padding=True)
        self.commit("Review first plan")
        self.git("checkout", "-q", "main")
        self.edit(len(json.loads(self.manifest.read_text())["deltas"]) - 1, "Reviewed-main.")
        self.commit("Review second plan")
        self.git("merge", "--no-edit", "side")
        self.assert_witnesses()

    def test_disabling_driver_reproduces_derived_conflicts(self):
        self.branches()
        self.git("config", "merge.plan-witness.driver", "git merge-file %A %O %B")
        result = self.git("merge", "--no-edit", "side", check=False)
        self.assertNotEqual(0, result.returncode)
        self.assertIn("docs/plans/dispositions.json", self.git("diff", "--name-only", "--diff-filter=U").stdout)

    def test_merge_is_deterministic_in_either_direction(self):
        self.branches()
        self.git("branch", "opposite", "main")
        self.git("merge", "--no-edit", "side")
        tree = self.git("rev-parse", "HEAD^{tree}").stdout
        self.git("checkout", "side")
        self.git("merge", "--no-edit", "opposite")
        self.assertEqual(tree, self.git("rev-parse", "HEAD^{tree}").stdout)
        self.assert_witnesses()

    def test_manifest_round_trip_preserves_bytes(self):
        for path in ("docs/plans/dispositions.json", "docs/plans/PARITY.md"):
            with self.subTest(path=path):
                original = (self.root / path).read_bytes()
                files = [self.root / name for name in ("base", "ours", "theirs")]
                for file in files:
                    file.write_bytes(original)
                self.run_command("python3", "-I", "hack/plans/merge.py", *map(str, files), path, "HEAD", "HEAD", "HEAD")
                self.assertEqual(original, files[1].read_bytes())

    def test_overlapping_authored_changes_remain_a_conflict(self):
        self.branches(same_clause=True)
        result = self.git("merge", "--no-edit", "side", check=False)
        self.assertNotEqual(0, result.returncode)
        self.assertIn("docs/plans/dispositions.json", self.git("diff", "--name-only", "--diff-filter=U").stdout)

    def test_missing_promised_object_refuses_without_fetching(self):
        self.branches()
        refs = [self.git("merge-base", "main", "side").stdout.strip(), "main", "side"]
        backing = self.root / "backing.git"
        self.git("clone", "--bare", str(self.root), str(backing))
        self.git("--git-dir=" + str(backing), "config", "uploadpack.allowAnySHA1InWant", "true")
        marker = self.root / "transport-started"
        transport = self.root / "upload-pack"
        transport.write_text("#!/bin/sh\nprintf started >> " + shlex.quote(str(marker)) +
                             '\nexec git-upload-pack "$@"\n')
        transport.chmod(0o700)
        for field, value in (("url", backing.as_uri()), ("promisor", "true"),
                             ("partialclonefilter", "blob:none"), ("uploadpack", str(transport))):
            self.git("config", "remote.origin." + field, value)
        plan = "docs/plans/c1-transport-and-tool-registry.md"
        missing = self.git("rev-parse", refs[0] + ":" + plan).stdout.strip()
        loose = self.root / ".git/objects" / missing[:2] / missing[2:]
        original = loose.read_bytes()
        loose.unlink()
        # Prove that the promised blob really is fetchable before testing refusal.
        self.git("show", refs[0] + ":" + plan)
        self.assertTrue(marker.exists())
        marker.unlink()
        # Lazy fetch packs its response; remove that fetched pack, then restore
        # the original loose blob so each case starts with exactly one missing blob.
        for file in (self.root / ".git/objects/pack").iterdir():
            file.unlink()
        for path in ("docs/plans/dispositions.json", "docs/plans/PARITY.md"):
            with self.subTest(path=path):
                loose.write_bytes(original)
                files = [self.root / name for name in ("base", "ours", "theirs")]
                for file, ref in zip(files, refs):
                    file.write_text(self.git("show", ref + ":" + path).stdout)
                before = files[1].read_bytes()
                loose.unlink()
                with patch.dict(os.environ, {"GIT_NO_LAZY_FETCH": "0"}):
                    result = self.run_command("python3", "-I", "hack/plans/merge.py",
                                              *map(str, files), path, *refs, check=False)
                self.assertEqual(1, result.returncode, result.stderr)
                self.assertIn("Plan merge needs review", result.stderr)
                self.assertEqual(before, files[1].read_bytes())
                self.assertFalse(marker.exists(), "merge driver started a promisor transport")
                self.assertFalse(loose.exists())
                self.assertEqual([], list((self.root / ".git/objects/pack").iterdir()))

    def test_driver_ignores_inherited_git_configuration(self):
        files = [self.root / name for name in ("base", "ours", "theirs")]
        original = self.manifest.read_bytes()
        for file in files:
            file.write_bytes(original)
        with patch.dict(os.environ, {"GIT_DIR": str(self.root / "absent.git"),
                                     "GIT_CONFIG_COUNT": "1",
                                     "GIT_CONFIG_KEY_0": "core.repositoryFormatVersion",
                                     "GIT_CONFIG_VALUE_0": "9999"}):
            self.run_command("python3", "-I", "hack/plans/merge.py", *map(str, files),
                             "docs/plans/dispositions.json", "HEAD", "HEAD", "HEAD")
        self.assertEqual(original, files[1].read_bytes())

    def test_guard_still_rejects_undeclared_edits_and_stale_locations(self):
        file = self.root / "docs/plans/c1-transport-and-tool-registry.md"
        file.write_text(file.read_text().replace("C1 owns HTTP", "C1 owns changed HTTP", 1))
        check = self.run_command("python3", "-I", "hack/plans/parity.py", "--check", check=False)
        self.assertNotEqual(0, check.returncode)
        self.assertIn("POLICY:", check.stderr)
        self.assertIn("stale spec digest", check.stderr)
        check = self.run_command("make", "check", check=False)
        self.assertNotEqual(0, check.returncode)
        self.assertIn("stale spec digest", check.stderr)
        self.assertNotIn("go test", check.stdout)
        self.git("restore", str(file))
        data = json.loads(self.manifest.read_text())
        data["deltas"][0]["spec"] = "docs/plans/c1-transport-and-tool-registry.md:1"
        self.manifest.write_text(json.dumps(data))
        check = self.run_command("python3", "-I", "hack/plans/parity.py", "--check", check=False)
        self.assertNotEqual(0, check.returncode)
        check = self.run_command("make", "check", check=False)
        self.assertNotEqual(0, check.returncode)
        self.assertNotIn("go test", check.stdout)

    def test_merge_never_approves_an_undeclared_plan_edit(self):
        self.branches()
        file = self.root / "docs/plans/c1-transport-and-tool-registry.md"
        file.write_text(file.read_text().replace("C1 owns HTTP", "C1 owns changed HTTP", 1))
        self.commit("Undeclared edit")
        result = self.git("merge", "--no-edit", "side", check=False)
        self.assertNotEqual(0, result.returncode)
        self.assertIn("stale spec digest", result.stderr)

    def test_merge_never_repairs_a_stale_input_location(self):
        self.branches()
        data = json.loads(self.manifest.read_text())
        data["deltas"][0]["spec"] = "docs/plans/c1-transport-and-tool-registry.md:1"
        self.manifest.write_text(json.dumps(data, indent=2, ensure_ascii=False) + "\n")
        self.commit("Stale location")
        self.assertNotEqual(0, self.git("merge", "--no-edit", "side", check=False).returncode)


if __name__ == "__main__":
    unittest.main()
