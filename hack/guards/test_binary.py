"""Build one immutable verifier executable for the test process."""
import atexit
from functools import cache
import os
from pathlib import Path
import runpy
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]
GO = runpy.run_path(str(ROOT/"hack/tools/go_tool.py"))


@cache
def compiled_guard():
    scratch = Path(os.environ.get("GOTMPDIR", Path.home() / ".cache/go-tmp"))
    scratch.mkdir(parents=True, exist_ok=True)
    directory = tempfile.TemporaryDirectory(dir=scratch)
    atexit.register(directory.cleanup)
    binary = str(Path(directory.name) / "guards")
    subprocess.run(GO["args"](ROOT, "build", "-p=2", "-buildvcs=false", "-o", binary, "./hack/guards"),
                   cwd=ROOT, check=True,
                   env={**GO["environment"](), "GOTMPDIR": str(scratch), "GOMAXPROCS": "2"})
    os.chmod(binary, 0o555)
    return binary
