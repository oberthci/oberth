#!/usr/bin/env python3
"""Check real pinned Argo Pod mount ordering for compiled assist Workflows."""
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile


def main():
    root = Path(__file__).resolve().parent.parent
    env = {**os.environ, "GOMAXPROCS": "2"}
    captured = subprocess.run(
        ["go", "test", "./internal/argojob", "-run",
         "^TestBuildParentMountsLeaveHelperLast$", "-count=1", "-v"],
        cwd=root, env=env, text=True, stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT, check=True).stdout
    match = re.search(r"HELPER_MOUNT_PROOF=(.+)", captured)
    if match is None:
        raise SystemExit("missing compiled Workflow mount proof")
    module = json.loads(subprocess.check_output(
        ["go", "list", "-m", "-json", "github.com/argoproj/argo-workflows/v4"],
        cwd=root, env=env, text=True))
    print(json.dumps({k: module[k] for k in ("Path", "Version", "Sum", "GoModSum")}),
          flush=True)
    with tempfile.TemporaryDirectory(prefix="oberth-helper-mounts-") as tmp:
        source = Path(tmp, "argo")
        shutil.copytree(module["Dir"], source)
        controller = source / "workflow/controller"
        controller.chmod(0o755)
        shutil.copyfile(root / "hack/testdata/helper-mounts/controller_test.go",
                        controller / "oberth_helper_mounts_test.go")
        for name in ("go.mod", "go.sum"):
            (source / name).chmod(0o644)
        posted = Path(tmp, "compiled-workflows.json")
        posted.write_text(match.group(1) + "\n")
        subprocess.run(["go", "test", "./workflow/controller", "-run",
                        "^TestOberthParentMountLeavesHelperAccessible$", "-count=1",
                        "-v", "-p=2", "-args", str(posted)],
                       cwd=source, env=env, check=True)


if __name__ == "__main__":
    main()
