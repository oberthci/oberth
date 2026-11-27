import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("observe", Path(__file__).with_name("observe.py"))
observe = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(observe)


def fixture():
    return {"observed_at": "2026-01-01T00:00:00Z", "workflows": {"items": [{"metadata": {
        "namespace": "runs", "name": "run", "uid": "wf-1", "labels": {"oberth.ci/repo": "7", "oberth.ci/sha": "a" * 40}},
        "status": {"nodes": {"node-1": {"templateName": "lint"}}}}]},
        "pods": {"items": [{"metadata": {"namespace": "runs", "name": "pod", "uid": "pod-1",
        "annotations": {"workflows.argoproj.io/node-id": "node-1"},
        "ownerReferences": [{"uid": "wf-1", "controller": True}]}, "spec": {
        "containers": [{"name": "main", "env": [{"name": "PRIVATE", "value": "never-print"}],
        "resources": {"requests": {"cpu": "1", "memory": "1Gi", "ephemeral-storage": "2Gi"}}}],
        "initContainers": [{"name": "plugin", "restartPolicy": "Always"}]}}]},
        "stats": {"pods": [{"podRef": {"uid": "pod-1"}, "containers": [{"name": "main", "memory": {"workingSetBytes": 100}}]}]}}


class ObservationTests(unittest.TestCase):
    def native_child(self, program, timeout=2):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        executable = Path(directory.name) / "kubectl"
        executable.write_text("#!" + sys.executable + "\n" + program)
        executable.chmod(0o700)
        children = []
        popen = subprocess.Popen
        def spawn(*args, **kwargs):
            child = popen(*args, **kwargs)
            children.append(child)
            return child
        self.enterContext(patch.dict(os.environ, {"PATH": directory.name}))
        self.enterContext(patch.object(observe.subprocess, "Popen", spawn))
        self.enterContext(patch.object(observe, "READ_TIMEOUT", timeout))
        return Path(directory.name), children

    def test_native_read_stops_oversize_producer_and_reaps_it(self):
        directory, children = self.native_child(
            "import os, pathlib, time\n"
            "progress = pathlib.Path(__file__).with_name('progress').open('ab', buffering=0)\n"
            "for n in range(512):\n"
            "    os.write(1, b'x' * 4096)\n"
            "    progress.write(b'x')\n"
            "time.sleep(1)\n")
        with patch.object(observe, "MAX_FRAME", 65536):
            with self.assertRaises(ValueError):
                observe.read_native(["get", "pods"])
        self.assertLessEqual((directory / "progress").stat().st_size * 4096, 65536 + 65536)
        self.assertIsNotNone(children[0].returncode)
        with self.assertRaises(ChildProcessError):
            os.waitpid(children[0].pid, os.WNOHANG)

    def test_native_timeout_kills_and_reaps_silent_or_closed_producer(self):
        for close in (False, True):
            with self.subTest(close=close):
                _, children = self.native_child("import os, time\n" +
                    ("os.close(1)\n" if close else "") + "time.sleep(60)\n", timeout=0.1)
                with self.assertRaises((ValueError, subprocess.TimeoutExpired)):
                    observe.read_native(["get", "pods"])
                self.assertIsNotNone(children[0].returncode)
                with self.assertRaises(ChildProcessError):
                    os.waitpid(children[0].pid, os.WNOHANG)

    def test_combined_native_budget_retains_readable_omission(self):
        value = {"items": [{"metadata": {"name": "x" * 500}}]}
        args = type("Args", (), {"samples": 1, "namespace": ["runs"], "node": "node"})()
        with patch.object(observe, "MAX_FRAME", 1024), patch.object(observe, "read_native", return_value=value), \
                patch("sys.stdout", new_callable=io.StringIO) as output:
            self.assertLess(len(observe.encoded_frame(value).encode()) + 1, observe.MAX_FRAME)
            observe.collect(args)
            result = list(observe.frames(io.BytesIO(output.getvalue().encode())))
        self.assertEqual(len(result), 1)
        self.assertIn("native_frame_oversize", observe.summarize(result)["missing"])
        self.assertEqual(result[0]["pods"], [])

    def test_projection_expansion_retains_readable_omission(self):
        frame = fixture()
        size = len(json.dumps(frame, separators=(",", ":")).encode()) + 1
        self.assertGreater(len(json.dumps(observe.observation(frame)).encode()), size)
        with patch.object(observe, "MAX_FRAME", size), patch("sys.stdout", new_callable=io.StringIO) as output:
            observe.emit(frame)
            result = list(observe.frames(io.BytesIO(output.getvalue().encode())))
        self.assertEqual(result[0]["missing"], ["projected_frame_oversize"])
        self.assertEqual(observe.summarize(result)["missing"]["projected_frame_oversize"], 1)
        self.assertEqual(result[0]["pods"], [])

    def test_exact_uid_join_retains_defaulted_resources_and_helpers(self):
        frame = fixture()
        before = copy.deepcopy(frame)
        result = observe.observation(frame)
        row = result["pods"][0]
        self.assertEqual((row["workflow"]["uid"], row["repo"], row["commit"], row["template"]), ("wf-1", "7", "a" * 40, "lint"))
        self.assertEqual(row["containers"][0]["resources"]["requests"]["ephemeral-storage"], "2Gi")
        self.assertEqual(row["containers"][1]["restart_policy"], "Always")
        self.assertIn("request:cpu", row["containers"][1]["missing"])
        self.assertIn("cpu_throttling", row["containers"][0]["missing"])
        self.assertNotIn("never-print", json.dumps(result))
        self.assertEqual(frame, before)

    def test_recreated_pod_or_workflow_never_joins_by_name(self):
        frame = fixture()
        frame["workflows"]["items"][0]["metadata"]["uid"] = "wf-2"
        frame["stats"]["pods"][0]["podRef"]["uid"] = "old-pod"
        row = observe.observation(frame)["pods"][0]
        self.assertIn("workflow_uid_join", row["missing"])
        self.assertIn("usage", row["missing"])
        self.assertIsNone(row["workflow"]["uid"])

    def test_resource_digest_does_not_change_with_usage(self):
        frame = fixture()
        first = observe.observation(frame)["pods"][0]["resources_sha256"]
        frame["stats"]["pods"][0]["containers"][0]["memory"]["workingSetBytes"] = 200
        self.assertEqual(first, observe.observation(frame)["pods"][0]["resources_sha256"])
        frame["pods"]["items"][0]["spec"]["containers"][0]["resources"]["requests"]["cpu"] = "2"
        self.assertNotEqual(first, observe.observation(frame)["pods"][0]["resources_sha256"])

    def test_claim_bytes_keep_pvc_identity_and_missing_reads(self):
        frame = fixture()
        frame["pods"]["items"][0]["spec"]["volumes"] = [{"name": "cache", "persistentVolumeClaim": {"claimName": "cache"}}]
        self.assertIn("pvc_uid:cache", observe.observation(frame)["pods"][0]["missing"])
        frame["pvcs"] = {"items": [{"metadata": {"namespace": "runs", "name": "cache", "uid": "pvc-1"}}]}
        frame["stats"]["pods"][0]["volume"] = [{"name": "cache", "usedBytes": 42, "pvcRef": {"namespace": "runs", "name": "cache"}}]
        row = observe.observation(frame)["pods"][0]
        self.assertEqual(row["claims"][0]["pvc"]["uid"], "pvc-1")
        self.assertEqual(row["volume_usage"][0]["usedBytes"], 42)

    def test_numeric_and_claim_coverage_survives_identity_resolution(self):
        for value in (None, 0, 42, False, "0", -1):
            for volume in (None, {"name": "cache", "time": "volume-time"}):
                with self.subTest(value=value, volume=volume):
                    frame = fixture()
                    pod = frame["pods"]["items"][0]
                    pod["spec"].pop("initContainers")
                    pod["spec"]["volumes"] = [{"name": "cache", "persistentVolumeClaim": {"claimName": "cache"}}]
                    frame["pvcs"] = {"items": [{"metadata": {"namespace": "runs", "name": "cache", "uid": "pvc-1"}}]}
                    stats = frame["stats"]["pods"][0]
                    metrics = {metric: {"time": metric + "-time"} for metric in ("cpu", "memory", "rootfs", "logs")}
                    stats["ephemeral-storage"] = {"time": "pod-time"}
                    if volume is not None:
                        stats["volume"] = [dict(volume)]
                    if value is not None:
                        for metric, field in (("cpu", "usageNanoCores"), ("memory", "workingSetBytes"), ("rootfs", "usedBytes"), ("logs", "usedBytes")):
                            metrics[metric][field] = value
                        stats["ephemeral-storage"]["usedBytes"] = value
                        if volume is not None:
                            stats["volume"][0]["usedBytes"] = value
                    stats["containers"] = [{"name": "main", **metrics}]
                    projected = observe.observation(frame)
                    row = projected["pods"][0]
                    summary = observe.summarize([projected, projected])["missing"]
                    valid = type(value) is int and value >= 0
                    for metric in metrics:
                        self.assertEqual(summary.get("container:" + metric, 0), 0 if valid else 2)
                        self.assertEqual(row["containers"][0]["usage"][metric]["time"], metric + "-time")
                    self.assertEqual(summary.get("ephemeral_storage", 0), 0 if valid else 2)
                    self.assertEqual(summary.get("pvc_usage:cache", 0), 0 if valid and volume is not None else 2)
                    self.assertNotIn("pvc_uid:cache", summary)
                    self.assertEqual(row["ephemeral_storage"]["time"], "pod-time")
                    if volume is not None:
                        self.assertEqual(row["volume_usage"][0]["time"], "volume-time")

    def test_child_owner_chain_and_unknowns_remain_in_denominator(self):
        frame = fixture()
        frame["pods"]["items"][0]["metadata"]["ownerReferences"][0]["uid"] = "job-1"
        frame["owners"] = {"items": [{"metadata": {"uid": "job-1", "namespace": "runs", "ownerReferences": [{"uid": "wf-1", "controller": True}]}}]}
        result = observe.observation(frame)
        self.assertEqual(result["pods"][0]["workflow"]["uid"], "wf-1")
        del frame["owners"]
        summary = observe.summarize([result, observe.observation(frame)])
        self.assertEqual(summary["pod_samples"], 2)
        self.assertEqual(summary["joined_run_pod_samples"], 1)
        self.assertEqual(summary["missing"]["workflow_uid_join"], 1)
        self.assertFalse(summary["sizing_eligible"])

    def test_collect_only_issues_gets_and_redacts_failures(self):
        calls = []
        def read(args):
            calls.append(args)
            raise ValueError("private response")
        args = type("Args", (), {"samples": 1, "namespace": ["runs"], "node": "node"})()
        with patch.object(observe, "read_native", read), patch("builtins.print") as output:
            observe.collect(args)
        self.assertTrue(all(a[0] == "get" for a in calls))
        result = json.loads(output.call_args.args[0])
        self.assertIn("pods:runs", result["missing"])
        self.assertNotIn("private response", json.dumps(result))

    def test_invalid_input_error_never_echoes_values(self):
        result = subprocess.run([sys.executable, "-I", str(Path(__file__).with_name("observe.py")), "project"],
                                input=b'{"secret": "never-print"', capture_output=True, check=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertNotIn(b"never-print", result.stderr)
        self.assertEqual(result.stdout, b"")

    def test_cli_numeric_boundaries_reach_collector(self):
        for samples in (1, 8640):
            for interval in (20, 60):
                argv = ["observe.py", "collect", "--namespace", "runs", "--node", "node",
                        "--samples", str(samples), "--interval", str(interval)]
                with patch.object(sys, "argv", argv), patch.object(observe, "collect") as collect:
                    observe.main()
                args = collect.call_args.args[0]
                self.assertEqual((args.samples, args.interval), (samples, interval))

    def test_cli_invalid_numbers_are_short_and_never_read_native(self):
        directory, _ = self.native_child("from pathlib import Path\nPath(__file__).with_name('called').touch()\n")
        for option, values in (("--samples", ("0", "8641", "bad", "9" * 10000)),
                               ("--interval", ("19", "61", "bad"))):
            for value in values:
                with self.subTest(option=option, value=value[:20]):
                    result = subprocess.run([sys.executable, "-I", str(Path(__file__).with_name("observe.py")),
                        "collect", "--namespace", "runs", "--node", "node", option, value],
                        capture_output=True, timeout=5)
                    self.assertEqual(result.returncode, 2)
                    self.assertEqual(result.stdout, b"")
                    error = result.stderr.decode().splitlines()[-1]
                    self.assertLessEqual(len(error), 80)
                    self.assertLessEqual(len(error.rsplit(": ", 1)[-1]), 40)
                    self.assertLess(len(result.stderr), 1024)
                    self.assertFalse((directory / "called").exists())

    def test_cli_help_does_not_enumerate_sample_range(self):
        result = subprocess.run([sys.executable, "-I", str(Path(__file__).with_name("observe.py")), "--help"],
                                capture_output=True, timeout=5)
        self.assertEqual(result.returncode, 0)
        self.assertLess(len(result.stdout), 2048)
        self.assertIn(b"1..8640", result.stdout)


if __name__ == "__main__":
    unittest.main()
