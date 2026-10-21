#!/usr/bin/python3
"""Run disjoint race-test groups, with new packages included in the rest group."""

import argparse
import json
import os
import subprocess


def package_groups():
    packages = subprocess.check_output(["go", "list", "./..."], text=True).split()
    if len(packages) != len(set(packages)):
        raise SystemExit("go list returned duplicate packages")
    dedicated = {
        "store": "github.com/oberthci/oberth/internal/store",
        "service": "github.com/oberthci/oberth/internal/service",
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
    parser.add_argument("group", nargs="?", choices=("store", "service", "rest"))
    parser.add_argument("--list", action="store_true", help="print the package partition without running tests")
    args = parser.parse_args()
    if not args.list and args.group is None:
        parser.error("a test group is required")
    groups = package_groups()
    if args.list:
        print(json.dumps(groups, sort_keys=True))
        return
    os.execvp("go", ["go", "test", "-race", "-v", "-count=1", "-p=2", "-timeout=20m", *groups[args.group]])


if __name__ == "__main__":
    main()
