"""Exercise poisoned vendor trees through the actual analyzer launchers."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile


class VendorGateTests:
    def check_vendor_tool(self, manifest, poisoned_package, package_name, entry):
        tools = self.fixture / "hack/tools"
        staging = Path(self.enterContext(tempfile.TemporaryDirectory(dir=self.fixture.parent)))
        shutil.copyfile(tools / manifest, staging / "go.mod")
        shutil.copyfile((tools / manifest).with_suffix(".sum"), staging / "go.sum")
        vendor = self.fixture / "vendor"
        subprocess.run(["go", "mod", "vendor", "-o", str(vendor)], cwd=staging, check=True,
            env={**os.environ, "GOWORK": "off", "GOFLAGS": "-p=2 -buildvcs=false", "GOTOOLCHAIN": "local"},
            capture_output=True, text=True, timeout=600)
        poison = vendor / poisoned_package / "zz_vendor_probe.go"
        self.assertTrue(poison.parent.is_dir())
        poison.write_text('package ' + package_name + '\nimport "os"\n'
            'func init(){ if p:=os.Getenv("GATE_BRANCH_MARKER"); p!="" { _ = os.WriteFile(p, []byte("vendored tool"), 0600) } }\n')
        (self.fixture / "go.work").write_text("go 1.26.0\nuse .\n")
        self.git("add", "vendor/modules.txt", str(poison.relative_to(self.fixture)), "go.work")
        self.git("commit", "-qm", "Add a vendor tree")
        gate = self.run_gate()
        self.assertEqual(0, gate.returncode, gate.stdout)
        marker = staging / "vendor-ran"
        result = subprocess.run(entry, cwd=self.fixture, text=True, capture_output=True, check=False,
            env={**os.environ, "GOFLAGS": "-mod=vendor", "GOWORK": str(self.fixture / "go.work"),
                 "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOMAXPROCS": "2", "GATE_BRANCH_MARKER": str(marker)},
            timeout=900)
        self.assertEqual(0, result.returncode, (result.stdout + result.stderr)[-2000:])
        self.assertFalse(marker.exists(), marker.read_text() if marker.exists() else "")
        changed = next(line for line in gate.stdout.splitlines() if line.startswith("GATE FILES CHANGED:"))
        self.assertIn("vendor/modules.txt", changed)
        self.assertIn(str(poison.relative_to(self.fixture)), changed)

    def test_vendor_cannot_replace_deadcode(self):
        self.check_vendor_tool("deadcode.mod", "golang.org/x/tools/go/ssa", "ssa", ["python3", "-I", "-c",
            'import runpy, sys; g=runpy.run_path("hack/guards/run.py"); r=g["analyzer"]("-help"); print(r.stdout); sys.exit(r.returncode)'])

    def test_vendor_cannot_replace_lint_launcher(self):
        self.check_vendor_tool("go.mod", "github.com/golangci/golangci-lint/v2/cmd/golangci-lint", "main",
                               ["bash", "hack/lint-tool.sh", "version", "--short"])
