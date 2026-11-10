#!/usr/bin/python3
"""Run disjoint race-test groups, with new packages included in the rest group."""

import argparse
import json
import os
from pathlib import Path
import subprocess
import sys



# Race instrumentation of the pure-Go SQLite engine and its libc made these
# CPU-bound suites ~30x slower (RCA-STORE-TEST-TIME, 2026-11-10). Oberth's own
# packages stay fully race-instrumented; races inside third-party SQLite are
# not reported.
RACE_SCOPE = ["-gcflags=modernc.org/sqlite/...=-race=false", "-gcflags=modernc.org/libc/...=-race=false"]

def package_groups():
    packages = subprocess.check_output(["go", "list", "./..."], text=True).split()
    if len(packages) != len(set(packages)):
        raise SystemExit("go list returned duplicate packages")
    dedicated = {
        "store": "github.com/oberthci/oberth/internal/store",
        "service": "github.com/oberthci/oberth/internal/service",
        "gitcache": "github.com/oberthci/oberth/internal/gitcache",
    }
    if not set(dedicated.values()).issubset(packages):
        raise SystemExit("dedicated test packages are missing")
    groups = {name: [package] for name, package in dedicated.items()}
    groups["rest"] = [package for package in packages if package not in dedicated.values()]
    if not groups["rest"]:
        raise SystemExit("the remaining package group is empty")
    return groups


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("group", nargs="?", choices=("store", "service", "gitcache", "rest"))
    parser.add_argument("--list", action="store_true", help="print the package partition without running tests")
    args = parser.parse_args()
    if not args.list and args.group is None:
        parser.error("a test group is required")
    groups = package_groups()
    if args.list:
        print(json.dumps(groups, sort_keys=True))
        return
    if args.group == "store":
        os.execv(sys.executable, [sys.executable, "-I", "-S", "-B",
                                 str(Path(__file__).with_name("test-store.py"))])
    # Whole-package race deadlines need max(1.5 * observed, observed + 120s).
    timeout = {"service": "60m", "rest": "55m"}.get(args.group, "20m")
    os.execvp("go", ["go", "test", "-race", *RACE_SCOPE, "-v", "-count=1", "-parallel=2", "-p=2", f"-timeout={timeout}", *groups[args.group]])


if __name__ == "__main__":
    main()
