"""Source load timestamps distinguish real outages from Pod termination."""

from copy import deepcopy
import unittest
import json
from unittest.mock import Mock, patch
from walkassert import Blocked, Failure
from walkrestart import (
    Observation,
    pod_sample,
    restart,
    validate,
    find_workflow,
    restore,
    NativeCommand,
    source_checks,
    outage_starts,
    dependency_start,
)
from workflows import fixtures, SOURCE_STEPS


def event(uid, namespace="oberth", **values):
    meta = {
        "uid": uid,
        "namespace": namespace,
        "name": uid,
        "creationTimestamp": "2026-01-01T00:00:00Z",
        "resourceVersion": "1",
        "labels": {},
        "ownerReferences": [{"uid": "workflow-uid"}],
    }
    meta.update(values)
    return {
        "type": "ADDED",
        "object": {"metadata": meta, "spec": {"initContainers": [
            {"name": "init"}, {"name": "init-artifact-oberth"}]}, "status": {"conditions": [
            {"type": "Ready", "status": "True", "lastTransitionTime": "2026-01-01T00:00:10Z"}]}},
    }


class RestartTests(unittest.TestCase):
    def setUp(self):
        self.old = pod_sample(event("old")["object"])
        self.state = Observation(self.old)

    def test_terminating_server_is_not_evidence_of_container_exit(self):
        self.state.add(event("old", deletionTimestamp="2026-01-01T00:00:01Z"))
        self.state.add(event("new"))
        self.assertIsNone(self.state.exit_at)
        self.assertIsNotNone(self.state.replacement())
        old = event("old")
        old["object"]["status"]["containerStatuses"] = [
            {"name": "oberth", "state": {"terminated": {"finishedAt": "2026-01-01T00:00:05Z"}}}]
        self.state.add(old)
        self.assertEqual(self.state.exit_at, "2026-01-01T00:00:05Z")

    def test_outage_holds_after_observed_dependency_start_then_restores_one_ready_replica(self):
        exited = event("old")
        exited["object"]["status"]["containerStatuses"] = [
            {"name": "oberth", "state": {"terminated": {"finishedAt": "2026-01-01T00:00:05Z"}}}]
        source = event("source", "oberth-argo", labels={"workflows.argoproj.io/workflow": "workflow"})
        source["object"]["status"]["initContainerStatuses"] = [
            {"name": "init-artifact-oberth", "state": {"running": {"startedAt": "2026-01-01T00:00:06Z"}}}]
        session, watch = Mock(), Mock()
        watch.__enter__ = Mock(return_value=watch)
        watch.__exit__ = Mock(return_value=None)
        watch.events.return_value = iter([event("old"), exited, source])
        watch.s, watch.deadline = session, 100
        session.runtime.clock.side_effect = [10, 25]
        deployment = {"spec": {"replicas": 1}, "status": {"readyReplicas": 1}}
        session.k.side_effect = [(0, b"ready", b""), (0, json.dumps(deployment).encode(), b""),
                                 (0, json.dumps({"items": [event("new")["object"]]}).encode(), b"")]
        with patch("walkrestart.NativeCommand", return_value=watch):
            state = restart(session, "workflow", self.old, 100)
        self.assertEqual(state.hold_seconds, 15)
        self.assertEqual(watch.deadline, 25)
        self.assertEqual(state.waiter["started_at"], "2026-01-01T00:00:06Z")
        self.assertEqual(state.restored, {"replicas": 1, "ready_replicas": 1})
        self.assertEqual([c.args for c in session.user.call_args_list], [
            ("server-scale", {"replicas": 0}), ("workflow-resume", {"workflow": "workflow"}),
            ("server-scale", {"replicas": 1})])

    def test_restore_and_replica_verification_run_when_outage_observation_fails(self):
        for stage in ("exit", "load", "hold"):
            session, watch = Mock(), Mock()
            watch.__enter__ = Mock(return_value=watch)
            watch.__exit__ = Mock(return_value=None)
            watch.s, watch.deadline = session, 100
            events = [event("old")]
            if stage != "exit":
                exited = event("old")
                exited["object"]["status"]["containerStatuses"] = [
                    {"name": "oberth", "state": {"terminated": {"finishedAt": "2026-01-01T00:00:05Z"}}}]
                events.append(exited)
            if stage == "hold":
                source = event("source", "oberth-argo", labels={"workflows.argoproj.io/workflow": "workflow"})
                source["object"]["status"]["initContainerStatuses"] = [
                    {"name": "init-artifact-oberth", "state": {"running": {"startedAt": "2026-01-01T00:00:06Z"}}}]
                events.append(source)
                session.runtime.clock.side_effect = [0, 14]
            watch.events.return_value = iter(events)
            deployment = {"spec": {"replicas": 1}, "status": {"readyReplicas": 1}}
            session.k.side_effect = [(0, b"ready", b""), (0, json.dumps(deployment).encode(), b""),
                                     (0, json.dumps({"items": [event("new")["object"]]}).encode(), b"")]
            with self.subTest(stage=stage), patch("walkrestart.NativeCommand", return_value=watch), \
                    self.assertRaises(Failure if stage == "hold" else Blocked) as caught:
                restart(session, "workflow", self.old, 100)
            if stage == "hold":
                self.assertNotIsInstance(caught.exception, Blocked)
            self.assertEqual(caught.exception.facts["replica_restoration"], {"replicas": 1, "ready_replicas": 1})
            self.assertEqual(session.user.call_args_list[-1].args, ("server-scale", {"replicas": 1}))
            self.assertEqual(session.k.call_args_list[-2].args,
                             ("-n", "oberth", "get", "deployment", "oberth", "-o", "json"))

    def test_restores_one_replica_after_scale_down_or_watch_failure(self):
        for stage in ("scale", "watch"):
            session, watch = Mock(), Mock()
            watch.__enter__ = Mock(return_value=watch)
            watch.__exit__ = Mock(return_value=None)
            def events():
                yield event("old")
                raise Failure("watch failed")
            watch.events.return_value = events()
            if stage == "scale":
                session.user.side_effect = [Failure("scale failed"), None]
            deployment = {"spec": {"replicas": 1}, "status": {"readyReplicas": 1}}
            session.k.side_effect = [(0, b"ready", b""), (0, json.dumps(deployment).encode(), b""),
                                     (0, json.dumps({"items": [event("new")["object"]]}).encode(), b"")]
            with self.subTest(stage=stage), patch("walkrestart.NativeCommand", return_value=watch), \
                    self.assertRaisesRegex(Failure, stage + " failed"):
                restart(session, "workflow", self.old, 100)
            self.assertEqual(session.user.call_args_list[-1].args, ("server-scale", {"replicas": 1}))
            self.assertEqual(session.k.call_args_list[-2].args,
                             ("-n", "oberth", "get", "deployment", "oberth", "-o", "json"))

    def test_restore_checks_replica_count_even_if_scale_or_readiness_fails(self):
        for failure in ("scale", "ready", "count", "unready"):
            session = Mock()
            deployment = {"spec": {"replicas": 0 if failure == "count" else 1},
                          "status": {"readyReplicas": 0 if failure == "unready" else 1}}
            if failure == "scale":
                session.user.side_effect = Failure("scale failed")
            session.k.side_effect = [Failure("ready failed") if failure == "ready" else (0, b"", b""),
                                     (0, json.dumps(deployment).encode(), b"")]
            with self.subTest(failure=failure), self.assertRaises(Failure):
                restore(session)
            self.assertEqual(session.k.call_args_list[-1].args,
                             ("-n", "oberth", "get", "deployment", "oberth", "-o", "json"))

    def test_only_a_dependency_start_inside_the_real_outage_can_qualify(self):
        facts = {"outage_start": "2026-01-01T00:00:05Z", "outage_end": "2026-01-01T00:00:10Z",
                 "source_checks": [{"uid": "step", "waited_started_at": "2026-01-01T00:00:06.123Z"}]}
        outage_starts(facts)
        self.assertEqual(facts["outage_starts"], ["step"])
        for time in (None, "2026-01-01T00:00:04Z", "2026-01-01T00:00:10Z"):
            facts["source_checks"][0]["waited_started_at"] = time
            with self.subTest(time=time), self.assertRaisesRegex(Blocked, "No server-dependent"):
                outage_starts(facts)
        facts["outage_start"] = None
        with self.assertRaisesRegex(Blocked, "not both observed"):
            outage_starts(facts)

    def test_first_server_dependency_comes_from_native_init_order_for_each_target(self):
        pod = event("source", "oberth-argo")["object"]
        pod["spec"]["initContainers"].insert(0, {"name": "oberth-executor-token"})
        pod["status"]["initContainerStatuses"] = [
            {"name": "init-artifact-oberth", "state": {"running": {"startedAt": "2026-01-01T00:00:12Z"}}},
            {"name": "oberth-executor-token", "state": {"terminated": {"startedAt": "2026-01-01T00:00:06Z"}}}]
        self.assertEqual(dependency_start(pod), {
            "container": "oberth-executor-token", "started_at": "2026-01-01T00:00:06Z"})
        pod["spec"]["initContainers"].reverse()
        self.assertEqual(dependency_start(pod)["container"], "init-artifact-oberth")
        pod["spec"]["initContainers"].reverse()
        pod["spec"]["initContainers"].pop(0)
        self.assertEqual(dependency_start(pod), {
            "container": "init-artifact-oberth", "started_at": "2026-01-01T00:00:12Z"})
        pod["status"]["initContainerStatuses"] = []
        self.assertIsNone(dependency_start(pod))
        pod["spec"]["initContainers"] = [{"name": "unrelated"}]
        with self.assertRaisesRegex(Failure, "no identifiable"):
            dependency_start(pod)

    def test_native_workflow_selection_waits_for_the_exact_source_sha(self):
        wrong = {"spec": {"suspend": True, "arguments": {"parameters": [{"name": "oberth-sha", "value": "other"}]}}}
        wanted = {
            "metadata": {"name": "native-name", "uid": "native-uid"},
            "spec": {"suspend": True, "arguments": {"parameters": [{"name": "oberth-sha", "value": "exact"}]}},
        }
        watch = Mock()
        watch.__enter__ = Mock(return_value=watch)
        watch.__exit__ = Mock(return_value=None)
        watch.events.return_value = iter([{"object": wrong}, {"object": wanted}])
        with patch("walkrestart.NativeCommand", return_value=watch):
            self.assertEqual(find_workflow(Mock(), "exact", 10), wanted)

    def test_native_inspector_refuses_product_operations_before_starting_a_process(self):
        with patch("walkrestart.subprocess.Popen") as process, self.assertRaisesRegex(Failure, "cannot hide"):
            with NativeCommand(Mock(), 10, "repo"):
                pass
        process.assert_not_called()

    def test_source_proof_cannot_borrow_a_recreated_pod(self):
        pod = event("source", "oberth-argo", labels={"workflows.argoproj.io/workflow": "workflow"})["object"]
        session = Mock()
        session.user.return_value = (0, b"walk: restart source exact clean", b"")
        session.k.side_effect = [(0, json.dumps({"items": [pod]}).encode(), b""),
                                 (0, json.dumps({"metadata": {"uid": "another"}}).encode(), b"")]
        with self.assertRaisesRegex(Failure, "Source Pod UID changed"):
            source_checks(session, {"metadata": {"name": "workflow", "uid": "workflow-uid"}}, "exact")

    def test_waited_container_and_later_exact_source_use_the_same_pod(self):
        pod = event("source", "oberth-argo", labels={"workflows.argoproj.io/workflow": "workflow"})["object"]
        pod["spec"]["initContainers"].insert(0, {"name": "oberth-executor-token"})
        pod["status"]["initContainerStatuses"] = [{"name": "oberth-executor-token",
            "state": {"terminated": {"startedAt": "2026-01-01T00:00:06Z"}}}]
        session = Mock()
        session.user.side_effect = [(0, b"walk: restart source exact clean", b""),
                                   (0, b'2026-01-01T00:00:12.123Z msg="Downloading artifact" name=source', b"")]
        session.k.side_effect = [(0, json.dumps({"items": [pod]}).encode(), b""),
                                 (0, json.dumps(pod).encode(), b"")]
        checks = source_checks(session, {"metadata": {"name": "workflow", "uid": "workflow-uid"}}, "exact")
        self.assertEqual(checks[0]["load_start"], "2026-01-01T00:00:12.123Z")
        self.assertNotEqual(checks[0]["load_start"], pod["metadata"]["creationTimestamp"])
        self.assertEqual(checks[0]["uid"], pod["metadata"]["uid"])
        self.assertEqual(checks[0]["waited_container"], "oberth-executor-token")
        self.assertEqual(checks[0]["waited_started_at"], "2026-01-01T00:00:06Z")
        self.assertTrue(checks[0]["exact_source"])

    def test_fixture_has_several_real_source_steps_and_no_authored_retries(self):
        value = fixtures("image", "marker", "registry:5000", "owned")["restart"]
        root, leaf = value["spec"]["templates"]
        self.assertGreaterEqual(SOURCE_STEPS, 3)
        self.assertEqual(len(root["steps"]), SOURCE_STEPS)
        self.assertTrue(all(len(group) == 1 for group in root["steps"]))
        self.assertTrue(leaf["inputs"]["artifacts"][0]["plugin"])
        self.assertTrue(value["spec"]["suspend"])
        self.assertNotIn("retryStrategy", str(value))

    def test_green_exact_source_is_required_without_native_retries_or_deadline_overrun(self):
        facts = {
            "workflow_uid": "same",
            "workflow_uid_before": "same",
            "restart_run": {"status": "passed"},
            "native_phase": "Succeeded",
            "replica_restoration": {"replicas": 1, "ready_replicas": 1},
            "native_nodes": {str(i): {"type": "Pod"} for i in range(SOURCE_STEPS)},
            "outage_start": "2026-01-01T00:00:05Z", "outage_end": "2026-01-01T00:00:10Z",
            "source_checks": [{"uid": str(i), "waited_started_at": "2026-01-01T00:00:06Z",
                               "waited_container": "oberth-executor-token", "load_start": "2026-01-01T00:00:12Z",
                               "exact_source": True, "restart_counts": [0]} for i in range(SOURCE_STEPS)],
        }
        validate(facts, True)
        unobserved = deepcopy(facts)
        for check in unobserved["source_checks"]:
            check["waited_started_at"] = "2026-01-01T00:00:01Z"
        with self.assertRaisesRegex(Blocked, "No server-dependent"):
            validate(unobserved, True)
        failures = []
        failed = deepcopy(facts)
        failed["restart_run"]["status"] = "failed"
        failures.append(failed)
        wrong = deepcopy(facts)
        wrong["source_checks"][1]["exact_source"] = False
        failures.append(wrong)
        retry = deepcopy(facts)
        retry["native_nodes"]["retry"] = {"type": "Retry", "children": ["one", "two"]}
        failures.append(retry)
        restart_count = deepcopy(facts)
        restart_count["source_checks"][0]["restart_counts"] = [1]
        failures.append(restart_count)
        replaced = deepcopy(facts)
        replaced["workflow_uid"] = "another"
        failures.append(replaced)
        unrestored = deepcopy(facts)
        unrestored["replica_restoration"]["replicas"] = 0
        failures.append(unrestored)
        for value in failures:
            with self.subTest(value=value), self.assertRaises(Failure) as caught:
                validate(value, True)
            self.assertEqual(caught.exception.facts, value)
        with self.assertRaisesRegex(Failure, "deadline"):
            validate(facts, False)
        facts["native_nodes"]["wrapper"] = {"type": "Retry", "children": ["one"]}
        validate(facts, True)


if __name__ == "__main__":
    unittest.main()
