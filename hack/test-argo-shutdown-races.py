#!/usr/bin/env python3
"""Run real pinned Argo races without adding controller dependencies to Oberth."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


PACKAGE = "github.com/oberthci/oberth/hack/argo-shutdown-races"


def run(json_events):
    root = Path(__file__).resolve().parent.parent
    # Authenticate test-only dependencies using the shipped checksums and the
    # exact pinned Argo module's own checksum manifest.
    upstream = json.loads(subprocess.check_output(
        ["go", "list", "-m", "-json", "github.com/argoproj/argo-workflows/v4"],
        cwd=root, text=True))
    checksums = set((root / "go.sum").read_text().splitlines())
    checksums.update((Path(upstream["Dir"]) / "go.sum").read_text().splitlines())
    with tempfile.TemporaryDirectory(prefix="oberth-argo-shutdown-races-") as tmp:
        module = Path(tmp)
        shutil.copyfile(root / "hack/testdata/argo-shutdown-races/race_test.go.txt",
                        module / "race_test.go")
        # The path grants the test access to Oberth's internal production API.
        # Only this disposable module acquires upstream utility/test dependencies.
        (module / "go.mod").write_text(
            "module github.com/oberthci/oberth/hack/argo-shutdown-races\n\n"
            "go 1.26.0\n\nrequire github.com/oberthci/oberth v0.0.0\n")
        (module / "go.sum").write_text("\n".join(sorted(checksums)) + "\n")
        env = {**os.environ, "GOWORK": "off", "GOMAXPROCS": "2",
               "GOPATH": str(module / "gopath")}
        subprocess.run(["go", "mod", "edit",
                        "-replace=github.com/oberthci/oberth=" + str(root)],
                       cwd=module, env=env, check=True)
        subprocess.run(["go", "test", "-mod=mod", "-race",
                        "-gcflags=modernc.org/sqlite/...=-race=false",
                        "-gcflags=modernc.org/libc/...=-race=false",
                        "-count=1", "-p=2", "-timeout=3m", "-json" if json_events else "-v", "."],
                       cwd=module, env=env, check=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--json", action="store_true")
    args = parser.parse_args()
    try:
        run(args.json)
    except (subprocess.CalledProcessError, OSError, ValueError, KeyError):
        if args.json:
            # Setup/build failures may have no Go test event. Publish only the
            # owning fixture package, never captured output or exception text.
            print(json.dumps(dict(Action="fail", Package=PACKAGE)), flush=True)
        else:
            print("Argo shutdown race fixture failed: " + PACKAGE, file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
