"""Run the contracts with an unrelated HOME and checkout path."""
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


class PortabilityContract(unittest.TestCase):
    def test_unrelated_home_and_checkout_path(self):
        source = Path(__file__).resolve().parent.parent
        with tempfile.TemporaryDirectory(prefix="portable checkout ") as directory:
            root = Path(directory)
            checkout, home, scratch = (root / name for name in ("checkout space", "empty home", "scratch space"))
            home.mkdir()
            scratch.mkdir()
            shutil.copytree(source / "agent", checkout / "agent", ignore=shutil.ignore_patterns("__pycache__"))
            for name in ("internal/api/rpc.go", "internal/stepmodel/enums.go", "internal/releaseverify/release-cosign.pub", "internal/releaseverify/origin.go", "hack/release-origin.py", "internal/workflowpolicy/release-version.pattern"):
                if not (source / name).exists():
                    continue
                destination = checkout / name
                destination.parent.mkdir(parents=True, exist_ok=True)
                shutil.copy2(source / name, destination)
            shutil.copytree(source / "internal/apicontract", checkout / "internal/apicontract")
            env = dict(os.environ, HOME=str(home), TMPDIR=str(scratch), PYTHONDONTWRITEBYTECODE="1")
            if not env.get("OBERTH_CLI"):
                executable = str(scratch / "oberth")
                subprocess.run(["go", "build", "-p=1", "-o", executable, "./cmd/oberth"], cwd=source, check=True)
                env["OBERTH_CLI"] = executable
            result = subprocess.run([sys.executable, "-m", "unittest", "test_fetch", "test_fetch_install", "test_guidance"], cwd=checkout / "agent", env=env, capture_output=True, timeout=60)
            self.assertEqual(result.returncode, 0, result.stdout.decode() + result.stderr.decode())
            self.assertFalse((home / "work").exists())


if __name__ == "__main__":
    unittest.main()
