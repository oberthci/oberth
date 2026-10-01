#!/usr/bin/env python3
"""Exercise the signed release's prefetch refusal without any credentials.

The source-pinned published binary is the runtime boundary under test. This
credentialless regression emits only checked numeric mount metadata so native
named-step logs remain useful after the Pod exits (#619).
"""
import hashlib
import os
from pathlib import Path
import stat
import subprocess
import tempfile

VERSION = "v0.16.11"
BINARY_SHA256 = "f46933d5c8743a253c0aa77d1f11ac36de589150cca7241b0d8019d3f3e32b74"


def main():
    with tempfile.TemporaryDirectory(prefix="oberth-diagnostic-") as temporary:
        work = Path(temporary)
        binary = work / "oberth"
        subprocess.run([
            "curl", "--fail", "--silent", "--show-error", "--location",
            "--connect-timeout", "10", "--max-time", "90", "--retry", "2",
            "--proto", "=https", "--proto-redir", "=https", "--tlsv1.3",
            "--output", str(binary),
            "https://releases.oberth.ci/oberth/" + VERSION + "/oberth-linux-amd64",
        ], check=True)
        if hashlib.sha256(binary.read_bytes()).hexdigest() != BINARY_SHA256:
            raise ValueError("published diagnostic binary differs from reviewed signed release")
        binary.chmod(0o700)
        root = work / "private-root"
        root.mkdir(mode=0o700)
        filesystem = int(subprocess.check_output(["stat", "-f", "-c", "%t", str(root)], text=True).strip(), 16)
        if filesystem == 0x01021994:
            raise ValueError("negative fixture requires a non-tmpfs directory")
        sentinel = root / "private-entry-sentinel"
        sentinel.write_text("synthetic-content-sentinel\n")
        before = root.stat()
        child_marker = work / "child-must-not-run"
        result = subprocess.run([
            str(binary), "secretstore", "exec", "--dir=" + str(root),
            "--path=oberth/data/unused-diagnostic-sentinel", "--",
            "/usr/bin/touch", str(child_marker),
        ], env={"PATH": "/usr/bin:/bin", "UNUSED_SENTINEL": "synthetic-environment-sentinel"},
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, check=False, timeout=10)
        mode = "0" + format(stat.S_IMODE(before.st_mode), "o")
        metadata = "uid={} gid={} euid={} mode={} fstype={:#x}".format(
            before.st_uid, before.st_gid, os.geteuid(), mode, filesystem)
        expected = "oberth: prepare private secret directory: secret directory is not verified tmpfs (" + metadata + ")\n"
        if result.returncode != 1 or result.stdout or result.stderr != expected:
            # Never echo unexpected output: it may violate the safety contract.
            raise ValueError("released prefetch refusal differs from exact numeric-only diagnostic")
        after = root.stat()
        identity = lambda info: (info.st_dev, info.st_ino, info.st_uid, info.st_gid, info.st_mode, info.st_nlink)
        if child_marker.exists() or list(root.iterdir()) != [sentinel] or sentinel.read_text() != "synthetic-content-sentinel\n" or identity(after) != identity(before):
            raise ValueError("prefetch refusal changed the fixture or executed its child")
        print(expected.strip())
        print("PASS: signed release refused before credential setup/child execution; no paths, entries or environment in diagnostic")


if __name__ == "__main__":
    main()
