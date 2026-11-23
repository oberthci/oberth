"""Real Go probes for the trusted-tool and product module boundary."""
import os
from pathlib import Path
import shlex
import subprocess
from unittest.mock import patch


class GoGateTests:
    def test_added_product_dependency_uses_branch_module_files(self):
        root = Path(__file__).resolve().parents[2]
        revision = "e95687a848b6dc636835292289ad8abd7f8c2fd1"
        self.git("checkout", "-q", "main")
        for name in ("go.mod", "go.sum"):
            (self.fixture / name).write_bytes(subprocess.check_output(
                ["git", "show", revision + "^:" + name], cwd=root))
        make = '''.PHONY: check lint-selftest
check:
	go vet -mod=readonly -p=2 ./internal/argojob
	go test -mod=readonly -race -count=1 -timeout=2m ./internal/argojob
	go test -mod=readonly -c -p=1 -o /dev/null ./internal/argojob
	bash hack/check.sh lint
lint-selftest:
	python3 -I -c 'import runpy, subprocess; f=runpy.run_path("hack/guards/test_binary.py"); subprocess.run([f["compiled_guard"](), "--yaml"], input="", text=True, check=True)'
'''
        (self.fixture / "Makefile").write_text(make)
        self.git("add", "go.mod", "go.sum", "Makefile")
        self.git("commit", "-qm", "Establish dependency baseline")
        self.git("checkout", "-qB", "w0/probe", "main")
        for name in ("go.mod", "go.sum"):
            (self.fixture / name).write_bytes(subprocess.check_output(
                ["git", "show", revision + ":" + name], cwd=root))
        product = self.fixture / "internal/argojob"
        product.mkdir(parents=True)
        (product / "policy_test.go").write_text('package argojob\nimport ("testing"; policy "k8s.io/pod-security-admission/api")\n'
            'func TestRestricted(t *testing.T) { if policy.LevelRestricted != "restricted" { t.Fatal("unexpected level") } }\n')
        self.git("add", "go.mod", "go.sum", "internal/argojob")
        self.git("commit", "-qm", "Add the restricted policy dependency")
        gate = self.run_gate()
        self.assertEqual(0, gate.returncode, gate.stdout)
        with patch.dict(os.environ, {"GOWORK": "off", "GOFLAGS": "-mod=readonly -p=2 -buildvcs=false"}):
            result, vm = self.run_vm_script(real_make=True, timeout=240)
        self.assertEqual(0, result.returncode, result.stdout)
        self.assertFalse((self.bin_path / "make").exists())
        self.assertFalse((self.bin_path / "go").exists())
        self.assertIn("go vet -mod=readonly", result.stdout)
        self.assertIn("0 issues", result.stdout)
        for checkout in ("src", "branch"):
            for name in ("go.mod", "go.sum"):
                self.assertEqual((self.fixture / name).read_bytes(), (vm / checkout / name).read_bytes())

    def test_main_module_files_keep_branch_code_out_of_trusted_go(self):
        module = self.fixture / "internal/zzyaml"
        module.mkdir(parents=True)
        (module / "go.mod").write_text("module go.yaml.in/yaml/v3\n\ngo 1.26.0\n")
        (module / "yaml.go").write_text('package yaml\nimport ("io"; "os")\n'
            'type Decoder struct{}\nfunc NewDecoder(io.Reader)*Decoder { return &Decoder{} }\n'
            'func (*Decoder) Decode(any)error { return io.EOF }\n'
            'func init(){ _ = os.WriteFile(os.Getenv("GATE_BRANCH_MARKER"), []byte("branch code"), 0600); os.Exit(0) }\n')
        mod = self.fixture / "go.mod"
        mod.write_text(mod.read_text() + "\nreplace go.yaml.in/yaml/v3 => ./internal/zzyaml\n")
        sums = self.fixture / "go.sum"
        sums.write_text(sums.read_text() + "\n")
        (self.fixture / "go.work").write_text("go 1.26.0\nuse .\nreplace go.yaml.in/yaml/v3 => ./internal/zzyaml\n")
        (self.fixture / "go.work.sum").write_text("\n")
        host = "fixture-host-" + self.fixture.name
        self.names.write_text("host:" + host + "\n")
        agent = self.fixture / "agent/probe.yaml"
        agent.parent.mkdir()
        agent.write_text("name: " + host + "\n")
        files = ("go.mod", "go.sum", "go.work", "go.work.sum")
        self.git("add", *files, "internal/zzyaml", "agent/probe.yaml")
        self.git("commit", "-qm", "Replace a tool dependency")
        with patch.dict(os.environ, {"OBERTH_GUARDS_NAMES_FILE": str(self.names)}):
            gate = self.run_gate()
        self.assertEqual(0, gate.returncode, gate.stdout)
        marker = self.fixture / "module-ran"
        probe = '''import os, runpy, subprocess, sys
g = runpy.run_path("hack/guards/run.py")
if os.environ.get("OBERTH_TOOLS_MODFILE"):
    assert not g["names"]()
e = g["command"](["go", "env", "GOWORK", "GOFLAGS"])
print(e.stdout)
r = g["command"](g["GO"].args(g["ROOT"], "run", "-p=2", "-buildvcs=false", "./hack/guards", "--yaml"), "")
print(r.stdout)
if r.returncode: sys.exit(r.returncode)
f = runpy.run_path("hack/guards/test_binary.py")
subprocess.run([f["compiled_guard"](), "--yaml"], input="", text=True, check=True)
'''
        body = '''if [[ $* == "-f Makefile check" ]]; then
    printf "%s %s\\n" "$PWD" "${OBERTH_TOOLS_MODFILE-unset}" >> "$GATE_CHECKS.env"
    python3 -I -c ''' + shlex.quote(probe) + ''' || exit "$?"
    if [[ $PWD == */src && -e $GATE_BRANCH_MARKER ]]; then
        printf 'branch module executed in trusted guard\\n' >&2; exit 13
    fi
fi
'''
        with patch.dict(os.environ, {"GOWORK": "auto", "GOFLAGS": "", "GATE_BRANCH_MARKER": str(marker),
                                    "OBERTH_GUARDS_NAMES_FILE": str(self.names)}):
            result, vm = self.run_vm_script(make_body=body, timeout=90)
        self.assertEqual(0, result.returncode, result.stdout)
        changed = next(line for line in gate.stdout.splitlines() if line.startswith("GATE FILES CHANGED:"))
        for name in files:
            self.assertIn(name, changed)
        for name in ("go.mod", "go.sum"):
            suffix = name.split(".")[1]
            self.assertEqual(self.git("show", "main:" + name), (vm / ("trusted." + suffix)).read_bytes())
            self.assertEqual((self.fixture / name).read_bytes(), (vm / "src" / name).read_bytes())
            self.assertEqual((self.fixture / name).read_bytes(), (vm / "branch" / name).read_bytes())
        observed = (vm / "checks.env").read_text().splitlines()
        self.assertEqual(f"{vm / 'src'} {vm / 'trusted.mod'}", observed[0])
        self.assertEqual(f"{vm / 'branch'} unset", observed[1])
        self.assertIn("off\n-mod=readonly -p=2 -buildvcs=false", result.stdout)
        self.assertEqual("branch code", marker.read_text())

