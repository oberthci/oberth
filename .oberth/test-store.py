#!/usr/bin/python3
"""Run the complete store race suite in two bounded, private test processes."""

import contextlib
import json
import math
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import tempfile
import time

PACKAGE = "github.com/oberthci/oberth/internal/store"
DEADLINE_SECONDS = 66 * 60
STOP_GRACE_SECONDS = 3



# Race instrumentation of the pure-Go SQLite engine and its libc made these
# CPU-bound suites ~30x slower (RCA-STORE-TEST-TIME, 2026-11-10). Oberth's own
# packages stay fully race-instrumented; races inside third-party SQLite are
# not reported.
RACE_SCOPE = ["-gcflags=modernc.org/sqlite/...=-race=false", "-gcflags=modernc.org/libc/...=-race=false"]

def discover(output):
    names = output.splitlines()
    if not names or any(not re.fullmatch(r"(?:Test|Example|Fuzz|Benchmark)\w*", name)
                        for name in names) or len(names) != len(set(names)):
        raise ValueError("invalid or duplicate store test discovery")
    names = [name for name in names if not name.startswith("Benchmark")]
    if not names:
        raise ValueError("store test discovery is empty")
    return names


def validate_partition(names, groups):
    flattened = [name for group in groups for name in group]
    if (len(groups) != 2 or not all(groups) or len(names) != len(set(names))
            or len(flattened) != len(set(flattened)) or sorted(flattened) != sorted(names)):
        raise ValueError("store partition omits or duplicates discovered targets")


def partition(names, weights):
    stale = set(weights) - set(names)
    if stale:
        raise ValueError("store timing weights name nonexistent targets: " + ", ".join(sorted(stale)))
    if any(not isinstance(value, (int, float)) or isinstance(value, bool)
           or not math.isfinite(value) or value < 0 for value in weights.values()):
        raise ValueError("invalid store timing weight")
    groups, totals = [[], []], [0.0, 0.0]
    # Same deterministic LPT partition as the measured R2 experiment. New
    # targets are always included, with one second default scheduling weight.
    for name in sorted(names, key=lambda name: (-weights.get(name, 0), name)):
        worker = min(range(2), key=lambda index: (totals[index], index))
        groups[worker].append(name)
        totals[worker] += weights.get(name, 1)
    validate_partition(names, groups)
    return groups


class Children:
    """Own child process groups, including build descendants, until reaped."""

    def __init__(self, deadline):
        self.deadline = deadline
        self.processes = []
        self.interrupted = 0

    def interrupt(self, signum, _frame):
        # Do not raise between Popen and ownership registration.
        self.interrupted = signum

    def start(self, command, cwd, env, stdout=None):
        process = subprocess.Popen(command, cwd=cwd, env=env, stdout=stdout,
                                   start_new_session=True)
        self.processes.append(process)
        return process

    def wait(self, processes):
        while any(process.poll() is None for process in processes):
            if self.interrupted:
                raise InterruptedError(self.interrupted)
            if time.monotonic() >= self.deadline:
                raise TimeoutError("store runner reached its 66 minute deadline")
            time.sleep(0.05)
        if self.interrupted:
            raise InterruptedError(self.interrupted)
        return [process.returncode for process in processes]

    def stop(self):
        # Signal every owned group, even if its leader already exited: a failed
        # worker/build can leave descendants. No unrelated process is targeted.
        for sig in (signal.SIGTERM, signal.SIGKILL):
            for process in self.processes:
                try:
                    os.killpg(process.pid, sig)
                except ProcessLookupError:
                    pass
            if sig == signal.SIGTERM:
                end = time.monotonic() + STOP_GRACE_SECONDS
                while any(process.poll() is None for process in self.processes) and time.monotonic() < end:
                    time.sleep(0.05)
        for process in self.processes:
            process.wait()
        self.processes.clear()


def main():
    root = Path(__file__).resolve().parent.parent
    weights = json.loads(Path(__file__).with_name("store-test-weights.json").read_text())["seconds"]
    env = dict(os.environ, GOMAXPROCS="2", GOMEMLIMIT="2GiB")
    children = Children(time.monotonic() + DEADLINE_SECONDS)
    previous = {sig: signal.signal(sig, children.interrupt) for sig in (signal.SIGINT, signal.SIGTERM)}
    try:
        with contextlib.ExitStack() as cleanup:
            scratch = Path(cleanup.enter_context(tempfile.TemporaryDirectory(prefix="oberth-store-build-")))
            # Cleanup children before their binary/private fixtures disappear.
            cleanup.callback(children.stop)
            binary = scratch / "store-race.test"
            build = children.start(["go", "test", "-race", "-c", "-p=2", *RACE_SCOPE, "-o", str(binary), PACKAGE], root, env)
            if children.wait([build]) != [0]:
                return 1
            package_dir = root / "internal/store"
            listing = scratch / "targets.txt"
            with listing.open("w") as output:
                process = children.start([str(binary), "-test.list=."], package_dir, env, stdout=output)
                if children.wait([process]) != [0]:
                    return 1
            if listing.stat().st_size > 2 << 20:
                raise ValueError("store test discovery exceeds bound")
            names = discover(listing.read_text())
            groups = partition(names, weights)
            fixture_root = None
            if env.get("OBERTH_TEST_STORE_TMPDIR"):
                fixture_root = Path(cleanup.enter_context(tempfile.TemporaryDirectory(
                    prefix="store-workers-", dir=env["OBERTH_TEST_STORE_TMPDIR"])))
                # Registered after fixture cleanup, hence runs before it.
                cleanup.callback(children.stop)
            workers = []
            for index, group in enumerate(groups):
                worker_env = dict(env, GOMAXPROCS="1", GOMEMLIMIT="1GiB")
                if fixture_root is not None:
                    private = fixture_root / str(index)
                    private.mkdir()
                    worker_env["OBERTH_TEST_STORE_TMPDIR"] = str(private)
                pattern = "^(" + "|".join(re.escape(name) for name in sorted(group)) + ")$"
                command = [str(binary), "-test.v", "-test.count=1", "-test.parallel=1",
                           "-test.timeout=66m", "-test.run=" + pattern]
                print(f"store worker {index + 1}/2: {len(group)} targets; GOMAXPROCS=1 GOMEMLIMIT=1GiB", flush=True)
                # Inherit both streams: output flows as tests execute, without
                # a pipe reader or end-of-worker buffering. No shell selectors.
                workers.append(children.start(command, package_dir, worker_env))
            codes = children.wait(workers)
            print(f"store workers finished: exit codes {codes}; {len(names)} discovered targets", flush=True)
            return 0 if codes == [0, 0] else 1
    except InterruptedError as error:
        return 128 + error.args[0]
    except (OSError, ValueError, TimeoutError) as error:
        print(f"store runner: {error}", file=sys.stderr, flush=True)
        return 1
    finally:
        children.stop()
        for sig, handler in previous.items():
            signal.signal(sig, handler)


if __name__ == "__main__":
    raise SystemExit(main())
