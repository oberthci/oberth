"""Observe the documented CLI follow while an independent Pod check says running."""
import os
import selectors
import signal
import subprocess
from walkcommands import render


def follow(s, identifier, running):
    shown = render(s, "log-follow", {"id": identifier})
    started = s.runtime.clock()
    s.sequence += 1
    stem = s.private / f"command-{s.sequence:04}"
    marker = ("walk: live " + s.data.sha + "\n").encode()
    found, elapsed, tail = False, None, b""
    with stem.with_suffix(".stdout").open("wb") as output, stem.with_suffix(".stderr").open("wb") as errors:
        process = subprocess.Popen([str(s.binary), *shown[1:]], stdout=subprocess.PIPE, stderr=errors,
                                   env={**s.env, **s.profile(False)}, start_new_session=True)
        try:
            with selectors.DefaultSelector() as selector:
                selector.register(process.stdout, selectors.EVENT_READ)
                deadline = started + s.args.run_timeout
                while s.runtime.clock() < deadline:
                    if not selector.select(max(0, deadline - s.runtime.clock())):
                        break
                    chunk = os.read(process.stdout.fileno(), 65536)
                    if not chunk:
                        break
                    output.write(chunk)
                    tail += chunk
                    if marker in tail:
                        elapsed = s.runtime.clock() - started
                        found = running()
                        break
                    tail = tail[-len(marker):]
        finally:
            stop(process)
            process.stdout.close()
    command = {"step": s.current, "argv": shown, "exit_code": process.returncode,
               "operation": "log-follow", "capture": stem.name, "stopped_after_observation": found,
               "duration_seconds": round(elapsed if elapsed is not None else s.runtime.clock() - started, 3)}
    s.record(command)
    return found, command


def stop(process):
    if process.poll() is None:
        os.killpg(process.pid, signal.SIGTERM)
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            os.killpg(process.pid, signal.SIGKILL)
            process.wait()
    else:
        process.wait()
