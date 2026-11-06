#!/usr/bin/python3
"""Controls for the store runner; synthetic workers do not benchmark the suite."""
import importlib.util
import json
import os
from pathlib import Path
import selectors
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import unittest

SPEC = importlib.util.spec_from_file_location("store_runner", Path(__file__).with_name("test-store.py"))
RUNNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(RUNNER)

WORKER = r'''
import json, os, pathlib, re, signal, subprocess, sys, time
records = pathlib.Path(os.environ["RECORDS"])
if sys.argv[1:] == ["-test.list=."]:
    print("TestAlpha\nTestBeta\nExampleNew\nFuzzFuture\nBenchmarkExcluded")
    raise SystemExit(0)
pattern = next(a.split("=", 1)[1] for a in sys.argv if a.startswith("-test.run="))
names = [n for n in ["TestAlpha", "TestBeta", "ExampleNew", "FuzzFuture"] if re.fullmatch(pattern, n)]
record = {"names": names, "argv": sys.argv[1:], "cwd": os.getcwd(),
          "env": {k:os.environ.get(k) for k in ["GOMAXPROCS", "GOMEMLIMIT", "OBERTH_TEST_TIMEOUT_SCALE", "OBERTH_TEST_STORE_TMPDIR"]}}
(records / (str(os.getpid()) + ".json")).write_text(json.dumps(record))
print("worker stdout live", flush=True)
print("worker stderr live", file=sys.stderr, flush=True)
if os.environ.get("BLOCK"):
    child = subprocess.Popen([sys.executable, "-c", "import pathlib,signal,time; signal.signal(signal.SIGTERM,lambda *a:exit(0)); pathlib.Path(" + repr(str(records / (str(os.getpid()) + ".child-ready"))) + ").write_text('ready'); time.sleep(60)"])
    (records / (str(os.getpid()) + ".child-pid")).write_text(str(child.pid))
    def stop(*args):
        child.wait(timeout=2)
        raise SystemExit(0)
    signal.signal(signal.SIGTERM, stop)
    time.sleep(60)
if os.environ.get("FAIL_WORKER") and "TestAlpha" in names:
    raise SystemExit(7)
'''


