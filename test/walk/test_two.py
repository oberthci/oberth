"""Offline controls for candidate orchestration; subprocesses and VMs are forbidden."""
import contextlib
from copy import deepcopy
import io
import json
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch
from walkassert import Failure, HERE, digest
from walkcommands import render
from walkconfig import Environment
from walkhost import Runtime
from walklib import Walk, main, parser
import walktwo as two
import walktwoacceptance as acceptance

COLLISION = (b"This profile does not own the existing installation. Choose another --namespace "
             b"and unused --https-node-port and --ssh-node-port.")


class TwoTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        values = json.loads((HERE / "env.example").read_text())
        values["state"] = str(self.root / "state")
        values["private"] = str(self.root / "private")
        values["two_installations"] = {"argo": {"namespace": "independent", "release": "plain"}, "instances": [
            {"name": "first", "namespace": "oberth", "pipeline_namespace": "oberth-runs",
             "url": "https://localhost:34443", "ssh_port": 34022, "https_node_port": 30443, "ssh_node_port": 30022,
             "run_id": "1" * 32, "commit_sha": "a" * 40, "repo": "forge/team/first"},
            {"name": "second", "namespace": "second", "pipeline_namespace": "second-runs",
             "url": "https://localhost:35443", "ssh_port": 35022, "https_node_port": 31443, "ssh_node_port": 31022,
             "run_id": "2" * 32, "commit_sha": "b" * 40, "repo": "forge/team/second"}]}
        env = self.root / "env.json"
        env.write_text(json.dumps(values))
        args = parser().parse_args(["--env", str(env), "--candidate", str(self.root / "dist"),
                                   "--two-installations", "install", "--out", str(self.root / "evidence")])
        load = SimpleNamespace(busy=False, samples=[(0, 0, 0)], sample=lambda: (0, 0, 0))
        self.walk = Walk(args, Environment.load(env, "walk-1"), Runtime(lambda: 100, lambda: "f" * 32, load))
        self.s = self.walk.s
        self.config = values["two_installations"]
        self.s.user = Mock(return_value=(0, b"{}", b""))
        self.s.run = Mock(side_effect=AssertionError("Live subprocess forbidden"))

    def test_configuration_refuses_namespace_port_and_client_collisions(self):
        two.configuration(self.s)
        for key in ("namespace", "pipeline_namespace", "url", "ssh_port", "https_node_port", "ssh_node_port"):
            with self.subTest(key=key):
                value = self.config["instances"][1][key]
                self.config["instances"][1][key] = self.config["instances"][0][key]
                # Environment.load owns the independent JSON object.
                self.s.config.values["two_installations"] = deepcopy(self.config)
                with self.assertRaises(Failure):
                    two.configuration(self.s)
                self.config["instances"][1][key] = value
        self.s.user.assert_not_called()

    def test_plan_is_offline_and_uses_one_ordered_entrypoint(self):
        for stage in ("install", "controllers", "schedule"):
            with patch("walklib.Environment.load", side_effect=AssertionError("Plan read environment")):
                with contextlib.redirect_stdout(io.StringIO()) as output:
                    self.assertEqual(main(["--plan", "--candidate", "/unused", "--two-installations", stage]), 0)
            result = json.loads(output.getvalue())
            preceding = set()
            for row in result["steps"]:
                self.assertTrue(set(row["needs"]) <= preceding)
                preceding.add(row["name"])
            self.assertIn(stage if stage != "install" else "second-install", preceding)
            self.assertIn("unsigned local candidate", result["target"])

    def test_failed_first_install_skips_collision_and_second_install_without_retry(self):
        with patch.object(two, "preflight", return_value={}), patch.object(two, "target", return_value={}), \
             patch.object(two, "independent", return_value={}), \
             patch.object(two, "install", side_effect=Failure("Install refused")) as install, \
             patch.object(two, "default_collision") as collision, contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(self.walk.execute(), 1)
        self.assertEqual(install.call_count, 1)
        collision.assert_not_called()
        self.assertEqual([r["status"] for r in self.s.results], ["pass", "pass", "pass", "fail", "skipped", "skipped", "pass"])

    def test_install_runs_once_verifies_and_preserves_shared_definitions(self):
        self.s.two_definitions = {"workflows": {"uid": "same"}}
        with patch.object(two, "crds", return_value=self.s.two_definitions), patch.object(two, "cli") as cli:
            two.install(self.s, 0)
            self.assertEqual(cli.call_count, 2)
            arguments = cli.call_args_list[0].args[2]
            self.assertIn("--chart", arguments)
            self.assertIn("--namespace", arguments)
            self.assertNotIn("--local-chart", arguments)
            self.assertEqual(arguments.count("--yes"), 1)
            self.assertIn("--verify", cli.call_args_list[1].args[2])
            with self.assertRaisesRegex(Failure, "fresh installation"):
                two.install(self.s, 0)
        with patch.object(two, "crds", return_value={}), patch.object(two, "cli"), \
             patch.object(two, "first_identity", return_value={}):
            with self.assertRaisesRegex(Failure, "shared definitions"):
                two.install(self.s, 1)

    def test_second_install_cannot_change_first_identity(self):
        self.s.two_definitions = {}
        with patch.object(two, "crds", return_value={}), patch.object(two, "cli"), \
             patch.object(two, "first_identity", side_effect=[{"revision": 1}, {"revision": 2}]):
            with self.assertRaisesRegex(Failure, "Second install changed"):
                two.install(self.s, 1)

    def test_observation_reuses_candidate_without_publishing_and_requires_receipt(self):
        self.s.args.two_installations = "controllers"
        self.s.args.candidate.mkdir()
        manifest = self.s.args.candidate / "SHA256SUMS"
        manifest.write_text("manifest")
        facts = {"candidate_commit": "c" * 40, "cli_sha256": "sha256", "manifest_sha256": digest(manifest)}
        with patch.object(two, "candidate_target", return_value=facts) as target:
            with self.assertRaisesRegex(Failure, "installation stage"):
                two.target(self.s)
            target.assert_not_called()
            two.receipt(self.s).parent.mkdir(parents=True)
            self.s.two_candidate = {"commit": facts["candidate_commit"], "cli_sha256": facts["cli_sha256"],
                                    "manifest_sha256": facts["manifest_sha256"],
                                    "configuration": two.configuration(self.s)}
            two.receipt(self.s).write_text(json.dumps(self.s.two_candidate))
            two.target(self.s)
            target.assert_called_with(self.s, fixture=False, publish=False)
            facts["manifest_sha256"] = "other-chart-bytes"
            with self.assertRaisesRegex(Failure, "configuration changed"):
                two.target(self.s)
            manifest.write_text("changed")
            target.reset_mock()
            with self.assertRaisesRegex(Failure, "manifest changed"):
                two.target(self.s)
            target.assert_not_called()

    def test_receipt_is_written_only_for_a_complete_installation_stage(self):
        self.s.two_candidate = {"commit": "c" * 40}
        self.s.results = [{"status": "fail"}]
        two.retained(self.s)
        self.assertFalse(two.receipt(self.s).exists())
        self.s.results = [{"status": "pass"}]
        two.receipt(self.s).parent.mkdir(parents=True)
        two.retained(self.s)
        self.assertEqual(json.loads(two.receipt(self.s).read_text()), self.s.two_candidate)

    def test_default_collision_refuses_interactive_and_yes_without_changing_first(self):
        self.s.user.return_value = (1, COLLISION, b"")
        with patch.object(two, "first_identity", return_value={"revision": 1}):
            two.default_collision(self.s)
        calls = self.s.user.call_args_list
        self.assertEqual(len(calls), 2)
        self.assertNotIn("--yes", calls[0].args[1]["arguments"])
        self.assertIn("--yes", calls[1].args[1]["arguments"])
        self.assertNotEqual(calls[0].kwargs["env"]["HOME"], calls[1].kwargs["env"]["HOME"])
        self.assertNotIn("OBERTH_PROFILE", calls[0].kwargs["env"])

    def test_collision_cannot_pass_after_first_identity_changes(self):
        self.s.user.return_value = (1, COLLISION, b"")
        with patch.object(two, "first_identity", side_effect=[{"revision": 1}, {"revision": 2}]):
            with self.assertRaisesRegex(Failure, "changed first identity"):
                two.default_collision(self.s)

    def test_collision_rejects_success_review_timeout_and_saved_profile(self):
        for index, (code, output, writes) in enumerate(((0, COLLISION, False),
                (-15, COLLISION, False), (1, COLLISION + b"Apply? [y/N]", False),
                (1, COLLISION, True), (1, b"Chart missing", False))):
            self.s.config.values["state"] = str(self.root / ("case-" + str(index)))
            def run(*args, **kwargs):
                if writes:
                    (Path(kwargs["env"]["HOME"]) / ".config/oberth").mkdir(parents=True)
                return code, output, b""
            self.s.user.side_effect = run
            with self.subTest(code=code, writes=writes), patch.object(two, "first_identity", return_value={}):
                with self.assertRaises(Failure):
                    two.default_collision(self.s)

    def test_controller_command_keeps_cluster_scope_and_explicit_cluster(self):
        command = render(self.s, "two-argo-install", {"release": "plain", "namespace": "independent",
                         "chart": "argo.tgz", "chart_version": "1.2.3"})
        self.assertNotIn("singleNamespace=true", command)
        self.assertNotIn("--namespaced", command)
        self.assertIn(str(self.s.config.kube), command)
        self.assertIn(self.s.config.context, command)

    def test_crd_snapshot_requires_all_eight_and_keeps_uid_spec_and_ownership(self):
        items = [{"metadata": {"name": name, "uid": name + "-uid", "labels": {"owner": "Helm"},
                               "annotations": {"release": "plain"}}, "spec": {"versions": ["v1alpha1"]}}
                 for name in sorted(two.CRDS)]
        self.s.user.return_value = (0, json.dumps({"items": items}).encode(), b"")
        snapshot = two.crds(self.s)
        self.assertEqual(set(snapshot), two.CRDS)
        first = items[0]
        self.assertEqual(snapshot[first["metadata"]["name"]], {
            "uid": first["metadata"]["uid"], "labels": first["metadata"]["labels"],
            "annotations": first["metadata"]["annotations"], "spec": first["spec"]})
        self.s.user.return_value = (0, json.dumps({"items": items[1:]}).encode(), b"")
        with self.assertRaisesRegex(Failure, "all eight"):
            two.crds(self.s)

    def native(self, instance):
        return [{"kind": "Workflow", "metadata": {"name": instance["name"] + "-workflow",
                 "labels": {acceptance.LABEL: instance["namespace"], "oberth.ci/run": instance["run_id"]}},
                 "status": {"phase": "Succeeded"}}]

    def controller_tool(self, session, instance, name, arguments, **kwargs):
        if name == "run_get":
            if arguments["id"] != instance["run_id"]:
                return 1, b'{"code":"not_found"}', b""
            value = {"status": "passed", "repo": instance["repo"], "commit_sha": instance["commit_sha"]}
        elif name == "repo_list":
            value = {"repositories": [{"repo": instance["repo"]}]}
        else:
            value = {"runs": [{"id": instance["run_id"], "repo": instance["repo"]}]}
        return 0, json.dumps(value).encode(), b""

    def test_controller_observation_requires_exact_runs_labels_and_silent_independent_controller(self):
        self.s.user.return_value = (0, b"unrelated-workflow", b"")
        with patch.object(acceptance, "tool", side_effect=self.controller_tool), \
             patch.object(acceptance, "objects", side_effect=lambda s, i: self.native(i)):
            acceptance.controllers(self.s)
            self.s.user.return_value = (0, b"acted on first-workflow", b"")
            with self.assertRaisesRegex(Failure, "acted on"):
                acceptance.controllers(self.s)
        for wrong in ("", "other-installation"):
            items = self.native(self.config["instances"][0])
            items[0]["metadata"]["labels"][acceptance.LABEL] = wrong
            with self.assertRaisesRegex(Failure, "controller label"):
                acceptance.labelled(items, "oberth")

    def test_positive_run_control_rejects_wrong_commit_and_transport_failure_is_not_isolation(self):
        for response in ((0, b'{"status":"passed","repo":"forge/team/first","commit_sha":"wrong"}', b""),
                         (1, b"", b"connection refused")):
            def tool(s, instance, name, arguments, **kwargs):
                if name == "run_get" and ((response[0] == 0 and arguments["id"] == instance["run_id"])
                                          or (response[0] == 1 and arguments["id"] != instance["run_id"])):
                    return response
                return self.controller_tool(s, instance, name, arguments, **kwargs)
            self.s.user.return_value = (0, b"unrelated-workflow", b"")
            with patch.object(acceptance, "tool", side_effect=tool), \
                 patch.object(acceptance, "objects", side_effect=lambda s, i: self.native(i)):
                with self.assertRaises(Failure):
                    acceptance.controllers(self.s)

    def test_lists_cannot_hide_foreign_objects_or_incomplete_pages(self):
        own, foreign = self.config["instances"]
        repos = {"repositories": [{"repo": own["repo"]}]}
        runs = {"runs": [{"repo": own["repo"], "id": own["run_id"]}]}
        acceptance.visible(repos, runs, own, foreign)
        for changed in ({**runs, "next_cursor": "more"},
                        {"runs": runs["runs"] + [{"repo": own["repo"], "id": foreign["run_id"]}]},
                        {"runs": runs["runs"] + [{"repo": foreign["repo"], "id": foreign["run_id"]}]}):
            with self.assertRaises(Failure):
                acceptance.visible(repos, changed, own, foreign)

    def test_schedule_requires_two_unique_occurrences_per_parent(self):
        items = [{"kind": "CronWorkflow", "metadata": {"uid": "cron"}}]
        for index in (1, 2):
            items.append({"kind": "Workflow", "metadata": {"name": str(index),
                          "ownerReferences": [{"kind": "CronWorkflow", "uid": "cron"}],
                          "annotations": {"workflows.argoproj.io/scheduled-time": f"2030-01-01T00:0{index}:00Z"}}})
        self.assertEqual(acceptance.unique_occurrences(items), 2)
        for changed in (items[:2], items + [deepcopy(items[1])], [],
                        items + [{"kind": "CronWorkflow", "metadata": {"uid": "unobserved"}}]):
            with self.assertRaises(Failure):
                acceptance.unique_occurrences(changed)

    def test_scope_guard_blocks_before_any_environment_command(self):
        self.s.k = Mock(return_value=(0, b'{"items":[]}', b""))
        with patch.object(two, "capped_scope", side_effect=Failure("Uncapped")), \
             patch.object(two.walksetup, "preflight") as preflight:
            with self.assertRaisesRegex(Failure, "Uncapped"):
                two.preflight(self.s)
        preflight.assert_not_called()
        self.s.user.assert_not_called()

    def test_scope_guard_requires_finite_memory_cap_and_zero_swap_on_an_ancestor(self):
        group = self.root / "cgroup"
        nested = group / "scope/child"
        nested.mkdir(parents=True)
        proc = self.root / "membership"
        proc.write_text("0::/scope/child\n")
        for cap, swap, allowed in ((str(8 * 1024**3), "0", True),
                                   (str(9 * 1024**3), "0", False), ("max", "0", False),
                                   ("1024", "max", False)):
            (nested.parent / "memory.max").write_text(cap)
            (nested.parent / "memory.swap.max").write_text(swap)
            def path(value):
                return proc if value == "/proc/self/cgroup" else group
            with patch.object(two, "Path", side_effect=path):
                if allowed:
                    two.capped_scope()
                else:
                    with self.assertRaises(Failure):
                        two.capped_scope()
