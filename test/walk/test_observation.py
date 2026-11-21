"""Controls for streaming, declared command costs, host load and final evidence scans."""
import contextlib
import io
import json
from pathlib import Path
import tempfile
import sys
from types import SimpleNamespace
from copy import deepcopy
import unittest
from unittest.mock import Mock, patch
from walkassert import Blocked, Failure, HERE
from walkcommands import COMMANDS, STORE_COMMANDS, flags
from walkconfig import Environment
from walkhost import Runtime, Load
from walklib import Walk, Step, parser, ux_gate
from walkreference import record_sample
from walkux import COUNTS
import walkevidence
import walksetup
from workflows import fixtures
from walkruns import admission_control, failing_build, running_pods, short_failure_reason, source_queries


class ObservationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        env = self.root / "env.json"
        env.write_text((HERE / "env.example").read_text())
        self.args = parser().parse_args(["--env", str(env), "--out", str(self.root / "evidence")])
        self.load = SimpleNamespace(busy=False, samples=[(0, 0, 0)], sample=lambda: (0, 0, 0))
        self.walk = Walk(self.args, Environment.load(env, "walk-1"), Runtime(lambda: 100, lambda: "1" * 32, self.load))
        self.s = self.walk.s
        self.s.ux.phase = "first_green"
        self.s.ux.baseline["samples"] = {"fresh": [], "warm": []}
        self.s.ux.baseline["profiles"]["reference"] = {"fresh": None, "warm": None}
        self.s.data.sha = "a" * 40

    def test_follow_waits_for_delayed_complete_line_and_checks_pod_then(self):
        process = Mock(pid=123, returncode=-15)
        process.poll.return_value = None
        process.stdout.fileno.return_value = 42
        selector = Mock()
        selector.__enter__ = Mock(return_value=selector)
        selector.__exit__ = Mock(return_value=None)
        marker = ("walk: live " + self.s.data.sha + "\n").encode()
        chunks = [b"walk: live ", marker[len(b"walk: live "):]]
        received = []
        running = Mock(side_effect=lambda: len(received) == 2)
        def read(*_):
            self.assertFalse(running.called)
            data = chunks[len(received)]
            received.append(data)
            return data
        selector.select.return_value = [(object(), 1)]
        with patch("walkstream.subprocess.Popen", return_value=process) as launch, \
                patch("walkstream.selectors.DefaultSelector", return_value=selector), \
                patch("walkstream.os.read", side_effect=read), patch("walkstream.os.killpg") as kill:
            observed, _ = self.s.follow("b" * 32, running)
        self.assertTrue(observed)
        self.assertEqual(launch.call_args.args[0][1:], ["log", "b" * 32])
        running.assert_called_once()
        self.assertEqual(kill.call_args.args[0], 123)
        self.assertEqual(len(self.s.ux.commands), 1)

    def test_failed_build_views_must_supply_the_exact_working_command(self):
        identifier, sha = "b" * 32, "a" * 40
        hint = {"action": "oberth log " + identifier}
        view = {"run": {"id": identifier, "sha": sha, "status": "failed", "failed_step": "failing",
                        "reason": "command exited 17"}, "retry_hint": hint}
        marker = "walk: deliberate failure " + sha
        with patch.object(self.s, "uj", return_value=view), patch.object(self.s, "mcp", return_value=view), patch(
            "walkruns.retained", return_value=marker
        ), patch.object(self.s, "user", return_value=(0, marker.encode(), b"")):
            self.assertEqual(failing_build(self.s, identifier, sha)["exact_log_command"], hint["action"])
        wrong = {**view, "retry_hint": {"action": "oberth log " + "c" * 32}}
        with patch.object(self.s, "uj", return_value=wrong), patch.object(self.s, "mcp", return_value=view), patch(
            "walkruns.retained", return_value=marker
        ), self.assertRaisesRegex(Failure, "run wait, run view") as refused:
            failing_build(self.s, identifier, sha)
        self.assertEqual(refused.exception.facts["views"]["run view"], wrong)

    def test_failed_views_require_step_reason_and_command_in_each_transport(self):
        identifier, sha = "b" * 32, "a" * 40
        marker = "walk: deliberate failure " + sha
        view = {"run": {"id": identifier, "sha": sha, "status": "failed", "failed_step": "failing",
                        "reason": "command exited 17"}, "retry_hint": {"action": "oberth log " + identifier}}
        for index in range(3):
            for field in ("failed_step", "reason", "command", "long_reason"):
                values = [deepcopy(view) for _ in range(3)]
                if field == "command":
                    values[index].pop("retry_hint")
                elif field == "long_reason":
                    values[index]["run"]["reason"] = "error " + "x" * 50
                else:
                    values[index]["run"].pop(field)
                with self.subTest(transport=index, field=field), \
                        patch.object(self.s, "uj", side_effect=values[:2]), \
                        patch.object(self.s, "mcp", return_value=values[2]), \
                        patch("walkruns.retained", return_value=marker), self.assertRaises(Failure):
                    failing_build(self.s, identifier, sha)

    def test_failed_views_accept_and_execute_the_printed_command_without_a_schema_field(self):
        identifier, sha = "b" * 32, "a" * 40
        command = "oberth log " + identifier[:8] + " --step failing"
        view = {"run": {"id": identifier, "sha": sha, "status": "failed"},
                "display": "failing\ncommand exited 17\nNext: " + command}
        marker = "walk: deliberate failure " + sha
        with patch.object(self.s, "uj", return_value=view), patch.object(self.s, "mcp", return_value=view), \
                patch("walkruns.retained", return_value=marker), \
                patch.object(self.s, "user", return_value=(0, marker.encode(), b"")) as execute:
            failing_build(self.s, identifier, sha)
        execute.assert_called_once_with("log-command", {"selector": identifier[:8], "options": ["--step", "failing"]})

    def test_printed_failure_log_command_must_return_the_failed_source_marker(self):
        identifier, sha = "b" * 32, "a" * 40
        view = {"run": {"id": identifier, "sha": sha, "status": "failed", "failed_step": "failing",
                        "reason": "command exited 17"}, "hint": "oberth log " + identifier}
        with patch.object(self.s, "uj", return_value=view), patch.object(self.s, "mcp", return_value=view), \
                patch("walkruns.retained", return_value="walk: deliberate failure " + sha), \
                patch.object(self.s, "user", return_value=(0, b"another step", b"")), \
                self.assertRaisesRegex(Failure, "Printed log command"):
            failing_build(self.s, identifier, sha)

    def test_failure_reason_is_a_phrase_with_a_forty_character_bound(self):
        reason = "exit 17 " + "x" * 32
        self.assertTrue(short_failure_reason(reason))
        self.assertFalse(short_failure_reason(reason + "x"))
        self.assertFalse(short_failure_reason(""))

    def test_positive_queries_find_exact_run_and_native_workflow(self):
        rows = {"runs": [{"sha": "exact", "ref": "walk/readonly"}]}
        native = {"items": [{"spec": {"arguments": {"parameters": [{"name": "oberth-sha", "value": "exact"}]}}}]}
        with patch.object(self.s, "http", return_value=json.dumps(rows).encode()), \
                patch.object(self.s, "k", return_value=(0, json.dumps(native).encode(), b"")):
            self.assertEqual(source_queries(self.s, "walk/readonly", "exact"), (True, True))
            self.assertEqual(source_queries(self.s, "walk/readonly", "other"), (False, False))
        with self.assertRaisesRegex(Failure, "positive control"):
            admission_control(self.s)

    def test_invalid_fixture_uses_unknown_fields_in_ordinary_container_templates(self):
        invalid = fixtures("image", "marker", "registry:5000", "owned")["invalid"]
        leaves = invalid["spec"]["templates"][1:]
        self.assertEqual([t["name"] for t in leaves], ["offender-one", "offender-two"])
        self.assertTrue(all("container" in t and "http" not in t for t in leaves))
        self.assertTrue(leaves[0]["walkUnknownOne"] and leaves[1]["walkUnknownTwo"])

    def test_optimized_probe_keeps_the_default_root_check(self):
        source = (HERE / "fixture/check-readonly.py").read_text()
        with patch("os.geteuid", return_value=1000), self.assertRaisesRegex(AssertionError, "default root user"):
            exec(compile(source, "check-readonly.py", "exec", optimize=2), {"__name__": "__main__"})

    def test_harness_output_cannot_satisfy_a_failed_product_command_remedy(self):
        self.s.binary.write_text(
            "#!" + sys.executable + "\nimport sys;print('endpoint EOF', file=sys.stderr);sys.exit(1)\n"
        )
        self.s.binary.chmod(0o700)
        def broken(s):
            try:
                s.user("repo")
            except Failure:
                s.run([sys.executable, "-c", "print('oberth install --verify')"])
                raise
        with contextlib.redirect_stdout(io.StringIO()):
            self.walk.step(Step("install", "Install", broken, phase="first_green"))
        self.assertEqual(self.s.ux.missing[0]["step"], "install")

    def test_passing_product_data_with_a_failed_harness_assertion_adds_no_message_failure(self):
        self.s.binary.write_text("#!" + sys.executable + "\nprint('{}')\n")
        self.s.binary.chmod(0o700)
        def broken(s):
            s.user("repo")
            raise Failure("Observed data differs from the expected value")
        with contextlib.redirect_stdout(io.StringIO()):
            self.walk.step(Step("install", "Install", broken, phase="first_green"))
        self.assertEqual(self.s.ux.missing, [])

    def test_arbitrary_explicit_docker_context_is_configuration(self):
        values = json.loads(self.args.env.read_text())
        values["docker_context"] = "provided-local-context"
        path = self.root / "arbitrary-context.json"
        path.write_text(json.dumps(values))
        self.assertEqual(Environment.load(path, "walk-1")["docker_context"], "provided-local-context")

    def test_running_confirmation_excludes_a_completed_main_with_live_sidecar(self):
        pod = {"metadata": {"name": "native-pod"}, "status": {"phase": "Running", "containerStatuses": [
            {"name": "main", "state": {"terminated": {"exitCode": 0}}}, {"name": "wait", "state": {"running": {}}}]}}
        with patch.object(self.s, "k", return_value=(0, json.dumps({"items": [pod]}).encode(), b"")):
            self.assertEqual(running_pods(self.s), [])
        pod["status"]["containerStatuses"][0]["state"] = {"running": {}}
        with patch.object(self.s, "k", return_value=(0, json.dumps({"items": [pod]}).encode(), b"")):
            self.assertEqual(running_pods(self.s), ["native-pod"])

    def test_mcp_argument_keys_are_flags(self):
        argv = [
            "oberth",
            "mcp",
            "call",
            "access_allow",
            json.dumps({"repo": "r", "workflow": "w", "step": "s", "secret": "p"}),
        ]
        self.assertEqual(len(flags(argv)), 4)

    def test_infrastructure_cannot_hide_user_setup_or_product_commands(self):
        self.s.ux.phase = ""
        for name, values in (("key", {"key": "admin-push"}), ("repo", {})):
            with self.subTest(name=name), self.assertRaisesRegex(Failure, "Infrastructure"):
                self.s.user(name, values)
        with self.assertRaisesRegex(Failure, "declared command table"):
            self.s.run([self.s.binary, "repo", "add", "r"])
        for name, argv in COMMANDS.items():
            if name.startswith("git-"):
                with self.subTest(name=name), self.assertRaisesRegex(Failure, "declared command table"):
                    self.s.check_operation(list(argv.argv), None)

    def test_declared_scripts_count_each_inner_command_and_duration_once(self):
        stem = self.s.private / "command-test"
        stem.with_suffix(".stdout").write_text("")
        stem.with_suffix(".stderr").write_text("")
        self.s.record({"step": "grant", "argv": list(COMMANDS["store"].argv), "exit_code": 0,
                       "operation": "store", "duration_seconds": 5, "capture": stem.name})
        self.assertEqual(len(self.s.ux.commands), len(STORE_COMMANDS) + 1)
        self.assertEqual(sum(c["duration_seconds"] for c in self.s.ux.commands), 5)
        self.assertTrue(all(c.argv[0] not in ("sh", "bash") for c in COMMANDS.values()))

    def test_load_above_guard_blocks_before_environment_commands(self):
        self.load.sample = lambda: (16.1, 0, 0)
        with patch.object(self.s, "d") as docker, self.assertRaisesRegex(Blocked, "1-minute load"):
            walksetup.preflight(self.s)
        docker.assert_not_called()

    def test_load_monitor_remembers_a_busy_five_minute_sample(self):
        values = iter(((1, 1, 1), (1, 12.1, 1), (1, 1, 1)))
        load = Load(lambda: next(values))
        for _ in range(3):
            load.sample()
        self.assertTrue(load.busy)

    def test_busy_host_result_is_inconclusive_and_cannot_persist_calibration(self):
        self.load.busy = True
        self.s.pending_baseline = {"should": "not persist"}
        specs = (Step("complete", "Complete", lambda s: {}), Step("cleanup", "Clean", lambda s: {}))
        with patch("walklib.STEPS", specs), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(self.walk.execute(), 2)
        self.assertEqual(json.loads((self.walk.out / "result.json").read_text())["verdict"], "INCONCLUSIVE")

    def test_failed_step_on_busy_host_still_exits_one(self):
        self.load.busy = True
        def broken(s):
            raise Failure("delivered source is writable")
        specs = (Step("broken", "Functional check", broken), Step("cleanup", "Clean", lambda s: {}))
        with patch("walklib.STEPS", specs), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(self.walk.execute(), 1)
        self.assertEqual(json.loads((self.walk.out / "result.json").read_text())["verdict"], "FAIL")

    def complete_budget(self, commands, seconds):
        counts = {key: 0 for key in COUNTS}
        profile = {name: {"complete": True, "counts": dict(counts), "seconds": 1}
                   for name in ("first_green", "first_release")}
        self.s.ux.baseline["profiles"]["reference"]["fresh"] = profile
        self.s.ux.completed = {name: {"complete": True, "counts": {**counts, "commands": commands},
                                     "seconds": seconds} for name in profile}
        self.load.busy = True

    def test_count_regression_on_busy_host_still_exits_one(self):
        limit = self.s.ux.baseline["candidate_target"]["first_green"]["commands"]
        self.complete_budget(limit + 1, 2)
        specs = (Step("cleanup", "Clean", lambda s: {}), Step("ux-gate", "Budget", ux_gate))
        with patch("walklib.STEPS", specs), patch("walklib.approved", return_value=set()), \
                contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(self.walk.execute(), 1)
        result = json.loads((self.walk.out / "result.json").read_text())
        self.assertEqual(result["verdict"], "FAIL")
        self.assertIn(f"first_green.commands: {limit + 1} > {limit}", result["steps"][-1]["reason"])

    def test_only_time_overrun_on_busy_host_is_inconclusive(self):
        self.complete_budget(0, 2)
        specs = (Step("cleanup", "Clean", lambda s: {}), Step("ux-gate", "Budget", ux_gate))
        with patch("walklib.STEPS", specs), patch("walklib.approved", return_value=set()), \
                contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(self.walk.execute(), 2)
        result = json.loads((self.walk.out / "result.json").read_text())
        self.assertEqual(result["verdict"], "INCONCLUSIVE")
        self.assertEqual(len(result["steps"][-1]["facts"]["time_overruns"]), 2)

    def test_secret_scan_failure_on_busy_host_still_exits_one(self):
        self.load.busy = True
        self.s.secrets.append("private-value-in-final-evidence")
        specs = (Step("cleanup", "Clean", lambda s: {}),)
        original = self.walk.flush
        def leaking():
            value = original()
            (self.walk.out / "late.txt").write_text(self.s.secrets[0])
            return value
        with patch("walklib.STEPS", specs), patch.object(self.walk, "flush", side_effect=leaking), \
                contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(self.walk.execute(), 1)
        self.assertEqual(json.loads((self.walk.out / "result.json").read_text())["verdict"], "FAIL")

    def test_mutating_cluster_and_http_probes_cannot_bypass_the_ledger(self):
        for argv in (("exec", "pod", "--", "oberth", "repo", "add"), ("delete", "namespace", "oberth")):
            with self.subTest(argv=argv), self.assertRaisesRegex(Failure, "read-only"):
                self.s.k(*argv)
            with self.assertRaisesRegex(Failure, "declared command table"):
                self.s.check_operation(["kubectl", *argv], None)
        with self.assertRaisesRegex(Failure, "read-only"):
            self.s.http("/mcp", {"method": "tools/call"})
        with self.assertRaisesRegex(Failure, "outside the product journey"):
            self.s.d("exec", "pod", "oberth", "repo", "add")

    def test_missing_baseline_number_names_milestone_and_still_scans_final_bytes(self):
        counts = {key: 0 for key in COUNTS}
        milestone = {"complete": True, "counts": counts, "seconds": 1}
        self.s.ux.baseline["profiles"]["reference"]["fresh"] = {
            "first_green": milestone, "first_release": {**milestone, "seconds": None}}
        specs = (Step("cluster", "Ready", lambda s: {}, milestone="cluster_ready"),
                 Step("build", "Green", lambda s: {}, milestone="first_green"),
                 Step("release", "Published", lambda s: {}, milestone="first_release"),
                 Step("cleanup", "Clean", lambda s: {}), Step("ux-gate", "Budget", ux_gate))
        with patch("walklib.STEPS", specs), patch("walklib.evidence.scan", wraps=walkevidence.scan) as scan, \
                contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(self.walk.execute(), 1)
        scan.assert_called_once()
        result = json.loads((self.walk.out / "result.json").read_text())
        self.assertIn("first_release.seconds: baseline is missing", result["steps"][-1]["reason"])
        self.assertFalse(self.s.private.exists())

    def test_final_flush_fault_always_reaches_secret_scan(self):
        specs = (Step("cleanup", "Clean", lambda s: {}),)
        with patch("walklib.STEPS", specs), patch.object(
            self.walk, "flush", side_effect=ValueError("broken publication")
        ), patch("walklib.evidence.scan", wraps=walkevidence.scan) as scan, contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(self.walk.execute(), 1)
        scan.assert_called_once()

    def test_escaped_multiline_and_nested_secret_fragments_are_found(self):
        secret = 'value-with-quote-"-and-backslash-\\-é'
        nested = self.walk.out / "nested"
        nested.mkdir()
        file = nested / "data.json"
        file.write_text(json.dumps({"value": secret}))
        self.assertEqual(walkevidence.unsafe_evidence(self.walk.out, [secret]), [file])
        file.write_text(json.dumps({"value": "a-long-secret-line-over-sixteen"}))
        self.assertEqual(
            walkevidence.unsafe_evidence(self.walk.out, ["prefix\na-long-secret-line-over-sixteen\nsuffix"]), [file]
        )

    def test_two_samples_choose_faster_milestones_with_their_actual_counts(self):
        first = {"complete": True, "counts": {key: 1 for key in COUNTS}, "seconds": 10}
        slow = {**first, "seconds": 20}
        baseline = self.s.ux.baseline
        sample = {
            "mode": "fresh",
            "release": "v0.23.4",
            "evidence": "first",
            "first_green": first,
            "first_release": slow,
        }
        baseline = record_sample(baseline, sample)
        self.assertIsNone(baseline["profiles"]["reference"]["fresh"])
        fast_release = {**first, "counts": {**first["counts"], "commands": 3}}
        sample = {**sample, "evidence": "second", "first_green": slow, "first_release": fast_release}
        baseline = record_sample(baseline, sample)
        profile = baseline["profiles"]["reference"]["fresh"]
        self.assertEqual(profile["first_green"]["seconds"], 10)
        self.assertEqual(profile["first_release"]["seconds"], 10)
        self.assertEqual(profile["first_release"]["counts"]["commands"], 3)


if __name__ == "__main__":
    unittest.main()
