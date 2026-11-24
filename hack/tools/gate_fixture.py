"""Shared local repositories and VM-script fixture for gate tests."""
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import tarfile
import tempfile


def nested_environment():
    return {name: value for name, value in os.environ.items()
            if name not in ("OBERTH_TOOLS_MODFILE", "GOLANGCI_LINT")}


class GateFixture:
    def setUp(self):
        scratch = Path(os.environ.get("GOTMPDIR", Path.home() / ".cache/go-tmp"))
        scratch.mkdir(parents=True, exist_ok=True)
        self.directory = tempfile.TemporaryDirectory(dir=scratch)
        self.addCleanup(self.directory.cleanup)
        self.fixture = Path(self.directory.name)
        root = Path(__file__).resolve().parents[2]
        manifest = re.search(r"tool_paths=\((.*?)\)", (root / "hack/gate.sh").read_text(), re.S)
        plans = [str(p.relative_to(root)) for p in (root / "docs/plans").rglob("*") if p.is_file()]
        # Preserve executable files while keeping copies of read-only inputs editable.
        inputs = ["go.mod", "go.sum", "hack/guards", "hack/base-image/inputs.lock.json", "hack/base-image/git-no-shell.patch", "internal/staticgit/build.env",
                  "internal/staticgit/qualified-artifacts.json"]
        for name in [*shlex.split(manifest[1]), *plans, *inputs]:
            source = root / name
            files = source.rglob("*") if source.is_dir() else [source]
            for file in files:
                if file.is_file() and file.suffix != ".pyc":
                    target = self.fixture / file.relative_to(root)
                    target.parent.mkdir(parents=True, exist_ok=True)
                    shutil.copyfile(file, target)
                    target.chmod(file.stat().st_mode | 0o200)
        self.packaged_git_body = b"#!/bin/sh\nexit 0\n"
        record = self.fixture / "internal/staticgit/qualified-artifacts.json"
        data = json.loads(record.read_text())
        data["amd64"]["git_sha256"] = hashlib.sha256(self.packaged_git_body).hexdigest()
        record.write_text(json.dumps(data))
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
        self.packaged_git = self.bin_path / "packaged-git"
        self.packaged_git.write_bytes(self.packaged_git_body)
        self.packaged_git.chmod(0o755)
        docker = self.bin_path / "docker"
        docker.write_text('''#!/usr/bin/env bash
shift 2
case $1 in
image) if [[ ${@: -1} == '{{.Architecture}}' ]]; then printf 'amd64\\n'; fi ;;
create) printf '%s\\n' "$@" > "$GATE_COMMANDS"; printf 'gate-test-container\\n' ;;
cp) case $2 in
    */tooling.tar) cp "$2" "$GATE_ARCHIVE" ;;
    */source.bundle) cp "$2" "$GATE_BUNDLE" ;;
    */go.mod) cp "$2" "$GATE_MAIN_MOD" ;;
    */go.sum) cp "$2" "$GATE_MAIN_SUM" ;;
    esac ;;
start) exit "${GATE_FAKE_START_STATUS:-0}" ;;
esac
''')
        docker.chmod(0o755)

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.fixture, stderr=subprocess.STDOUT)

    def run_gate(self, branch="w0/probe", status=0):
        result = subprocess.run(["bash", "hack/gate.sh", branch], cwd=self.fixture,
            env={**nested_environment(), "PATH": f"{self.bin_path}:{os.environ['PATH']}",
                 "GOLANGCI_LINT": str(self.bin_path / "docker"),
                 "OBERTH_TEST_GIT": str(self.packaged_git),
                 "GOTMPDIR": str(self.fixture), "GATE_FAKE_START_STATUS": str(status),
                 "OBERTH_GUARDS_NAMES_FILE": os.environ.get("OBERTH_GUARDS_NAMES_FILE", str(self.names)),
                 "GATE_ARCHIVE": str(self.fixture / "trusted.tar"),
                 "GATE_BUNDLE": str(self.fixture / "source.bundle"),
                 "GATE_MAIN_MOD": str(self.fixture / "trusted.mod"),
                 "GATE_MAIN_SUM": str(self.fixture / "trusted.sum"),
                 "GATE_COMMANDS": str(self.fixture / "commands")},
            text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False)
        self.assertEqual(list(self.fixture.glob("gate.*")), [])
        return result

    def run_vm_script(self, status=0, make_body="", real_make=False, timeout=30):
        make = self.bin_path / "make"
        if not real_make:
            make.write_text('#!/usr/bin/env bash\nprintf "%s %s\\n" "$PWD" "$*" >> "$GATE_CHECKS"\n'
                            + make_body + 'exit "${GATE_MAKE_STATUS:-0}"\n')
            make.chmod(0o755)
        vm = Path(self.enterContext(tempfile.TemporaryDirectory(dir=self.fixture.parent)))
        for suffix in ("mod", "sum"):
            shutil.copyfile(self.fixture / ("trusted." + suffix), vm / ("trusted." + suffix))
        command = (self.fixture / "commands").read_text()
        script = command.split("bash\n-ec\n", 1)[1].rsplit("\ngate\n", 1)[0]
        paths = {"/source.bundle": self.fixture / "source.bundle", "/tooling.tar": self.fixture / "trusted.tar",
                 "/repo.git": vm / "repo.git", "/src": vm / "src", "/branch": vm / "branch",
                 "/go/tmp": vm / "tmp", "/guard-names.txt": self.names,
                 "/trusted.mod": vm / "trusted.mod", "/trusted.sum": vm / "trusted.sum"}
        script = re.sub("|".join(map(re.escape, paths)), lambda match: shlex.quote(str(paths[match[0]])), script)
        result = subprocess.run(["bash", "-ec", script, "gate", self.git("rev-parse", "HEAD").decode().strip(),
                                 command.splitlines()[-3], self.fixture.name, self.fixture.name],
            cwd=self.fixture, env={**nested_environment(), "PATH": f"{self.bin_path}:{os.environ['PATH']}",
                "GATE_CHECKS": str(vm / "checks"), "GATE_MAKE_STATUS": str(status), "OBERTH_PARITY_TARGET_FILE": ""},
            text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=False, timeout=timeout)
        return result, vm

    def check_trusted_tools(self, files, addition=None):
        for name in files:
            path = self.fixture / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text("# branch tooling must not run\n")
        if addition:
            (self.fixture / addition).write_text("raise RuntimeError('branch tooling ran')\n")
        self.git("add", *files, *([addition] if addition else []))
        self.git("commit", "-qm", "Change check tooling")
        if "hack/steps.sh" in files:
            self.fixture.joinpath("hack/steps.sh").write_bytes(self.git("show", "main:hack/steps.sh"))
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
