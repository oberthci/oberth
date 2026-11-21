"""Build one immutable verifier executable for the test process."""
import atexit
from functools import cache
import os
from pathlib import Path
import subprocess
import tempfile


@cache
def compiled_guard():
    scratch = Path(os.environ.get("GOTMPDIR", Path.home() / ".cache/go-tmp"))
    scratch.mkdir(parents=True, exist_ok=True)
    directory = tempfile.TemporaryDirectory(dir=scratch)
    atexit.register(directory.cleanup)
    binary = str(Path(directory.name) / "guards")
    subprocess.run(["go", "build", "-p=2", "-buildvcs=false", "-o", binary, "./hack/guards"],
                   cwd=Path(__file__).resolve().parents[2], check=True,
                   env={**os.environ, "GOTMPDIR": str(scratch), "GOMAXPROCS": "2"})
    os.chmod(binary, 0o555)
    return binary
