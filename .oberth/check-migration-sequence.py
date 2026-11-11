#!/usr/bin/env python3
"""Check the store migration sequence (#1524).

.oberth/migration-sequence.txt lists every timestamped store migration ID in
apply order, one per line, and only ever grows at its end. hack/new-migration.py
appends the new ID. A new migration must therefore be numbered main's latest+1:
its ID sorts after every ID already listed. The check refuses

- an ID listed twice, or two migrations that share a timestamp (a collision);
- a migration file that is not listed, or a listed ID with no file;
- a list that is not strictly increasing, which is what a migration created
  before main's latest and merged after it looks like once appended.

With --base <git-ref> (a local checkout with history), the base's list must
also be an unchanged prefix of this one: shipped IDs are never edited,
removed or reordered.
"""

import argparse
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parent.parent
MIGRATIONS = Path("internal/store/migrations")
SEQUENCE = Path(".oberth/migration-sequence.txt")
ID = re.compile(r"(?P<stamp>[0-9]{14})-[a-z][a-z0-9]*(?:-[a-z0-9]+)*")


def read_sequence(text):
    return [line.strip() for line in text.splitlines() if line.strip() and not line.startswith("#")]


def check(files, sequence, base=None):
    problems = []
    seen = set()
    stamps = {}
    for entry in sequence:
        match = ID.fullmatch(entry)
        if match is None:
            problems.append(f"{entry}: not a timestamped migration ID (create one with hack/new-migration.py)")
            continue
        if entry in seen:
            problems.append(f"{entry}: listed twice")
        seen.add(entry)
        stamp = match.group("stamp")
        if stamp in stamps and stamps[stamp] != entry:
            problems.append(f"{entry}: shares timestamp {stamp} with {stamps[stamp]}; regenerate one with hack/new-migration.py")
        stamps.setdefault(stamp, entry)
    for previous, entry in zip(sequence, sequence[1:]):
        if entry <= previous:
            problems.append(f"{entry}: sorts before or with {previous}, the latest before it; a new migration must be numbered after main's latest: regenerate it with hack/new-migration.py and move its SQL")
    for name in sorted(set(files) - seen):
        problems.append(f"{name}: migration file is not listed in {SEQUENCE}")
    for name in sorted(seen - set(files)):
        problems.append(f"{name}: listed in {SEQUENCE} but has no migration file")
    if base is not None:
        if sequence[: len(base)] != base:
            problems.append(f"{SEQUENCE}: the base list is not an unchanged prefix; shipped migrations are never edited, removed or reordered")
        elif base and any(entry <= base[-1] for entry in sequence[len(base):]):
            problems.append(f"a new migration sorts before main's latest {base[-1]}")
    return problems


def migration_files(root):
    return [path.stem for path in sorted((root / MIGRATIONS).glob("*.sql"))]


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--root", type=Path, default=ROOT)
    parser.add_argument("--base", help="git ref whose list must be a prefix, e.g. origin/main")
    args = parser.parse_args()
    sequence = read_sequence((args.root / SEQUENCE).read_text(encoding="utf-8"))
    base = None
    if args.base:
        shown = subprocess.run(["git", "-C", str(args.root), "show", f"{args.base}:{SEQUENCE.as_posix()}"],
                               check=False, capture_output=True, text=True)
        if shown.returncode != 0:
            print(f"cannot read {SEQUENCE} at {args.base}: {shown.stderr.strip()}", file=sys.stderr)
            return 2
        base = read_sequence(shown.stdout)
    problems = check(migration_files(args.root), sequence, base)
    for problem in problems:
        print(problem, file=sys.stderr)
    if problems:
        return 1
    print(f"migration sequence: {len(sequence)} IDs, latest {sequence[-1] if sequence else 'none'}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
