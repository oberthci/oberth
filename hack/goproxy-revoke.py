#!/usr/bin/env python3
"""Local PVC maintenance: deny one immutable private module version.

Run as the data-volume owner. No network, database, or Git access is used.
A marker denies new responses even with Git down and survives cache restarts.
"""
import argparse
import hashlib
import os
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cache-root", required=True)
    parser.add_argument("--repository", required=True, help="exact mapped Git input")
    parser.add_argument("--module", required=True, help="full module path")
    parser.add_argument("--version", required=True, help="canonical tagged version")
    args = parser.parse_args()
    for value in (args.repository, args.module, args.version):
        if not value or "\0" in value:
            parser.error("coordinates must be nonempty and contain no NUL")
    root = Path(args.cache_root)
    if not (root / "versions").is_dir():
        parser.error("cache root must be an initialized proxy cache")
    key = hashlib.sha256(
        ("module-artifact-v1\0" + args.repository + "\0" + args.module + "\0" + args.version).encode()
    ).hexdigest()
    revoked = root / "revoked"
    revoked.mkdir(mode=0o700, exist_ok=True)
    marker = revoked / key
    # Existence is the deny bit. O_EXCL never replaces another operator's marker.
    try:
        fd = os.open(marker, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    except FileExistsError:
        fd = os.open(marker, os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)
    for directory in (revoked, root):
        fd = os.open(directory, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(fd)
        finally:
            os.close(fd)
    print(f"Revoked {args.module}@{args.version}; persistent marker: {marker}")


if __name__ == "__main__":
    main()
