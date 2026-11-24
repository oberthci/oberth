"""Exercise gate refusals and trusted tooling with local Git and a fake VM."""
import os
import hashlib
import json
import re
import shlex
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest
from unittest.mock import patch

from tools.gate_fixture import GateFixture
from tools.gate_go import GoGateTests
from tools.gate_vendor import VendorGateTests


class GateVerdicts(VendorGateTests, GoGateTests, GateFixture, unittest.TestCase):

    def test_nested_checks_ignore_outer_tool_selection(self):
        path = self.fixture / "hack/ui-check.sh"
        path.write_text("# branch tooling\n")
        self.git("add", "hack/ui-check.sh")
        self.git("commit", "-qm", "Change branch UI tooling")
        with patch.dict(os.environ, {"GOLANGCI_LINT": "/outer/linter", "OBERTH_TOOLS_MODFILE": "/outer/trusted.mod"}):
            gate = self.run_gate()
            self.assertEqual(0, gate.returncode, gate.stdout)
            result, vm = self.run_vm_script(make_body='printf "%s %s\\n" "${GOLANGCI_LINT-unset}" "${OBERTH_TOOLS_MODFILE-unset}" >> "$GATE_CHECKS.tools"\n')
        self.assertEqual(0, result.returncode, result.stdout)
        self.assertEqual([f"unset {vm / 'trusted.mod'}"] * 2 + ["unset unset"],
                         (vm / "checks.tools").read_text().splitlines())



    def test_root_python_cannot_replace_trusted_entries(self):
        files = ("unittest.py", "json.py", "portable.py", "sitecustomize.py", "unittest/__init__.py",
                 "unittest/__main__.py", "json/__init__.py", "zz.pth")
        body = "import os\np = os.environ.get('GATE_BRANCH_MARKER')\nif p:\n open(p, 'w').write(__name__)\n raise SystemExit(0)\n"
        for name in files:
            path = self.fixture / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(body)
        self.git("add", *files)
        self.git("commit", "-qm", "Add root modules")
        marker = self.fixture / "root-ran"
        with patch.dict(os.environ, {"GATE_BRANCH_MARKER": str(marker), "PYTHONPATH": str(self.fixture)}):
            gate = self.run_gate()
        self.assertEqual(0, gate.returncode, gate.stdout)
        self.assertFalse(marker.exists())
        changed = next(line for line in gate.stdout.splitlines() if line.startswith("GATE FILES CHANGED:"))
        for name in files:
            self.assertIn(name, changed)
        result, vm = self.run_vm_script()
        self.assertEqual(0, result.returncode, result.stdout)
        src = vm / "src"
        script = (src / "hack/check.sh").read_text()
        entries = re.findall(r"(?m)^\s*run (python3 .*unittest.*)$", script)
        self.assertEqual(7, len(entries))
        entries.append(re.search(r"check\) run (python3 [^;]+)", script)[1])
        for entry in entries:
            args = shlex.split(entry)
            filtered = "unittest" in args and entry != entries[0]
            if filtered:
                args += ["-k", "_gate_shadow_probe_"]
            result = subprocess.run(args, cwd=src,
                env={**os.environ, "PYTHONPATH": str(src), "GATE_BRANCH_MARKER": str(marker)},
                capture_output=True, text=True, check=False, timeout=30)
            self.assertIn(result.returncode, (0, 5) if filtered else (0,), result.stdout + result.stderr)
            self.assertFalse(marker.exists(), entry)
        self.assertTrue(all((src / name).is_file() for name in files))

    def test_success_verdict_keeps_branch(self):
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("VERDICT: PASS\nBRANCH: w0/probe", result.stdout)
        self.assertIn("GATE FILES CHANGED: none", result.stdout)

    def test_nested_command_failure_keeps_branch_and_status(self):
        result = self.run_gate(status=7)
        self.assertEqual(result.returncode, 7, result.stdout)
        self.assertIn("VERDICT: FAIL\nBRANCH: w0/probe", result.stdout)
        self.assertIn("FAIL (7): timeout", result.stdout)
        self.assertIn("STATUS: 7", result.stdout)

    def test_cache_prefix_keeps_scratch_out_of_shared_volumes(self):
        with patch.dict(os.environ, {"GATE_CACHE_PREFIX": "gate-fixture"}):
            result = self.run_gate()
        self.assertEqual(0, result.returncode, result.stdout)
        command = (self.fixture / "commands").read_text()
        self.assertIn("src=gate-fixture-go-build,dst=/go/cache", command)
        self.assertIn("src=gate-fixture-go-mod,dst=/go/pkg/mod", command)
        self.assertIn("GOCACHE=/go/cache", command)
        self.assertNotIn("dst=/go/tmp", command)
        self.assertIn("mkdir -p /go/tmp", command)
        self.assertIn("OBERTH_GUARDS_NAMES_FILE=/guard-names.txt", command)
        self.assertIn("gate: phase=cleanup seconds=", result.stdout)

    def test_invalid_cache_prefix_is_refused_before_container_creation(self):
        with patch.dict(os.environ, {"GATE_CACHE_PREFIX": "bad,dst=/other"}):
            result = self.run_gate()
        self.assertEqual(2, result.returncode, result.stdout)
        self.assertIn("invalid GATE_CACHE_PREFIX", result.stdout)
        self.assertFalse((self.fixture / "commands").exists())

    def test_gate_budget_covers_both_package_deadlines(self):
        result = self.run_gate()
        self.assertEqual(0, result.returncode, result.stdout)
        command = (self.fixture / "commands").read_text().splitlines()
        self.assertIn("GOMAXPROCS=" + command[command.index("--cpus") + 1], command)
        limit = re.search(r"go test [^\n]*-timeout=(\d+)m", (self.fixture / "hack/check.sh").read_text())
        deadline = re.search(r"--kill-after=15s (\d+)", result.stdout)
        self.assertGreater(int(deadline[1]), 2 * int(limit[1]) * 60)

    def test_packaged_git_uses_main_record_and_reaches_test_override(self):
        record = self.fixture / "internal/staticgit/qualified-artifacts.json"
        data = json.loads(record.read_text())
        unreviewed = b"#!/bin/sh\nexit 1\n"
        data["amd64"]["git_sha256"] = hashlib.sha256(unreviewed).hexdigest()
        record.write_text(json.dumps(data))
        self.git("commit", "-qam", "Change branch qualification")
        result = self.run_gate()
        self.assertEqual(0, result.returncode, result.stdout)
        self.assertIn("OBERTH_TEST_GIT=/usr/local/bin/oberth-test-git", (self.fixture / "commands").read_text())
        self.assertIn(":/usr/local/bin/oberth-test-git", result.stdout)
        (self.fixture / "commands").unlink()
        self.packaged_git.write_bytes(unreviewed)
        result = self.run_gate()
        self.assertNotEqual(0, result.returncode, result.stdout)
        self.assertIn("packaged Git differs from main", result.stdout)
        self.assertFalse((self.fixture / "commands").exists())


    def test_real_product_tree_passes_with_main_tooling_and_keeps_check_failures(self):
        root = Path(__file__).resolve().parents[1]
        self.git("checkout", "-q", "main")
        with tempfile.NamedTemporaryFile(dir=self.fixture.parent) as archive:
            subprocess.run(["git", "archive", "--output=" + archive.name, "HEAD"], cwd=root, check=True)
            subprocess.run(["tar", "-xf", archive.name, "--skip-old-files", "-C", str(self.fixture)], check=True)
        files = subprocess.check_output(["git", "ls-tree", "-rz", "--name-only", "HEAD"], cwd=root).decode().split("\0")[:-1]
        self.git("add", *files)
        self.git("commit", "-qm", "Establish product tree")
        self.git("checkout", "-qB", "w0/probe", "main")
        for changed in (False, True):
            with self.subTest(tooling_changed=changed):
                if changed:
                    (self.fixture / "Makefile").write_text("branch tooling must not run as trusted\n")
                    self.git("add", "Makefile")
                    self.git("commit", "-qm", "Change branch check")
                gate = self.run_gate()
                self.assertEqual(0, gate.returncode, gate.stdout)
                result, vm = self.run_vm_script()
                self.assertEqual(0, result.returncode, result.stdout)
                self.assertTrue((vm / "src/cmd/oberth/main.go").is_file())
                self.assertTrue((vm / "src/charts/oberth/Chart.yaml").is_file())
                self.assertEqual((vm / "src/Makefile").read_bytes(), self.git("show", "main:Makefile"))
                checks = [f"{vm}/src -f Makefile check"]
                if changed:
                    checks += [f"{vm}/src -f Makefile lint-selftest", f"{vm}/branch -f Makefile check"]
                    self.assertEqual((vm / "branch/Makefile").read_bytes(), self.git("show", "HEAD:Makefile"))
                self.assertEqual((vm / "checks").read_text().splitlines(), checks)
                self.assertIn("gate: phase=parity", result.stdout)
                self.assertIn("install: not run; requires a live environment", result.stdout)
                self.assertIn("walk: not run; requires a live environment", result.stdout)
                failure, failed_vm = self.run_vm_script(status=7)
                self.assertEqual(7, failure.returncode, failure.stdout)
                self.assertEqual((failed_vm / "checks").read_text().splitlines(),
                                 [f"{failed_vm}/src -f Makefile check"])

    def test_invalid_branch_has_failure_verdict(self):
        result = self.run_gate(branch="invalid..branch")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("PHASE: resolve", result.stdout)

    def test_branch_missing_main_is_refused_before_vm(self):
        self.git("checkout", "-q", "main")
        self.git("commit", "--allow-empty", "-qm", "Advance main")
        result = self.run_gate()
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn("merge or rebase", result.stdout)
        self.assertFalse((self.fixture / "commands").exists())

    def test_missing_main_tooling_requires_bootstrap(self):
        self.git("checkout", "-q", "main")
        self.git("rm", "-q", "Makefile")
        self.git("commit", "-qm", "Remove tooling")
        self.git("branch", "-f", "w0/probe", "main")
        result = self.run_gate()
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn("bootstrap main tooling first", result.stdout)
        self.assertFalse((self.fixture / "commands").exists())

    def test_changed_tooling_uses_main_and_requests_selftest(self):
        files = ("Makefile", ".golangci.yml", "hack/lint-version", "hack/check.sh",
                 "hack/steps.sh", "hack/history.py", "hack/test_lint.py", "hack/devloop/Dockerfile",
                 "hack/guards/main.go", "hack/guards/run.py")
        self.check_trusted_tools(files, "hack/guards/branch_only.py")
        command = (self.fixture / "commands").read_text()
        self.assertEqual(command.splitlines()[-2:], [self.fixture.name, self.fixture.name])
        self.assertIn('git config user.name "$3"\n        git config user.email "$4"', command)
        self.assertLess(command.index("tar -xf /tooling.tar"), command.index("make -f Makefile check"))
        self.assertIn('if [[ -n $2 ]]; then timed lint-selftest env OBERTH_TOOLS_MODFILE=/trusted.mod make -f Makefile lint-selftest; fi', command)

    def test_missing_main_guards_requires_bootstrap(self):
        self.git("checkout", "-q", "main")
        self.git("rm", "-q", "hack/guards/main.go")
        self.git("commit", "-qm", "Remove parser")
        self.git("branch", "-f", "w0/probe", "main")
        result = self.run_gate()
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn("main lacks hack/guards/main.go", result.stdout)
        self.assertFalse((self.fixture / "commands").exists())


    def test_lint_launcher_and_modules_use_main_tooling(self):
        self.check_trusted_tools(("hack/lint-tool.sh", "hack/tools/go.mod", "hack/tools/go.sum",
                                  "hack/tools/deadcode.mod", "hack/tools/deadcode.sum"),
                                 "hack/tools/branch.go")

    def test_docs_check_uses_main_tooling(self):
        self.check_trusted_tools(("hack/test_docs.py",))

    def test_base_image_checks_use_main_logic(self):
        self.check_trusted_tools(("hack/base-image/test_driver.py", "hack/base-image/test_recipe.py",
                                  "hack/base-image/build.py", "hack/base-image/layout.py", "hack/base-image/check.sh",
                                  "hack/base-image/compile.sh", "hack/base-image/Dockerfile", "hack/base-image/Dockerfile.layout"),
                                 "hack/base-image/test_branch.py")
        recipe = self.fixture / "hack/base-image/build.py"
        recipe.unlink()
        recipe.mkdir()
        (recipe / "input").write_text("branch directory\n")
        self.git("add", "hack/base-image/build.py")
        self.git("commit", "-qm", "Replace a recipe with a directory")
        self.assertEqual(0, self.run_gate().returncode)
        result, vm = self.run_vm_script()
        self.assertEqual(0, result.returncode, result.stdout)
        self.assertEqual(self.git("show", "main:hack/base-image/build.py"), (vm / "src/hack/base-image/build.py").read_bytes())

    def test_base_image_patch_and_pin_update_pass_main_checks(self):
        patch_file = self.fixture / "hack/base-image/git-no-shell.patch"
        patch_file.write_bytes(patch_file.read_bytes() + b"\n# revised input\n")
        pins = self.fixture / "internal/staticgit/build.env"
        pins.write_text(re.sub(r"GIT_PATCH_SHA256=.*", "GIT_PATCH_SHA256=" + hashlib.sha256(patch_file.read_bytes()).hexdigest(),
                               pins.read_text()))
        self.git("add", "hack/base-image/git-no-shell.patch", "internal/staticgit/build.env")
        self.git("commit", "-qm", "Update reviewed patch and pin")
        gate = self.run_gate()
        self.assertEqual(0, gate.returncode, gate.stdout)
        self.assertIn("hack/base-image/git-no-shell.patch", gate.stdout)
        result, vm = self.run_vm_script()
        self.assertEqual(0, result.returncode, result.stdout)
        for checkout in (vm / "src", vm / "branch"):
            self.assertEqual((checkout / "hack/base-image/git-no-shell.patch").read_bytes(), patch_file.read_bytes())
            result = subprocess.run(["python3", "-I", "-m", "unittest", "discover", "-s", "hack/base-image", "-p", "test_*.py"],
                                    cwd=checkout, capture_output=True, text=True, check=False, timeout=30)
            self.assertEqual(0, result.returncode, result.stdout + result.stderr)
            self.assertIn("Ran 7 tests", result.stderr)

    def test_added_hack_files_are_listed_and_never_run_as_trusted(self):
        body = "import os\nfrom pathlib import Path\nPath(os.environ['GATE_BRANCH_MARKER']).write_text(__name__)\n"
        self.git("checkout", "-q", "main")
        (self.fixture / "hack/component.py").write_text(body)
        self.git("add", "hack/component.py")
        self.git("commit", "-qm", "Establish movable component")
        self.git("checkout", "-qB", "w0/probe", "main")
        files = ("hack/zz/__init__.py", "hack/zz/test_history.py", "hack/tempfile.py", "hack/new-tool")
        for name in files:
            path = self.fixture / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(body)
        self.git("mv", "hack/component.py", "hack/renamed.py")
        self.git("add", *files)
        self.git("commit", "-qm", "Add unreviewed tooling")
        gate = self.run_gate()
        self.assertEqual(0, gate.returncode, gate.stdout)
        changed = next(line for line in gate.stdout.splitlines() if line.startswith("GATE FILES CHANGED:"))
        for name in (*files, "hack/renamed.py"):
            self.assertIn(name, changed)
        result, vm = self.run_vm_script()
        self.assertEqual(0, result.returncode, result.stdout)
        for name in (*files, "hack/renamed.py"):
            self.assertFalse((vm / "src" / name).exists(), name)
            self.assertTrue((vm / "branch" / name).is_file(), name)
        result = subprocess.run(["python3", "-I", "-m", "unittest", "discover", "-s", "hack", "-p", "test_history.py"],
            cwd=vm / "src", env={**os.environ, "GATE_BRANCH_MARKER": str(vm / "branch-ran")},
            capture_output=True, text=True, check=False, timeout=30)
        self.assertEqual(0, result.returncode, result.stdout + result.stderr)
        self.assertFalse((vm / "branch-ran").exists())

    def test_main_guard_and_plan_additions_are_in_the_trusted_bundle(self):
        self.git("checkout", "-q", "main")
        files = ("hack/guards/requirements_test.go", "hack/guards/test_walk.py", "hack/plans/extra.py")
        for file in files:
            (self.fixture / file).write_text("approved tooling\n")
        self.git("add", *files)
        self.git("commit", "-qm", "Extend approved tooling")
        self.git("checkout", "-qB", "w0/probe", "main")
        result = self.run_gate()
        self.assertEqual(0, result.returncode, result.stdout)
        with tarfile.open(self.fixture / "trusted.tar") as archive:
            for file in files:
                self.assertEqual(b"approved tooling\n", archive.extractfile(file).read())

    def test_external_names_check_uses_branch_before_vm(self):
        with tempfile.NamedTemporaryFile(mode="w", dir=self.fixture.parent) as names:
            token = "forbidden-" + "fixture-value"
            names.write(token + "\n")
            names.flush()
            self.fixture.joinpath("example.txt").write_text(token)
            self.git("add", "example.txt")
            self.git("commit", "-qm", "Add fixture text")
            with patch.dict(os.environ, {"OBERTH_GUARDS_NAMES_FILE": names.name}):
                result = self.run_gate()
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn("[NAME]", result.stdout)
        self.assertIn("PHASE: names", result.stdout)
        self.assertFalse((self.fixture / "commands").exists())

    def test_component_and_fixture_files_keep_branch_contents(self):
        self.git("checkout", "-q", "main")
        files = ("hack/pipeline.go", "hack/testdata/input.json", "hack/upgrade-e2e/fixture/value.go")
        for name in files:
            path = self.fixture / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("main contents\n")
        self.git("add", *files)
        self.git("commit", "-qm", "Add component fixtures")
        self.git("checkout", "-qB", "w0/probe", "main")
        for name in files:
            (self.fixture / name).write_text("branch contents\n")
        self.git("add", *files)
        self.git("commit", "-qm", "Update component fixtures")
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("GATE FILES CHANGED: none", result.stdout)
        result, vm = self.run_vm_script()
        self.assertEqual(0, result.returncode, result.stdout)
        for name in files:
            self.assertEqual("branch contents\n", (vm / "src" / name).read_text())

    def test_check_runs_guard_fixtures_and_propagates_failure(self):
        go = self.bin_path / "go"
        go.write_text("#!/usr/bin/env bash\nexit 0\n")
        go.chmod(0o755)
        python = self.bin_path / "python3"
        python.write_text('#!/usr/bin/env bash\nprintf "%s\\\n" "$*" >> "$CHECK_COMMANDS"\n'
                          'if [[ "$*" == *"-s hack/guards"* ]]; then exit 7; fi\n')
        python.chmod(0o755)
        result = subprocess.run(["bash", "hack/check.sh", "test"], cwd=self.fixture,
            env={**os.environ, "PATH": str(self.bin_path) + ":" + os.environ["PATH"],
                 "GOTMPDIR": str(self.fixture), "CHECK_COMMANDS": str(self.fixture / "checks")},
            text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)
        self.assertEqual(7, result.returncode, result.stdout)
        self.assertIn("-m unittest discover -s hack/guards -p test_*.py",
                      (self.fixture / "checks").read_text())

    def test_make_check_rejects_broken_plans_before_go(self):
        register = self.fixture / "docs/plans/retired.json"
        data = json.loads(register.read_text())
        data["rules"][0]["tokens"] = []
        register.write_text(json.dumps(data))
        go = self.bin_path / "go"
        go.write_text('#!/usr/bin/env bash\nprintf "started\\n" > "$CHECK_GO_STARTED"\nexit 97\n')
        go.chmod(0o755)
        marker = self.fixture / "go-started"
        result = subprocess.run(["make", "check"], cwd=self.fixture,
            env={**os.environ, "PATH": str(self.bin_path) + ":" + os.environ["PATH"],
                 "GOTMPDIR": str(self.fixture), "CHECK_GO_STARTED": str(marker)},
            text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)
        self.assertEqual(2, result.returncode, result.stdout)
        self.assertIn("retirement selectors missing", result.stdout)
        self.assertFalse(marker.exists(), result.stdout)

    def test_fixture_edits_its_copies_of_a_read_only_checkout(self):
        # A pipeline step receives the checkout with read-only files.
        root = Path(__file__).resolve().parents[1]
        checkout = Path(self.enterContext(tempfile.TemporaryDirectory(dir=self.fixture.parent)))
        for name in ["hack/test_gate.py", *self.git("ls-files", "-z").decode().split("\0")[:-1]]:
            (checkout / name).parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(root / name, checkout / name)
        for path in [*checkout.rglob("*"), checkout]:
            path.chmod(path.stat().st_mode & ~0o222)
        tests = ("test_branch_policy_changes_use_target_and_request_branch_check",
                 "test_changed_tooling_uses_main_and_requests_selftest",
                 "test_make_check_rejects_broken_plans_before_go")
        result = subprocess.run(["python3", "-I", "-B", "-m", "unittest", "discover", "-s", str(checkout / "hack"),
                                 "-p", "test_gate.py", *(arg for test in tests for arg in ("-k", test))],
            cwd=checkout / "hack", env=os.environ,
            text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)
        self.assertEqual(0, result.returncode, result.stdout)
        self.assertIn("Ran 3 tests", result.stdout)

    def test_missing_names_policy_refuses_gate(self):
        with patch.dict(os.environ, {"OBERTH_GUARDS_NAMES_FILE":""}):
            result=self.run_gate()
        self.assertEqual(1,result.returncode,result.stdout)
        self.assertIn("external names policy is required",result.stdout)
        self.assertFalse((self.fixture/"commands").exists())

    def test_new_browser_module_uses_base_tooling_and_checks_branch(self):
        name = "hack/webui/authentication.mjs"
        self.git("checkout", "-q", "main")
        self.git("rm", "-q", name)
        self.git("commit", "-qm", "Keep the base browser harness")
        self.git("checkout", "-qB", "w0/probe", "main")
        (self.fixture / name).write_text("branch browser assertions\n")
        self.git("add", name)
        self.git("commit", "-qm", "Extend browser assertions")
        result = self.run_gate()
        self.assertEqual(0, result.returncode, result.stdout)
        self.assertIn("GATE FILES CHANGED: " + name + "\n", result.stdout)
        with tarfile.open(self.fixture / "trusted.tar") as archive:
            self.assertNotIn(name, archive.getnames())
            self.assertEqual(self.git("show", "main:hack/webui/behavior.mjs"), archive.extractfile("hack/webui/behavior.mjs").read())
        result, vm = self.run_vm_script()
        self.assertEqual(0, result.returncode, result.stdout)
        self.assertFalse((vm / "src" / name).exists())
        self.assertTrue((vm / "branch" / name).is_file())

    def test_branch_policy_changes_use_target_and_request_branch_check(self):
        data=json.loads((self.fixture/"docs/plans/dependencies.json").read_text())
        data["allow"]["C2"].append("internal/api")
        data["rules"]=[r for r in data["rules"] if r["id"] not in (2,3)]
        (self.fixture/"docs/plans/dependencies.json").write_text(json.dumps(data))
        self.git("add","docs/plans/dependencies.json")
        self.git("commit","-qm","Change branch policy")
        result=self.run_gate()
        self.assertEqual(0,result.returncode,result.stdout)
        self.assertIn("GATE FILES CHANGED: docs/plans/dependencies.json",result.stdout)
        with tarfile.open(self.fixture/"trusted.tar") as archive:
            self.assertEqual(self.git("show","main:docs/plans/dependencies.json"),
                             archive.extractfile("docs/plans/dependencies.json").read())
        command=(self.fixture/"commands").read_text()
        self.assertIn("timed branch-check make",command)
        self.assertIn("worktree add -q --detach /branch",command)

    def test_branch_cannot_remove_existing_parity(self):
        self.git("checkout","-q","main")
        p=self.fixture/"test/parity/corpus.json";p.parent.mkdir(parents=True);p.write_text("{}")
        self.git("add",str(p));self.git("commit","-qm","Add parity")
        self.git("checkout","-qB","w0/probe","main")
        self.git("rm","test/parity/corpus.json");self.git("commit","-qm","Remove parity")
        result=self.run_gate()
        self.assertEqual(1,result.returncode,result.stdout)
        self.assertIn("parity harness was removed",result.stdout)
