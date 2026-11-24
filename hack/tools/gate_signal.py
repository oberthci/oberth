"""Exercise launcher cancellation during a build and a tool invocation."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time


BUILDER = '''#!/usr/bin/env python3
import json, os, signal, subprocess, sys, time
from pathlib import Path
kind = "child" if sys.argv[1:] == ["child"] else "parent"
if kind == "child": signal.signal(signal.SIGTERM, signal.SIG_IGN)
Path(os.environ["SIGNAL_READY"], kind).write_text(json.dumps({"pid": os.getpid(), "group": os.getpgrp(), "directory": str(Path.cwd())}))
if kind == "parent": subprocess.Popen([sys.executable, __file__, "child"])
while True: time.sleep(60)
'''

PROGRAM = '''package main
import ("encoding/json"; "os"; "os/exec"; "os/signal"; "path/filepath"; "syscall"; "time")
func main() {
 kind := "parent"
 if len(os.Args) > 1 { kind = "child"; signal.Ignore(syscall.SIGTERM) }
 row, _ := json.Marshal(map[string]any{"pid":os.Getpid(), "group":syscall.Getpgrp(), "directory":filepath.Dir(os.Args[0])})
 if err := os.WriteFile(filepath.Join(os.Getenv("SIGNAL_READY"), kind), row, 0600); err != nil { panic(err) }
 if kind == "parent" { if err := exec.Command(os.Args[0], "child").Start(); err != nil { panic(err) } }
 for { time.Sleep(time.Hour) }
}
'''


def running(pid):
    try:
        return (Path("/proc") / str(pid) / "stat").read_text().rsplit(")", 1)[1].split()[0] != "Z"
    except FileNotFoundError:
        try:
            os.kill(pid, 0)
        except ProcessLookupError:
            return False
        return True


class SignalGateTests:
    def check_signal_cleanup(self, during_build, received=signal.SIGTERM):
        ready = self.fixture / "signal-ready"
        ready.mkdir()
        scratch = self.fixture / "tool-scratch"
        scratch.mkdir()
        if during_build:
            compiler = self.bin_path / "go"
            compiler.write_text(BUILDER)
            compiler.chmod(0o755)
        else:
            module = self.fixture / "controlled-linter"
            package = module / "cmd/golangci-lint"
            package.mkdir(parents=True)
            (module / "go.mod").write_text("module github.com/golangci/golangci-lint/v2\n\ngo 1.26.0\n")
            (package / "main.go").write_text(PROGRAM)
            (self.fixture / "hack/tools/go.mod").write_text("module fixture.tools\n\ngo 1.26.0\n"
                "require github.com/golangci/golangci-lint/v2 v2.12.2\n"
                "replace github.com/golangci/golangci-lint/v2 => " + str(module) + "\n")
            (self.fixture / "hack/tools/go.sum").write_text("")
        records = {}
        with (self.fixture / "launcher.log").open("w") as log:
            process = subprocess.Popen(["bash", "hack/lint-tool.sh"], cwd=self.fixture,
                env={**os.environ, "PATH": str(self.bin_path) + ":" + os.environ["PATH"],
                     "GOTMPDIR": str(scratch), "SIGNAL_READY": str(ready)},
                stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
            try:
                deadline = time.monotonic() + 180
                while len(records) != 2:
                    for kind in ("parent", "child"):
                        try:
                            records[kind] = json.loads((ready / kind).read_text())
                        except (FileNotFoundError, json.JSONDecodeError):
                            pass
                    if process.poll() is not None or time.monotonic() > deadline:
                        self.fail("tool did not become ready: " + (self.fixture / "launcher.log").read_text())
                    time.sleep(0.01)
                self.assertTrue(Path(records["parent"]["directory"]).is_relative_to(scratch))
                process.send_signal(received)
                process.wait(timeout=15)
                alive = [row["pid"] for row in records.values() if running(row["pid"])]
                deadline = time.monotonic() + 5
                while alive and time.monotonic() < deadline:
                    time.sleep(0.01)
                    alive = [pid for pid in alive if running(pid)]
                self.assertEqual([], alive, "launcher left running children")
                self.assertEqual([], list(scratch.glob("oberth-tool-*")), "launcher left private build files")
                self.assertEqual(128 + received, process.returncode)
            finally:
                for group in {process.pid, *(row["group"] for row in records.values())}:
                    try:
                        os.killpg(group, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                process.wait(timeout=5)

    def test_sigterm_during_tool_build_cleans_children_and_directory(self):
        self.check_signal_cleanup(during_build=True)

    def test_sigterm_during_tool_run_cleans_children_and_binary(self):
        self.check_signal_cleanup(during_build=False)

    def test_sigint_during_tool_run_cleans_children_and_binary(self):
        self.check_signal_cleanup(during_build=False, received=signal.SIGINT)
