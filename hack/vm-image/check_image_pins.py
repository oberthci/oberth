#!/usr/bin/env python3
"""Resolve every exact image pin anonymously. Network check; no VM or daemon."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile


PIN_FILE = Path(__file__).resolve().parents[2] / ".oberth/pins/walk-tools.json"


def check_image(reference, crane, env):
    if not re.fullmatch(r"[^\s@]+:[^\s/@]+@sha256:[0-9a-f]{64}", reference):
        raise ValueError("Image must have a fixed SHA-256 digest.")
    result = subprocess.run([crane, "manifest", reference], env=env, capture_output=True, timeout=90)
    if result.returncode:
        raise ValueError(result.stderr.decode(errors="replace").strip())
    digest = "sha256:" + hashlib.sha256(result.stdout).hexdigest()
    if digest != reference.rsplit("@", 1)[1]:
        raise ValueError("Fetched image manifest checksum differs.")
    return {"digest": digest, "bytes": len(result.stdout), "manifest": json.loads(result.stdout)}


def check(pins, crane):
    results = []
    # Do not consult the operator's credentials or configured credential helpers.
    with tempfile.TemporaryDirectory(prefix="walk-public-pins-") as anonymous:
        env = {"PATH": os.environ["PATH"], "HOME": anonymous, "DOCKER_CONFIG": anonymous}
        for name, reference in pins["images"].items():
            result = {"name": name, "reference": reference}
            try:
                result.update(check_image(reference, crane, env))
            except (ValueError, OSError, subprocess.TimeoutExpired) as error:
                result["error"] = str(error)
            results.append(result)
    return results


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--pins", type=Path, default=PIN_FILE)
    parser.add_argument("--crane", default="crane", help="Path to a local crane executable")
    args = parser.parse_args()
    results = check(json.loads(args.pins.read_text()), args.crane)
    print(json.dumps(results, indent=2))
    raise SystemExit(int(any("error" in result for result in results)))
