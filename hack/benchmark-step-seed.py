#!/usr/bin/env python3
"""Measure the binary-only SourceSeeder stream + SHA-256 readback over Pod exec.

Use a disposable seed Pod with a writable PVC at /seed and sh/cat/chmod/sha256sum.
No Pod creation, RBAC changes, or source/credential access is performed here.
Each sample overwrites /seed/oberth-bin/oberth. Run on the server's network for
representative transport latency; kubectl startup is included in both controls.
"""
import argparse
import hashlib
import json
import subprocess
import threading
import time
from pathlib import Path


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("binary", type=Path)
    parser.add_argument("namespace")
    parser.add_argument("pod")
    parser.add_argument("--container", default="seed")
    parser.add_argument("--samples", type=int, default=10)
    parser.add_argument("--kubectl", default="kubectl")
    args = parser.parse_args()
    if not 1 <= args.samples <= 100:
        parser.error("samples must be 1..100")
    base = [args.kubectl, "-n", args.namespace, "exec", "-c", args.container]

    def execute(command):
        return subprocess.run(base + [args.pod, "--"] + command, check=True,
                              capture_output=True, text=True, timeout=60).stdout.strip()

    print(json.dumps({"binary_bytes": args.binary.stat().st_size,
                      "namespace": args.namespace, "pod": args.pod,
                      "method": "two Pod execs: stream+SHA256, then SHA256 readback"}), flush=True)
    for sample in range(args.samples):
        # Each real run seeds a new file; remove the prior read-only sample.
        execute(["/bin/sh", "-c", "rm -f /seed/oberth-bin/oberth"])
        start = time.perf_counter()
        execute(["/bin/sh", "-c", "true"])
        execute(["/bin/sh", "-c", "true"])
        control = time.perf_counter() - start
        digest = hashlib.sha256()
        start = time.perf_counter()
        with subprocess.Popen(base + ["-i", args.pod, "--", "/bin/sh", "-c",
                "mkdir -p /seed/oberth-bin && cat > /seed/oberth-bin/oberth && chmod 0555 /seed/oberth-bin/oberth"],
                stdin=subprocess.PIPE, stdout=subprocess.DEVNULL) as child:
            timer = threading.Timer(60, child.kill)
            timer.start()
            try:
                with args.binary.open("rb") as source:
                    while chunk := source.read(32768):
                        digest.update(chunk)
                        child.stdin.write(chunk)
                child.stdin.close()
                if child.wait() != 0:
                    raise RuntimeError("binary stream failed")
            finally:
                timer.cancel()
        streamed = time.perf_counter() - start
        actual = execute(["/bin/sh", "-c", "sha256sum /seed/oberth-bin/oberth | cut -d' ' -f1"])
        total = time.perf_counter() - start
        if actual != digest.hexdigest():
            raise RuntimeError("binary SHA256 readback mismatch")
        print(json.dumps({"sample": sample, "control_two_exec_s": control,
                          "stream_s": streamed, "verify_s": total - streamed,
                          "total_s": total, "increment_over_control_s": total - control,
                          "sha256": actual}), flush=True)


if __name__ == "__main__":
    main()
