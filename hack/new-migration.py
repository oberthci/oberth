#!/usr/bin/env python3
"""Create a timestamped SQL migration without allocating a shared number."""

import argparse
from datetime import datetime, timezone
from pathlib import Path
import re
import secrets

SLUG = re.compile(r"[a-z][a-z0-9]*(?:-[a-z0-9]+)*")


def append_sequence(sequence, migration):
    """Append the new ID to .oberth/migration-sequence.txt (#1524)."""
    sequence = Path(sequence)
    with sequence.open("a", encoding="utf-8", newline="\n") as output:
        output.write(migration.stem + "\n")


def create_migration(directory, slug):
    if len(slug) > 40 or SLUG.fullmatch(slug) is None:
        raise ValueError("slug must be 1..40 lowercase letters/digits separated by hyphens, starting with a letter")
    directory = Path(directory)
    directory.mkdir(parents=True, exist_ok=True)
    # The suffix separates independent branches even with identical names and
    # clocks. Exclusive creation also prevents overwriting a local collision.
    while True:
        stamp = datetime.now(timezone.utc).strftime("%Y%m%d%H%M%S")
        path = directory / f"{stamp}-{slug}-{secrets.token_hex(16)}.sql"
        try:
            with path.open("x", encoding="utf-8", newline="\n") as output:
                output.write(
                    f"-- Purpose: {slug.replace('-', ' ')}.\n"
                    "-- Rollback: Restore the pre-upgrade database backup.\n"
                    "-- Replace this comment with forward-only SQL and add an upgrade test.\n"
                )
            return path
        except FileExistsError:
            continue


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("slug", help="short description, e.g. add-run-index")
    args = parser.parse_args()
    root = Path(__file__).resolve().parent.parent
    try:
        path = create_migration(root / "internal/store/migrations", args.slug)
    except ValueError as error:
        parser.error(str(error))
    append_sequence(root / ".oberth/migration-sequence.txt", path)
    print(path)


if __name__ == "__main__":
    main()
