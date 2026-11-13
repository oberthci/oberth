#!/usr/bin/env python3
"""Check retry hints against errors emitted by the pinned Argo controller."""
import argparse
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile


def run(args, cwd, env):
    result = subprocess.run(args, cwd=cwd, env=env, text=True,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                            check=False)
    print(result.stdout, end="", flush=True)
    if result.returncode:
        raise SystemExit(result.returncode)
    return result.stdout


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--update", action="store_true", help="record emitted controller errors")
    args = parser.parse_args()
    root = Path(__file__).resolve().parent.parent
    env = {**os.environ, "GOMAXPROCS": "2"}
    module = json.loads(subprocess.check_output(
        ["go", "list", "-m", "-json", "github.com/argoproj/argo-workflows/v4"],
        cwd=root, env=env, text=True))
    print(json.dumps({k: module[k] for k in ("Path", "Version", "Sum", "GoModSum")}),
          flush=True)
    with tempfile.TemporaryDirectory(prefix="oberth-semaphore-proof-") as tmp:
        source = Path(tmp, "argo")
        shutil.copytree(module["Dir"], source)
        controller = source / "workflow/controller"
        controller.chmod(0o755)
        shutil.copyfile(root / "hack/testdata/semaphore-failures/controller_test.go",
                        controller / "oberth_semaphore_test.go")
        for name in ("go.mod", "go.sum"):
            (source / name).chmod(0o644)
        captured = run(["go", "test", "./workflow/controller", "-run",
                        "^TestOberthSemaphoreConfigurationFailures$", "-count=1",
                        "-v", "-p=2"], source, env)
    cases = [json.loads(row) for row in re.findall(r"SEMAPHORE_FAILURE_PROOF=(.+)", captured)]
    if sorted(row["missing"] for row in cases) != ["configmap", "key"]:
        raise SystemExit("missing controller failure cases")
    proof = {"argo_version": module["Version"], "cases": cases}
    fixture = root / "internal/service/testdata/semaphore-failures.json"
    if args.update:
        fixture.parent.mkdir(parents=True, exist_ok=True)
        fixture.write_text(json.dumps(proof, indent=2) + "\n")
    if json.loads(fixture.read_text()) != proof:
        raise SystemExit("controller errors differ; review them before --update")
    run(["go", "test", "./internal/service", "-run",
         "^TestReviewerPinnedArgoSemaphoreHint$", "-count=1", "-v"], root, env)


if __name__ == "__main__":
    main()