class StoreRunner(unittest.TestCase):
    def test_discovery_and_partition_include_new_examples_and_fuzz(self):
        names = RUNNER.discover("TestOld\nExampleFuture\nFuzzNew\nTestOther\nExample\nTest\nFuzz\nBenchmark\nBenchmarkOnly\n")
        groups = RUNNER.partition(names, {"TestOld": 100, "RemovedTest": 1000})
        self.assertEqual(sorted(sum(groups, [])), sorted(names))
        self.assertEqual(groups, RUNNER.partition(list(reversed(names)), {"TestOld": 100, "RemovedTest": 1000}))
        self.assertNotIn("BenchmarkOnly", sum(groups, []))
        for invalid in ["", "TestOld\nTestOld", "ok package", "TestA/subtest"]:
            with self.subTest(invalid=invalid), self.assertRaises(ValueError):
                RUNNER.discover(invalid)
        for groups in [[["TestOld"], []], [["TestOld"], ["TestOld", "TestOther"]],
                       [["TestOld"], ["Unknown"]]]:
            with self.subTest(groups=groups), self.assertRaises(ValueError):
                RUNNER.validate_partition(["TestOld", "TestOther"], groups)
        with self.assertRaises(ValueError):
            RUNNER.partition(names, {"TestOld": float("nan")})

    def fixture(self, directory, **extra):
        root = Path(directory).resolve()
        scripts = root / ".oberth"
        scripts.mkdir()
        (root / "internal/store").mkdir(parents=True)
        for name in ["test-store.py", "test-packages.py"]:
            shutil.copyfile(Path(__file__).with_name(name), scripts / name)
        (scripts / "store-test-weights.json").write_text(json.dumps({"seconds": {"TestAlpha": 10, "TestBeta": 9}}))
        records, tmp, memory = root / "records", root / "tmp", root / "memory-volume"
        for path in (records, tmp, memory):
            path.mkdir()
        fake = root / "go"
        fake.write_text("#!" + sys.executable + "\n" + r'''
import json, os, pathlib, sys
if sys.argv[1:] == ["list", "./..."]:
    print("github.com/oberthci/oberth/internal/store\ngithub.com/oberthci/oberth/internal/service\ngithub.com/oberthci/oberth/internal/gitcache\ngithub.com/oberthci/oberth/pkg/other")
    raise SystemExit(0)
assert sys.argv[1:5] == ["test", "-race", "-c", "-p=2"], sys.argv
pathlib.Path(os.environ["RECORDS"], "build.json").write_text(json.dumps({"argv":sys.argv[1:],"cpu":os.environ["GOMAXPROCS"],"heap":os.environ["GOMEMLIMIT"]}))
output = pathlib.Path(sys.argv[sys.argv.index("-o") + 1])
output.write_text("#!" + sys.executable + "\n" + ''' + repr(WORKER) + ''')
output.chmod(0o700)
''')
        fake.chmod(0o700)
        env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ["PATH"],
                   RECORDS=str(records), TMPDIR=str(tmp), OBERTH_TEST_STORE_TMPDIR=str(memory),
                   OBERTH_TEST_TIMEOUT_SCALE="4", **extra)
        cmd = [sys.executable, "-I", "-S", "-B", str(scripts / "test-packages.py"), "store"]
        return root, cmd, env

    def test_launcher_builds_once_and_streams_complete_private_workers(self):
        with tempfile.TemporaryDirectory() as directory:
            root, cmd, env = self.fixture(directory)
            done = subprocess.run(cmd, cwd=root, env=env, text=True, capture_output=True, timeout=15)
            self.assertEqual(done.returncode, 0, done.stdout + done.stderr)
            self.assertIn("worker stdout live", done.stdout)
            self.assertIn("worker stderr live", done.stderr)
            build = json.loads((root / "records/build.json").read_text())
            self.assertEqual(build["cpu"], "2")
            self.assertEqual(build["heap"], "2GiB")
            self.assertEqual(build["argv"][-1], RUNNER.PACKAGE)
            workers = [json.loads(p.read_text()) for p in (root / "records").glob("*.json") if p.name != "build.json"]
            self.assertEqual(len(workers), 2)
            self.assertEqual(sorted(n for w in workers for n in w["names"]), ["ExampleNew", "FuzzFuture", "TestAlpha", "TestBeta"])
            self.assertEqual(len({w["env"]["OBERTH_TEST_STORE_TMPDIR"] for w in workers}), 2)
            for worker in workers:
                self.assertEqual(worker["cwd"], str(root / "internal/store"))
                self.assertEqual(worker["env"]["GOMAXPROCS"], "1")
                self.assertEqual(worker["env"]["GOMEMLIMIT"], "1GiB")
                self.assertEqual(worker["env"]["OBERTH_TEST_TIMEOUT_SCALE"], "4")
                for flag in ["-test.v", "-test.count=1", "-test.parallel=1", "-test.timeout=66m"]:
                    self.assertIn(flag, worker["argv"])
            self.assertEqual(list((root / "tmp").iterdir()), [])
            self.assertEqual(list((root / "memory-volume").iterdir()), [])

    def test_worker_failure_propagates_after_all_workers(self):
        with tempfile.TemporaryDirectory() as directory:
            root, cmd, env = self.fixture(directory, FAIL_WORKER="1")
            done = subprocess.run(cmd, cwd=root, env=env, text=True, capture_output=True, timeout=15)
            self.assertNotEqual(done.returncode, 0)
            self.assertIn("exit codes [7, 0]", done.stdout)
            self.assertEqual(len(list((root / "records").glob("*.json"))), 3)
            self.assertEqual(list((root / "memory-volume").iterdir()), [])

    def test_parent_deadline_cleans_blocked_workers(self):
        with tempfile.TemporaryDirectory() as directory:
            root, cmd, env = self.fixture(directory, BLOCK="1")
            script = root / ".oberth/test-store.py"
            text = script.read_text()
            self.assertEqual(text.count("DEADLINE_SECONDS = 66 * 60"), 1)
            # Only this synthetic fixture shortens the deadline; production
            # retains the same 66 minute package/Pod boundary.
            script.write_text(text.replace("DEADLINE_SECONDS = 66 * 60", "DEADLINE_SECONDS = 1"))
            started = time.monotonic()
            done = subprocess.run(cmd, cwd=root, env=env, text=True, capture_output=True, timeout=8)
            self.assertNotEqual(done.returncode, 0)
            self.assertIn("deadline", done.stderr)
            self.assertLess(time.monotonic() - started, 7)
            self.assertEqual(list((root / "tmp").iterdir()), [])
            self.assertEqual(list((root / "memory-volume").iterdir()), [])

    def test_interruption_reaps_workers_and_descendants_with_prompt_output(self):
        with tempfile.TemporaryDirectory() as directory:
            root, cmd, env = self.fixture(directory, BLOCK="1")
            process = subprocess.Popen(cmd, cwd=root, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
            try:
                with selectors.DefaultSelector() as selector:
                    selector.register(process.stdout, selectors.EVENT_READ)
                    selector.register(process.stderr, selectors.EVENT_READ)
                    seen = b""
                    deadline = time.monotonic() + 10
                    while time.monotonic() < deadline:
                        for key, _ in selector.select(0.1):
                            seen += os.read(key.fileobj.fileno(), 4096)
                        if (b"worker stdout live" in seen and b"worker stderr live" in seen
                                and len(list((root / "records").glob("*.child-ready"))) == 2):
                            break
                    else:
                        self.fail("worker output/descendant readiness did not arrive promptly")
                self.assertIsNone(process.poll(), "output appeared only after completion")
                process.send_signal(signal.SIGTERM)
                process.communicate(timeout=8)
                self.assertEqual(process.returncode, 128 + signal.SIGTERM)
                for path in (root / "records").glob("*.child-pid"):
                    with self.assertRaises(ProcessLookupError):
                        os.kill(int(path.read_text()), 0)
                self.assertEqual(list((root / "tmp").iterdir()), [])
                self.assertEqual(list((root / "memory-volume").iterdir()), [])
            finally:
                if process.poll() is None:
                    process.kill()
                process.communicate(timeout=8)


if __name__ == "__main__":
    unittest.main()
