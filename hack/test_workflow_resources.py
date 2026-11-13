"""Regression cases for explicit workflow container admission."""
import copy
import importlib.util
from pathlib import Path
import unittest

SOURCE = Path(__file__).with_name("check_workflow_resources.py")
if not SOURCE.exists():
    SOURCE = Path(__file__).resolve().parents[1] / "tools/check_workflow_resources.py"
SPEC = importlib.util.spec_from_file_location("resource_guard", SOURCE)
guard = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(guard)


class ResourceGuardTests(unittest.TestCase):
    def workflow(self):
        return {"kind": "Workflow", "spec": {"templates": [{"name": "test", "container": {
            "image": "runner", "resources": {"requests": {"cpu": "50m", "memory": "64Mi"},
                                             "limits": {"cpu": "4", "memory": "8Gi"}}}}]}}

    def test_boundary(self):
        self.assertEqual([], guard.check(self.workflow()))

    def test_each_required_field(self):
        for group in ("requests", "limits"):
            for resource in ("cpu", "memory"):
                with self.subTest(group=group, resource=resource):
                    workflow = self.workflow()
                    del workflow["spec"]["templates"][0]["container"]["resources"][group][resource]
                    self.assertTrue(guard.check(workflow))

    def test_every_container_role(self):
        for role in ("script", "initContainers", "sidecars", "containerSet"):
            workflow = self.workflow()
            template = workflow["spec"]["templates"][0]
            box = template.pop("container")
            del box["resources"]
            template[role] = box if role == "script" else ({"containers": [box]} if role == "containerSet" else [box])
            self.assertTrue(guard.check(workflow), role)

    def test_ceiling_and_request_order(self):
        for resource, value in (("cpu", "4001m"), ("memory", "8193Mi")):
            workflow = self.workflow()
            workflow["spec"]["templates"][0]["container"]["resources"]["limits"][resource] = value
            self.assertTrue(guard.check(workflow))
        workflow = self.workflow()
        workflow["spec"]["templates"][0]["container"]["resources"]["requests"]["cpu"] = "5"
        self.assertTrue(guard.check(workflow))

    def test_invalid_quantities(self):
        for value in ("NaN", "Infinity", "-1", 0, True, {}, "1garbage"):
            workflow = self.workflow()
            workflow["spec"]["templates"][0]["container"]["resources"]["requests"]["cpu"] = value
            self.assertTrue(guard.check(workflow), value)

    def test_compressible_cpu_request_cap(self):
        workflow = self.workflow()
        workflow['spec']['templates'][0]['container']['resources']['requests']['cpu'] = '1001m'
        self.assertTrue(guard.check(workflow))

    def test_workflow_annotations_are_not_resource_policy(self):
        workflow = self.workflow()
        workflow["metadata"] = {"annotations": {
            "/".join(("oberth.ci", "size")): "XL",
            "/".join(("oberth.ci", "weight")): "4",
        }}
        self.assertEqual([], guard.check(workflow))

    def test_empty_metadata(self):
        workflow = self.workflow()
        workflow['metadata'] = None
        self.assertEqual([], guard.check(workflow))

    def test_multidocument_and_aliases(self):
        raw = "kind: Workflow\nspec:\n  templates:\n  - name: a\n    container: &box\n      resources:\n        requests: {cpu: 50m, memory: 64Mi}\n        limits: {cpu: 100m, memory: 128Mi}\n  - name: b\n    script: *box\n"
        self.assertEqual([], guard.check(guard.yaml.load(raw, Loader=guard.UniqueLoader)))
        with self.assertRaises(ValueError):
            guard.yaml.load("kind: Workflow\nkind: WorkflowTemplate\n", Loader=guard.UniqueLoader)

    def test_templates_and_cron(self):
        for kind in ("WorkflowTemplate", "ClusterWorkflowTemplate", "CronWorkflow"):
            workflow = self.workflow()
            workflow["kind"] = kind
            if kind == "CronWorkflow":
                workflow["spec"] = {"workflowSpec": workflow["spec"]}
            self.assertEqual([], guard.check(workflow))

    def test_patches_cannot_bypass_guard(self):
        workflow = self.workflow()
        workflow["spec"]["podSpecPatch"] = '{"containers": [{"name": "main"}]}'
        self.assertTrue(guard.check(workflow))


if __name__ == "__main__":
    unittest.main()
