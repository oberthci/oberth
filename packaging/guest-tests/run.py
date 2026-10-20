#!/usr/bin/env python3
"""Run repository tests in a networkless KVM guest with no host filesystem share.

This is ordinary CI test output, not an independently attested trusted receipt.
Only files copied into the initramfs enter the guest. No mount or host privilege
is required to construct it. The caller supplies reviewed kernel/QEMU artifacts.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import stat
import subprocess
import tempfile


def record(out, ino, name, mode, data=b"", major=0, minor=0):
    encoded = name.encode() + b"\0"
    fields = [ino, mode, 0, 0, 1, 0, len(data), 0, 0, major, minor, len(encoded), 0]
    out.extend(b"070701" + b"".join(f"{n:08x}".encode() for n in fields))
    out.extend(encoded)
    out.extend(b"\0" * (-len(out) % 4))
    out.extend(data)
    out.extend(b"\0" * (-len(out) % 4))


def archive(files):
    entries = {name: (stat.S_IFDIR | 0o755, b"") for name in ("dev", "proc", "sys", "tmp", "bin")}
    for name, (mode, data) in files.items():
        path = Path(name)
        if path.is_absolute() or ".." in path.parts or name in ("", ".", "TRAILER!!!"):
            raise ValueError("noncanonical guest path")
        for parent in path.parents:
            if str(parent) != ".":
                entries.setdefault(str(parent), (stat.S_IFDIR | 0o755, b""))
        entries[name] = (stat.S_IFREG | mode, data)
    out = bytearray()
    for ino, (name, (mode, data)) in enumerate(sorted(entries.items()), 1):
        record(out, ino, name, mode, data)
    record(out, len(entries) + 1, "dev/console", stat.S_IFCHR | 0o600, major=5, minor=1)
    record(out, 0, "TRAILER!!!", 0)
    out.extend(b"\0" * (-len(out) % 512))
    return out


def add_file(files, name, source, executable=False):
    path = Path(source)
    if not path.is_file() or not 0 < path.stat().st_size <= 128 << 20:
        raise ValueError(f"invalid guest input: {source}")
    data = path.read_bytes()
    files[name] = (0o755 if executable else 0o644, data)
    print(f"guest input {name} sha256:{hashlib.sha256(data).hexdigest()}", flush=True)


def add_tool(files, source):
    # These are the digest-pinned runner image's tools, not candidate binaries.
    add_file(files, "bin/" + Path(source).name, source, True)
    result = subprocess.run(["/usr/bin/ldd", source], text=True, capture_output=True, check=True)
    for library in re.findall(r"(/[^\s()]+)", result.stdout):
        add_file(files, library.lstrip("/"), library, True)


def qemu_command(args, initramfs):
    return [args.qemu, "-no-user-config", "-nodefaults", "-display", "none",
            "-monitor", "none", "-serial", "stdio", "-nic", "none",
            "-accel", "kvm", "-cpu", "host", "-smp", "2", "-m", "2048",
            "-no-reboot", "-L", args.firmware,
            "-sandbox", "on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny",
            "-kernel", args.kernel, "-initrd", str(initramfs),
            "-append", "console=ttyS0 panic=-1 rdinit=/init lsm=lockdown,capability,yama,bpf"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("kernel", "qemu", "firmware", "init", "test", "pattern"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--env", action="append", default=[])
    parser.add_argument("--profile", choices=("bpf", "fuse"), default="bpf")
    parser.add_argument("--object")
    parser.add_argument("--fixture")
    parser.add_argument("--timeout", type=int, default=600)
    args = parser.parse_args()
    def interrupted(signum, _frame):
        raise SystemExit(128 + signum)
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    if os.environ.get("OBERTH_GUEST_TEST") != "kvm-v1" or os.environ.get("OBERTH_KVM") != "1":
        raise SystemExit("admin-granted Oberth guest-test KVM admission required")
    if not os.access("/dev/kvm", os.R_OK | os.W_OK):
        raise SystemExit("KVM device unavailable; no emulation fallback")
    files = {}
    add_file(files, "init", args.init, True)
    add_file(files, "tests", args.test, True)
    for tool in ("/bin/sh", "/bin/sleep", "/bin/cp"):
        add_tool(files, tool)
    if args.object:
        add_file(files, "bpf.o", args.object)
    if args.fixture:
        add_file(files, "runtime-bootstrap", args.fixture, True)
    config = {"Profile": args.profile, "Args": ["-test.v", "-test.count=1", "-test.timeout=8m", "-test.run=" + args.pattern], "Env": args.env}
    files["test-config.json"] = (0o644, json.dumps(config).encode())
    print("guest kernel sha256:" + hashlib.sha256(Path(args.kernel).read_bytes()).hexdigest(), flush=True)
    with tempfile.TemporaryDirectory(prefix="oberth-guest-") as directory:
        initramfs = Path(directory) / "initramfs"
        initramfs.write_bytes(archive(files))
        with (Path(directory) / "console").open("w+") as console:
            process = subprocess.Popen(qemu_command(args, initramfs), stdin=subprocess.DEVNULL,
                                       stdout=console, stderr=subprocess.STDOUT)
            try:
                result = process.wait(timeout=args.timeout)
            finally:
                if process.poll() is None:
                    # Stay in the caller's process group so load guards pause
                    # the VM too. QEMU's sandbox forbids child processes.
                    process.kill()
                    process.wait()
                console.seek(0)
                output = console.read()
                print(output, end="", flush=True)
            lines = output.splitlines()
            if result != 0 or lines.count("OBERTH_GUEST_TEST_PASS") != 1 or any("OBERTH_GUEST_TEST_FAIL" in line for line in lines):
                raise SystemExit("guest tests failed or returned no complete result")


if __name__ == "__main__":
    main()
