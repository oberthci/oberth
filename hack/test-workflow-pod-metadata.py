#!/usr/bin/env python3
"""Exercise the posted Workflow with the pinned Argo controller's Pod builder.

Workflow metadata is applied by operator.go:updateWorkflowMetadata to the
Workflow object. workflowpod.go:addMetadata copies PodMetadata and template
Metadata to Pods, rather than Workflow-object annotations. This test uses the
compiler's actual API payload, including annotation behavior-switch keys.
"""
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
    root = Path(__file__).resolve().parent.parent
    env = {**os.environ, "GOMAXPROCS": "2"}
    capture = run(["go", "test", "./internal/argojob", "-run",
                   "^TestNativeAuthorLabelsAndAntiAffinityReachWorkflowAPI$",
                   "-count=1", "-v"], root, env)
    match = re.search(r"POD_METADATA_PROOF=(.+)", capture)
    if match is None:
        raise SystemExit("missing posted Workflow proof")
    module = json.loads(subprocess.check_output(
        ["go", "list", "-m", "-json", "github.com/argoproj/argo-workflows/v4"],
        cwd=root, env=env, text=True))
    print(json.dumps({k: module[k] for k in ("Path", "Version", "Sum", "GoModSum")}),
          flush=True)
    with tempfile.TemporaryDirectory(prefix="oberth-pod-metadata-") as tmp:
        source = Path(tmp, "argo")
        shutil.copytree(module["Dir"], source)
        controller = source / "workflow/controller"
        controller.chmod(0o755)
        shutil.copyfile(root / "hack/testdata/native-pod-metadata/controller_test.go",
                        controller / "oberth_metadata_test.go")
        for name in ("go.mod", "go.sum"):
            (source / name).chmod(0o644)
        posted = Path(tmp, "posted-workflow.json")
        posted.write_text(match.group(1) + "\n")
        run(["go", "test", "./workflow/controller", "-run",
             "^TestOberthPostedAuthorMetadataReachesPod$", "-count=1", "-v",
             "-p=2", "-args", str(posted)], source, env)


if __name__ == "__main__":
    main()
