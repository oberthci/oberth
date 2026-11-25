#!/usr/bin/env python3
"""Verify the repository's pinned archive and stage an exclusive raw guest disk."""
import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import tempfile
import urllib.parse
import urllib.request


PIN = Path(__file__).resolve().parents[2] / ".oberth/pins/guest-disk.json"


def copy_bounded(source, destination, limit):
    total = 0
    while chunk := source.read(1024 * 1024):
        total += len(chunk)
        if total > limit:
            raise ValueError("Guest disk exceeds its reviewed size bound.")
        destination.write(chunk)


def checksums(path):
    sha256, sha512 = hashlib.sha256(), hashlib.sha512()
    with path.open("rb") as stream:
        while chunk := stream.read(1024 * 1024):
            sha256.update(chunk)
            sha512.update(chunk)
    return sha256.hexdigest(), sha512.hexdigest()


def prepare(pin, output, archive=None):
    if pin["format"] != "raw.gz" or pin["architecture"] != "amd64":
        raise ValueError("Guest disk format or architecture differs.")
    if urllib.parse.urlsplit(pin["url"]).scheme != "https":
        raise ValueError("Guest disk source must use HTTPS.")
    if output.exists() or output.is_symlink():
        raise ValueError("VM output exists; choose a new disk path.")
    with tempfile.TemporaryDirectory(prefix=".guest-disk-", dir=output.parent) as temporary:
        stage = Path(temporary)
        compressed = stage / "archive.gz"
        source = archive.open("rb") if archive else urllib.request.urlopen(pin["url"], timeout=60)
        with source, compressed.open("xb") as target:
            if not archive and urllib.parse.urlsplit(source.geturl()).scheme != "https":
                raise ValueError("Guest disk redirect must use HTTPS.")
            copy_bounded(source, target, pin["max_download_bytes"])
        if checksums(compressed) != (pin["sha256"], pin["sha512"]):
            raise ValueError("Guest disk archive checksum differs.")
        disk = stage / "disk.img"
        with gzip.open(compressed, "rb") as source, disk.open("xb") as target:
            copy_bounded(source, target, pin["max_disk_bytes"])
        disk.chmod(0o660)
        receipt = {"source": pin["url"], "archive_sha256": pin["sha256"],
                   "disk_sha256": checksums(disk)[0], "disk_bytes": disk.stat().st_size}
        # Linking publishes verified bytes atomically and refuses a concurrent writer.
        os.link(disk, output)
    return receipt


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True, type=Path)
    parser.add_argument("--archive", type=Path, help="Verify a local copy of the same pinned archive")
    args = parser.parse_args()
    print(json.dumps(prepare(json.loads(PIN.read_text()), args.output, args.archive), sort_keys=True))


if __name__ == "__main__":
    main()
