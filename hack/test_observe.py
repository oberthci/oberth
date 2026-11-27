import copy
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
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


if __name__ == "__main__":
    unittest.main()
