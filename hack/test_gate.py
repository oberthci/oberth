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


class GateVerdicts(unittest.TestCase):
    def setUp(self):
        scratch = Path(os.environ.get("GOTMPDIR", Path.home() / ".cache/go-tmp"))
        scratch.mkdir(parents=True, exist_ok=True)
        self.directory = tempfile.TemporaryDirectory(dir=scratch)
        self.addCleanup(self.directory.cleanup)
        self.fixture = Path(self.directory.name)
        root = Path(__file__).resolve().parents[1]
        manifest = re.search(r"tool_paths=\((.*?)\)", (root / "hack/gate.sh").read_text(), re.S)
        plans = [str(p.relative_to(root)) for p in (root / "docs/plans").rglob("*") if p.is_file()]
        # Contents only: a step receives the checkout with read-only files and the fixture edits its copies.
        inputs = ["hack/base-image/inputs.lock.json", "hack/base-image/git-no-shell.patch", "internal/staticgit/build.env"]
        for name in [*shlex.split(manifest[1]), *plans, *inputs]:
            source = root / name
            files = source.rglob("*") if source.is_dir() else [source]
            for file in files:
                if file.is_file():
                    target = self.fixture / file.relative_to(root)
                    target.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copyfile(file, target)
        self.git("init", "-q", "-b", "main")
        for key in ("user.name", "user.email"):
            self.git("config", key, self.fixture.name)
        self.git("add", ".")
        self.git("commit", "-qm", "Establish tooling")
        self.git("checkout", "-qb", "w0/probe")
        self.names = self.fixture.parent / (self.fixture.name + "-names")
        self.names.write_text("unmatched-"+self.fixture.name+"\n")
        self.addCleanup(self.names.unlink)
        self.bin_path = self.fixture / "bin"
        self.bin_path.mkdir()
        docker = self.bin_path / "docker"
        docker.write_text('''#!/usr/bin/env bash
shift 2
case $1 in
create) printf '%s\\n' "$@" > "$GATE_COMMANDS"; printf 'gate-test-container\\n' ;;
cp) case $2 in
    */tooling.tar) cp "$2" "$GATE_ARCHIVE" ;;
    */source.bundle) cp "$2" "$GATE_BUNDLE" ;;
    esac ;;
start) exit "${GATE_FAKE_START_STATUS:-0}" ;;
esac
''')
        docker.chmod(0o755)

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.fixture, stderr=subprocess.STDOUT)

    def run_gate(self, branch="w0/probe", status=0):
        result = subprocess.run(["bash", "hack/gate.sh", branch], cwd=self.fixture,
            env={**os.environ, "PATH": f"{self.bin_path}:{os.environ['PATH']}",
                 "GOLANGCI_LINT": str(self.bin_path / "docker"),
                 "GOTMPDIR": str(self.fixture), "GATE_FAKE_START_STATUS": str(status),
                 "OBERTH_GUARDS_NAMES_FILE": os.environ.get("OBERTH_GUARDS_NAMES_FILE", str(self.names)),
                 "GATE_ARCHIVE": str(self.fixture / "trusted.tar"),
                 "GATE_BUNDLE": str(self.fixture / "source.bundle"),
                 "GATE_COMMANDS": str(self.fixture / "commands")},
            text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)
        self.assertEqual(list(self.fixture.glob("gate.*")), [])
        return result

    def test_success_verdict_keeps_branch(self):
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout)
        self.assertIn("VERDICT: PASS\nBRANCH: w0/probe", result.stdout)
        self.assertIn("GATE FILES CHANGED: none", result.stdout)
        self.assertEqual((self.fixture / "commands").read_text().splitlines()[-3], "")

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

    def run_vm_script(self, status=0):
        make = self.bin_path / "make"
        make.write_text('#!/usr/bin/env bash\nprintf "%s %s\\n" "$PWD" "$*" >> "$GATE_CHECKS"\n'
                        'exit "${GATE_MAKE_STATUS:-0}"\n')
        make.chmod(0o755)
        vm = Path(self.enterContext(tempfile.TemporaryDirectory(dir=self.fixture.parent)))
        command = (self.fixture / "commands").read_text()
        script = command.split("bash\n-ec\n", 1)[1].rsplit("\ngate\n", 1)[0]
        paths = {"/source.bundle": self.fixture / "source.bundle", "/tooling.tar": self.fixture / "trusted.tar",
                 "/repo.git": vm / "repo.git", "/src": vm / "src", "/branch": vm / "branch", "/go/tmp": vm / "tmp"}
        for original, path in paths.items():
            script = script.replace(original, shlex.quote(str(path)))
        result = subprocess.run(["bash", "-ec", script, "gate", self.git("rev-parse", "HEAD").decode().strip(),
                                 command.splitlines()[-3], self.fixture.name, self.fixture.name],
            cwd=self.fixture, env={**os.environ, "PATH": f"{self.bin_path}:{os.environ['PATH']}",
                "GATE_CHECKS": str(vm / "checks"), "GATE_MAKE_STATUS": str(status), "OBERTH_PARITY_TARGET_FILE": ""},
            text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False, timeout=30)
        return result, vm

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
        for name in files:
            self.fixture.joinpath(name).write_text("# weakened branch tooling\n")
        self.fixture.joinpath("hack/guards/branch_only.py").write_text("raise RuntimeError('branch tooling')\n")
        self.git("add", *files)
        self.git("add", "hack/guards/branch_only.py")
        self.git("commit", "-qm", "Change tooling")
        # Invoke the entry point with its own required shell support intact.
        self.fixture.joinpath("hack/steps.sh").write_bytes(self.git("show", "main:hack/steps.sh"))
        result = self.run_gate()
        self.assertEqual(result.returncode, 0, result.stdout)
        line = next(s for s in result.stdout.splitlines() if s.startswith("GATE FILES CHANGED:"))
        with tarfile.open(self.fixture / "trusted.tar") as archive:
            self.assertNotIn("hack/guards/branch_only.py", archive.getnames())
            for name in files:
                self.assertIn(name, line)
                self.assertEqual(archive.extractfile(name).read(), self.git("show", f"main:{name}"))
        command = (self.fixture / "commands").read_text()
        self.assertEqual(command.splitlines()[-3], line.removeprefix("GATE FILES CHANGED: "))
        self.assertEqual(command.splitlines()[-2:], [self.fixture.name, self.fixture.name])
        self.assertIn('git config user.name "$3"', command)
        self.assertIn('git config user.email "$4"', command)
        self.assertLess(command.index("tar -xf /tooling.tar"), command.index("make -f Makefile check"))
        self.assertLess(command.index('for dir in hack/guards hack/plans hack/devloop docs/plans'), command.index("tar -xf /tooling.tar"))
        self.assertIn('if [[ -n $2 ]]; then timed lint-selftest make -f Makefile lint-selftest; fi', command)
        result, vm = self.run_vm_script()
        self.assertEqual(0, result.returncode, result.stdout)
        self.assertFalse((vm / "src/hack/guards/branch_only.py").exists())

    def test_missing_main_guards_requires_bootstrap(self):
        self.git("checkout", "-q", "main")
        self.git("rm", "-q", "hack/guards/main.go")
        self.git("commit", "-qm", "Remove parser")
        self.git("branch", "-f", "w0/probe", "main")
        result = self.run_gate()
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertIn("main lacks hack/guards/main.go", result.stdout)
        self.assertFalse((self.fixture / "commands").exists())

    def check_trusted_tools(self, files, addition=None):
        for name in files:
            path = self.fixture / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("branch tooling must not run\n")
        if addition:
            (self.fixture / addition).write_text("raise RuntimeError('branch tooling ran')\n")
        self.git("add", *files, *([addition] if addition else []))
        self.git("commit", "-qm", "Change check tooling")
        result = self.run_gate()
        self.assertEqual(0, result.returncode, result.stdout)
        changed = next(line.removeprefix("GATE FILES CHANGED: ")
                       for line in result.stdout.splitlines() if line.startswith("GATE FILES CHANGED: "))
        with tarfile.open(self.fixture / "trusted.tar") as archive:
            for name in files:
                self.assertIn(name, changed.split())
                self.assertEqual(archive.extractfile(name).read(), self.git("show", f"main:{name}"))
            if addition:
                self.assertIn(addition, changed.split())
                self.assertNotIn(addition, archive.getnames())
        command = (self.fixture / "commands").read_text()
        self.assertEqual(command.splitlines()[-3], changed)
        result, vm = self.run_vm_script()
        self.assertEqual(0, result.returncode, result.stdout)
        for name in files:
            self.assertEqual((vm / "src" / name).read_bytes(), self.git("show", f"main:{name}"))
            self.assertEqual((vm / "branch" / name).read_bytes(), self.git("show", f"HEAD:{name}"))
        if addition:
            self.assertFalse((vm / "src" / addition).exists())
            self.assertTrue((vm / "branch" / addition).is_file())

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
            result = subprocess.run(["python3", "-m", "unittest", "discover", "-s", "hack/base-image", "-p", "test_*.py"],
                                    cwd=checkout, capture_output=True, text=True, check=False, timeout=30)
            self.assertEqual(0, result.returncode, result.stdout + result.stderr)
            self.assertIn("Ran 7 tests", result.stderr)

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
            shutil.copyfile(root / name, checkout / name)
        for path in [*checkout.rglob("*"), checkout]:
            path.chmod(path.stat().st_mode & ~0o222)
        tests = ("test_branch_policy_changes_use_target_and_request_branch_check",
                 "test_changed_tooling_uses_main_and_requests_selftest",
                 "test_make_check_rejects_broken_plans_before_go")
        result = subprocess.run(["python3", "-m", "unittest", *(f"test_gate.GateVerdicts.{t}" for t in tests)],
            cwd=checkout / "hack", env={**os.environ, "PYTHONDONTWRITEBYTECODE": "1"},
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
