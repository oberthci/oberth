"""Select the module closure for trusted Go tool builds."""
import atexit
from functools import cache
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


def args(root, action, *arguments):
    modfile = os.environ.get("OBERTH_TOOLS_MODFILE", str(root / "go.mod"))
    return ["go", action, "-modfile=" + modfile, *arguments]


def environment():
    return {**os.environ, "GOWORK": "off", "GOFLAGS": "-mod=readonly -p=2 -buildvcs=false", "GOTOOLCHAIN": "local"}


@cache
def binary(modfile, package):
    scratch = Path(os.environ.get("GOTMPDIR", Path.home() / ".cache/go-tmp"))
    scratch.mkdir(parents=True, exist_ok=True)
    directory = tempfile.TemporaryDirectory(prefix="oberth-tool-", dir=scratch)
    root = Path(directory.name)
    program = root / "tool"
    try:
        shutil.copyfile(modfile, root / "go.mod")
        shutil.copyfile(modfile.with_suffix(".sum"), root / "go.sum")
        subprocess.run(["go", "build", "-mod=readonly", "-modfile=" + str(root / "go.mod"),
                        "-o", str(program), package], cwd=root, env=environment(),
                       text=True, capture_output=True, check=True)
        program.chmod(0o555)
    except BaseException:
        directory.cleanup()
        raise
    atexit.register(directory.cleanup)
    return str(program)


def tool(modfile, package, execute, *arguments):
    try:
        program = binary(Path(modfile).resolve(), package)
    except subprocess.CalledProcessError as error:
        return subprocess.CompletedProcess(error.cmd, error.returncode,
                                           stdout=(error.stdout or "") + (error.stderr or ""))
    return execute([program, *arguments], env=environment())


if __name__ == "__main__":
    modules = {"lint": ("go.mod", "github.com/golangci/golangci-lint/v2/cmd/golangci-lint"),
               "deadcode": ("deadcode.mod", "golang.org/x/tools/cmd/deadcode")}
    manifest, package = modules[sys.argv[1]]
    result = tool(Path(__file__).with_name(manifest), package,
                  lambda argv, **options: subprocess.run(argv, **options), *sys.argv[2:])
    if result.stdout:
        print(result.stdout, end="", file=sys.stderr if result.returncode else sys.stdout)
    sys.exit(result.returncode)
