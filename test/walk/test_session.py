"""Offline contracts for command evidence, saved profiles and the step runner."""
import contextlib
import hashlib
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch
from walkassert import Blocked, Failure, HERE
from walkconfig import Environment
from walkhost import Runtime
from types import SimpleNamespace
from walklib import STEPS, Step, Walk, parser, ux_gate
import walkevidence
import walkruns
import walksetup


class SessionTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.env_file = self.root / "env.json"
        self.env_file.write_text((HERE / "env.example").read_text())
        self.config = Environment.load(self.env_file, "walk-1")
        self.args = parser().parse_args(["--out", str(self.root / "evidence"), "--env", str(self.env_file)])
        self.walk = Walk(
            self.args,
            self.config,
            Runtime(
                lambda: 100.0,
                lambda: "1" * 32,
                SimpleNamespace(busy=False, samples=[(0, 0, 0)], sample=lambda: (0, 0, 0)),
            ),
        )
        self.s = self.walk.s
        self.s.ux.baseline["profiles"]["reference"] = {"fresh": None, "warm": None}
        self.s.ux.baseline["samples"] = {"fresh": [], "warm": []}

    def fake_cli(self, source):
        self.s.binary.write_text("#!" + sys.executable + "\n" + source + "\n")
        self.s.binary.chmod(0o700)

    def test_failed_command_exports_command_exit_and_actual_redacted_product_message(self):
        self.s.current = "install"
        self.s.secrets.append("private-value")
        argv = [
            sys.executable,
            "-c",
            "import sys; print('Error: endpoint EOF private-value; run install --verify',file=sys.stderr);sys.exit(1)",
        ]
        with self.assertRaisesRegex(Failure, "endpoint EOF"):
            self.s.run(argv, display=["oberth", "install", "--yes"])
        evidence = self.s.public_commands()[0]
        self.assertEqual(evidence["command"], "oberth install --yes")
        self.assertEqual(evidence["exit_code"], 1)
        self.assertIn("endpoint EOF <private>", evidence["message"])
        self.assertNotIn("private-value", json.dumps(evidence))

    def test_remote_commands_use_saved_profile_without_connection_flags(self):
        self.s.ux.phase = "first_green"
        with patch.object(self.s, "run", return_value=(0, b"{}", b"")) as execute:
            self.s.user("runs")
            self.s.user("runs", agent=True)
        for call in execute.call_args_list:
            self.assertFalse({"--url", "--client-cert", "--client-key", "--ca-file"} & set(call.args[0]))
        self.assertNotEqual(
            execute.call_args_list[0].kwargs["env"]["HOME"], execute.call_args_list[1].kwargs["env"]["HOME"]
        )

    def test_operator_and_agent_bind_distinct_checkouts_once_without_remote_replacement(self):
        with patch.object(self.s, "uj", return_value={"configured": True}) as command:
            walksetup.bind(self.s)
            walksetup.bind(self.s, agent=True)
        first, second = command.call_args_list
        self.assertEqual(first.args[1]["repo"], self.s.admin_repo)
        self.assertEqual(second.args[1]["repo"], self.s.agent_repo)

    def test_agent_git_uses_its_own_checkout_and_saved_profile(self):
        with patch.object(self.s, "run") as execute:
            self.s.g("push", "oberth", "HEAD:refs/heads/walk/change", agent=True)
        self.assertIn(self.s.agent_repo, execute.call_args.args[0])
        self.assertEqual(execute.call_args.kwargs["env"]["HOME"], str(self.s.private / "agent-home"))

    def test_certificate_install_saves_admin_profile_before_upstream_followup(self):
        self.s.ux.phase = "first_green"
        profile = self.s.home / ".config/oberth"
        profile.mkdir(parents=True)
        (profile / "installer-ca.crt").write_text("public CA")
        installed = {"checks": [{"code": "upstream_key_pending", "ok": False}],
                     "ca_sha256": hashlib.sha256(b"ca").hexdigest(), "server_url": self.config.base}
        original = self.s.user
        def command(name, values=None, **kwargs):
            if name == "fingerprint":
                return 0, b"256 SHA256:public", b""
            if name == "install":
                return 3, json.dumps(installed).encode(), b""
            if name == "profile":
                return original(name, values, **kwargs)
            self.assertEqual(name, "upstream")
            self.assertIn("OBERTH_CLIENT_CERT", (profile / "env").read_text())
            return 0, b"{}", b""
        with patch.object(self.s, "user", side_effect=command), patch(
            "walksetup.ssl.PEM_cert_to_DER_cert", return_value=b"ca"
        ):
            self.assertTrue(walksetup.install(self.s)["documented_upstream_followup"])
        self.assertEqual((profile / "env").stat().st_mode & 0o777, 0o600)

    def test_typed_stdout_refusal_retains_reason_and_remedy_in_public_evidence(self):
        self.s.current = "install"
        body = {"code": "unavailable", "reason": "endpoint EOF", "remedy": "Run oberth install --verify"}
        script = "import sys; print(" + repr(json.dumps(body)) + "); sys.exit(1)"
        with self.assertRaisesRegex(Failure, "endpoint EOF"):
            self.s.run([sys.executable, "-c", script], display=["oberth", "install"])
        message = self.s.public_commands()[0]["message"]
        self.assertIn("unavailable", message)
        self.assertIn("Run oberth install --verify", message)

    def test_failed_run_message_is_public_even_when_the_wait_command_exits_zero(self):
        body = {"state": "finished", "run": {"status": "failed", "error": "source permission denied"}}
        script = "print(" + repr(json.dumps(body)) + ")"
        self.s.run([sys.executable, "-c", script], display=["oberth", "run", "wait", "run-id"])
        self.assertEqual(self.s.public_commands()[0]["exit_code"], 0)
        self.assertEqual(self.s.public_commands()[0]["message"], "source permission denied")

    def test_process_start_error_preserves_errno_and_failed_command(self):
        self.s.ux.phase = "first_green"
        with patch("walkio.subprocess.Popen", side_effect=OSError(11, "Resource temporarily unavailable")):
            with self.assertRaisesRegex(Failure, "Resource temporarily unavailable"):
                self.s.user("repo")
        self.assertEqual(self.s.public_commands()[0]["exit_code"], 127)
        self.assertEqual(len(self.s.ux.commands), 1)

    def test_timeout_stops_only_owned_process_group_and_exports_termination(self):
        self.s.ux.phase = "first_green"
        process = Mock(pid=123, returncode=-15)
        process.communicate.side_effect = subprocess.TimeoutExpired("oberth", 1)
        with patch("walkio.subprocess.Popen", return_value=process), patch("walkio.os.killpg") as stop:
            with self.assertRaises(Failure):
                self.s.user("repo")
        stop.assert_called_once()
        self.assertEqual(stop.call_args.args[0], 123)
        self.assertEqual(self.s.public_commands()[0]["exit_code"], -15)

    def test_mcp_protocol_errors_preserve_command_and_product_message(self):
        self.s.ux.phase = "first_green"
        self.fake_cli("import sys;print('identity denied; Run oberth client renew', file=sys.stderr);sys.exit(1)")
        with self.assertRaisesRegex(Failure, "identity denied"):
            self.s.mcp("uplink_list", {})
        command = self.s.public_commands()[-1]
        self.assertIn("oberth mcp call uplink_list", command["command"])
        self.assertEqual(command["exit_code"], 1)
        self.assertIn("Run oberth client renew", command["message"])

    def test_malformed_mcp_response_is_a_recorded_failure(self):
        self.s.ux.phase = "first_green"
        self.fake_cli("print('not JSON')")
        with self.assertRaisesRegex(Failure, "invalid reply"):
            self.s.mcp("uplink_list", {})
        self.assertTrue(self.s.public_commands()[-1]["product_failure"])
        self.assertEqual(self.s.public_commands()[-1]["message"], "not JSON")

    def test_mcp_failed_run_is_domain_data_and_preserves_the_failure_message(self):
        self.s.ux.phase = "checks"
        value = {"id": "failed-run", "status": "failed", "error": "main: Error (exit code 17)"}
        self.fake_cli("print(" + repr(json.dumps(value)) + ")")
        self.assertEqual(self.s.mcp("run_get", {"id": "failed-run"}), value)
        command = self.s.public_commands()[-1]
        self.assertTrue(command["product_failure"])
        self.assertEqual(command["message"], value["error"])

    def test_step_evidence_retains_the_product_message_even_with_a_useful_remedy(self):
        def broken(s):
            s.run(
                [
                    sys.executable,
                    "-c",
                    "import sys;print('Error: EOF; run oberth install --verify',file=sys.stderr);sys.exit(1)",
                ],
                display=["oberth", "install"],
            )
        with contextlib.redirect_stdout(io.StringIO()):
            self.walk.step(Step("install", "Endpoint ready", broken))
        result = json.loads((self.walk.out / "install.json").read_text())
        self.assertEqual(result["status"], "fail")
        self.assertIn("EOF", result["reason"])
        self.assertIn("oberth install --verify", result["commands"][0]["message"])
        self.assertEqual(self.s.ux.missing, [])

    def test_preflight_blocking_skips_vm_commands_but_cleanup_still_runs(self):
        calls = []
        specs = (Step("preflight", "Environment ready", lambda s: (_ for _ in ()).throw(Blocked("slot busy"))),
                 Step("target", "Target", lambda s: calls.append("target"), ("preflight",)),
                 Step("cleanup", "Remove owned names", lambda s: calls.append("cleanup")))
        with patch("walklib.STEPS", specs), patch.object(self.s, "run", side_effect=AssertionError("VM forbidden")):
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(self.walk.execute(), 2)
        self.assertEqual(calls, ["cleanup"])
        self.assertFalse(self.s.private.exists())

    def test_final_evidence_scan_sees_the_final_result_and_summary(self):
        observed = []
        def scan(out, secrets):
            result = json.loads((out / "result.json").read_text())
            observed.append(result["steps"][-1]["name"])
            self.assertIn("complete", (out / "summary.txt").read_text())
        specs = (Step("complete", "Final assertion", lambda s: {}), Step("cleanup", "Cleanup", lambda s: {}))
        with patch("walklib.STEPS", specs), patch("walklib.evidence.scan", side_effect=scan):
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(self.walk.execute(), 0)
        self.assertEqual(observed, ["cleanup"])

    def test_lifecycle_failure_still_runs_remote_and_local_cleanup(self):
        cleaned = []
        specs = (Step("preflight", "Ready", lambda s: {}),
                 Step("cleanup", "Owned names removed", lambda s: cleaned.append(True)))
        original, first = self.walk.flush, [True]
        def flush():
            if first[0]:
                first[0] = False
                raise OSError("evidence publication unavailable")
            return original()
        with patch("walklib.STEPS", specs), patch.object(self.walk, "flush", side_effect=flush):
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(self.walk.execute(), 1)
        self.assertEqual(cleaned, [True])
        self.assertFalse(self.s.private.exists())

    def test_secret_introduced_only_by_final_flush_is_refused(self):
        self.s.secrets.append("private-evidence-value")
        specs = (Step("complete", "Final assertion", lambda s: {}), Step("cleanup", "Cleanup", lambda s: {}))
        original = self.walk.flush
        def leaking_flush():
            result = original()
            (self.walk.out / "late.txt").write_text("private-evidence-value")
            return result
        with patch("walklib.STEPS", specs), patch.object(self.walk, "flush", side_effect=leaking_flush):
            with contextlib.redirect_stdout(io.StringIO()):
                self.assertEqual(self.walk.execute(), 1)
        self.assertEqual(json.loads((self.walk.out / "result.json").read_text())["verdict"], "FAIL")
        self.assertFalse((self.walk.out / "late.txt").exists())
        walkevidence.scan(self.walk.out, self.s.secrets)

    def test_slot_two_ports_context_and_kubeconfig_are_derived(self):
        second = Environment.load(self.env_file, "walk-2")
        self.assertEqual(second.context, "k3d-walk-2")
        self.assertEqual(second.ports, {"api": 6552, "ssh": 35022, "https": 35443})
        self.assertEqual(second.kube.name, "walk-2.kubeconfig")
        with patch.object(self.s, "run") as execute:
            self.s.config = second
            self.s.k("get", "pods")
        self.assertIn("k3d-walk-2", execute.call_args.args[0])
        self.assertNotIn("k3d-walk-1", execute.call_args.args[0])

    def test_restart_resume_patch_is_literal_json_and_not_a_template_field(self):
        with patch.object(self.s, "run", return_value=(0, b"{}", b"")) as execute:
            self.s.user("workflow-resume", {"workflow": "owned-workflow"})
        argv = execute.call_args.args[0]
        self.assertEqual(json.loads(argv[-1]), {"spec": {"suspend": False}})

    def test_product_wait_replaces_fixed_sleep_polling(self):
        with patch.object(self.s, "uj", return_value={"state": "finished"}) as command:
            with patch("walkruns.detail", return_value={"run": {"status": "passed"}}):
                walkruns.wait(self.s, "run-id")
        self.assertEqual(command.call_args.args, ("wait", {"id": "run-id"}))
        self.assertEqual(self.s.ux.events[0]["kind"], "wait")

    def test_live_logs_share_one_running_observation_for_both_transports(self):
        self.s.data.sha = "exact"
        def follow(identifier, running):
            return running(), {"message": "observed"}
        with patch.object(self.s, "follow", side_effect=follow), \
                patch("walkruns.running_pods", side_effect=[["native-pod"], []]) as pods, \
                patch.object(self.s, "mcp", return_value={"output": "walk: live exact"}):
            self.assertTrue(walkruns.live_logs(self.s)["both_transports_active"])
        pods.assert_called_once()

    def test_live_logs_reject_agent_output_that_arrives_only_after_retention(self):
        self.s.data.sha = "exact"
        active = [True]
        def follow(identifier, running):
            return running(), {"message": "observed"}
        def retained(*args):
            active[0] = False
            return {"output": "walk: live exact"}
        with patch.object(self.s, "follow", side_effect=follow), \
                patch("walkruns.running_pods", side_effect=lambda s: ["native-pod"] if active[0] else []), \
                patch.object(self.s, "mcp", side_effect=retained), \
                self.assertRaisesRegex(Failure, "Agent output was not observed"):
            walkruns.live_logs(self.s)

    def test_failed_readonly_run_attaches_its_actual_probe_log(self):
        self.s.data.sha = "base"
        self.s.data.documents = {"readonly": {}}
        self.s.agent_repo.mkdir()
        (self.s.agent_repo / ".oberth").mkdir()
        link = ("watch: " + self.config.base + "/runs/" + "a" * 32).encode()
        with patch.object(self.s, "g", return_value=(0, link, b"")), patch(
            "walkruns.wait", return_value={"run": {"id": "a" * 32, "status": "failed", "error": "main failed"}}
        ), patch("walkruns.retained", return_value="AssertionError: source accepted a write"), patch(
            "walkruns.source_queries", return_value=(True, True)
        ), patch.object(
            self.s, "user", return_value=(0, link, b"")
        ):
            with self.assertRaises(Failure) as caught:
                walkruns.negative(self.s, "readonly")
        self.assertIn("source accepted a write", caught.exception.facts["probe_log"])

    def test_push_link_must_resolve_to_this_exact_source_and_branch(self):
        self.s.data.sha = "exact"
        message = ("remote: watch: " + self.config.base + "/runs/" + "a" * 32).encode()
        wrong = {"run": {"id": "a" * 32, "sha": "other", "ref": "walk/change", "release": False}}
        with patch.object(self.s, "user", return_value=(0, b"", message)), patch("walkruns.detail", return_value=wrong):
            with self.assertRaisesRegex(Failure, "sha differs"):
                walkruns.push(self.s)

    def test_immediate_refusal_still_fails_if_a_run_exists(self):
        self.s.data.sha = "exact"
        error = {
            "code": "invalid_input",
            "reason": ".oberth/build.yaml offender-one walkUnknownOne offender-two walkUnknownTwo",
            "remedy": "remove unknown fields",
        }
        with patch("walkruns.source_queries", return_value=(True, False)):
            with self.assertRaisesRegex(Failure, "durable run"):
                walkruns.refused_push(self.s, "invalid", "walk/invalid", "exact", 1, json.dumps(error))

    def test_invalid_field_refusal_must_name_fields_as_well_as_templates(self):
        error = {"code": "invalid_input", "reason": ".oberth/build.yaml offender-one offender-two",
                 "remedy": "remove unknown fields"}
        with patch("walkruns.source_queries") as queries, self.assertRaisesRegex(Failure, "exact scope"):
            walkruns.refused_push(self.s, "invalid", "walk/invalid", "exact", 1, json.dumps(error))
        queries.assert_not_called()

    def test_immediate_refusal_still_fails_if_native_workflow_exists(self):
        error = {
            "code": "invalid_input",
            "reason": ".oberth/build.yaml offender-one walkUnknownOne offender-two walkUnknownTwo",
            "remedy": "remove unknown fields",
        }
        native = {"items": [{"spec": {"arguments": {"parameters": [{"name": "oberth-sha", "value": "exact"}]}}}]}
        with patch("walkruns.source_queries", return_value=(False, True)):
            with self.assertRaisesRegex(Failure, "native Workflow"):
                walkruns.refused_push(self.s, "invalid", "walk/invalid", "exact", 1, json.dumps(error))

    def test_accepted_negative_push_remains_failure_with_later_refusal_attached(self):
        message = "remote: watch: " + self.config.base + "/runs/" + "a" * 32
        later = {"run": {"status": "failed", "error": "refused later"}}
        with patch("walkruns.wait", return_value=later), patch.object(
            self.s, "http", return_value=b'{"containers":[]}'
        ):
            with self.assertRaises(Failure) as refused:
                walkruns.refused_push(self.s, "ungranted", "walk/denied", "exact", 0, message)
        self.assertEqual(refused.exception.facts["accepted_run"], later)

    def test_release_distinguishes_annotated_tag_object_from_tested_commit(self):
        s = self.s
        s.data.release_sha, s.data.tag = "tested-commit", "v0.0.0-walk-test"
        identifier = "a" * 32
        run = {"id": identifier, "release": True, "ref": s.data.tag, "sha": "tag-object",
               "tested_sha": "tested-commit", "status": "passed"}
        push = ("watch: " + s.config.base + "/runs/" + identifier).encode()
        for field, value, error in (
            ("sha", "wrong-tag", "sha differs"),
            ("tested_sha", "wrong-source", "tested_sha differs"),
            ("tested_sha", "tested-commit", "Release proof is missing"),
        ):
            with self.subTest(field=field, value=value), patch.object(
                s, "user", side_effect=[(0, b"", b""), (0, push, b"")]
            ), patch.object(s, "g", return_value=(0, b"tag-object", b"")), patch(
                "walkruns.wait", return_value={"run": {**run, field: value}}
            ), patch(
                "walkruns.retained", return_value=""
            ):
                with self.assertRaisesRegex(Failure, error):
                    walkruns.release(s)

    def test_promotion_rejects_green_wrong_source_even_when_forge_matches_product(self):
        self.s.data.sha = "tested"
        promotion = {"id": "promotion", "status": "passed", "result_sha": "wrong", "run_id": "other-green"}
        with patch.object(self.s, "uj", return_value={"id": "promotion"}), \
                patch.object(self.s, "mcp", return_value={"promotion": promotion, "still_running": False}), \
                patch("walkruns.forge_ref", return_value="wrong"), patch.object(self.s, "user"), \
                patch.object(self.s, "g", return_value=(1, b"", b"")):
            with self.assertRaisesRegex(Failure, "does not contain the exact tested"):
                walkruns.promote(self.s)

    def test_bad_signed_cli_checksum_stops_before_any_cli_execution(self):
        def fetch(argv, **kwargs):
            if argv[0] == "curl":
                path = argv[argv.index("--output") + 1]
                body = "0" * 64 + "  oberth-linux-amd64\n" if path.name == "SHA256SUMS" else "changed CLI"
                path.write_text(body)
            return 0, b"", b""
        with patch.object(self.s, "run", side_effect=fetch), patch.object(self.s, "user") as cli:
            with self.assertRaisesRegex(Failure, "signed checksum"):
                walksetup.target(self.s)
        cli.assert_not_called()
        self.assertFalse(self.s.binary.exists())

    def test_signature_verification_is_mandatory_even_with_correct_cli_bytes(self):
        import hashlib
        def fetch(argv, **kwargs):
            if argv[0] == "curl":
                path = argv[argv.index("--output") + 1]
                body = (
                    hashlib.sha256(b"CLI").hexdigest() + "  oberth-linux-amd64\n"
                    if path.name == "SHA256SUMS"
                    else "CLI"
                )
                path.write_text(body)
                return 0, b"", b""
            raise Failure("signature refused")
        with patch.object(self.s, "run", side_effect=fetch), patch.object(self.s, "user") as cli:
            with self.assertRaisesRegex(Failure, "signature refused"):
                walksetup.target(self.s)
        cli.assert_not_called()

    def test_baseline_recording_refuses_any_failed_functional_assertion(self):
        self.s.args.record_baseline = True
        self.s.data.cli_sha256 = self.s.ux.baseline["reference"]["cli_sha256"]
        self.s.results.extend({"name": spec.name, "status": "fail" if spec.name == "readonly-source" else "pass"}
                              for spec in STEPS if spec.name != "ux-gate")
        with self.assertRaisesRegex(Failure, "Unapproved failure"):
            ux_gate(self.s)

    def test_unmeasured_baseline_blocks_even_with_complete_milestone_clocks(self):
        self.s.ux.mark("cluster_ready")
        self.s.ux.mark("first_green")
        self.s.ux.mark("first_release")
        with patch("walklib.approved", return_value=set()), self.assertRaises(Blocked):
            ux_gate(self.s)

    def test_first_reference_sample_is_staged_without_publishing_a_one_sample_budget(self):
        self.s.ux.mark("cluster_ready")
        self.s.ux.mark("first_green")
        self.s.ux.mark("first_release")
        self.s.args.record_baseline = True
        self.s.data.cli_sha256 = self.s.ux.baseline["reference"]["cli_sha256"]
        self.s.results.extend({"name": spec.name, "status": "pass"} for spec in STEPS if spec.name != "ux-gate")
        ux_gate(self.s)
        self.assertIsNone(self.s.pending_baseline["profiles"]["reference"]["fresh"])
        self.assertEqual(len(self.s.pending_baseline["samples"]["fresh"]), 1)

    def test_calibration_is_persisted_only_after_final_evidence_scan(self):
        self.args.record_baseline = True
        self.s.data.cli_sha256 = self.s.ux.baseline["reference"]["cli_sha256"]
        specs = (Step("cluster", "Ready", lambda s: {}, milestone="cluster_ready"),
                 Step("build", "Green", lambda s: {}, milestone="first_green"),
                 Step("release", "Released", lambda s: {}, milestone="first_release"),
                 Step("cleanup", "Clean", lambda s: {}), Step("ux-gate", "Budget", ux_gate))
        original_scan = walkevidence.scan
        def scan(out, secrets):
            self.assertFalse((self.root / "golden-path.json").exists())
            original_scan(out, secrets)
        with patch("walklib.STEPS", specs), patch("walklib.HERE", self.root), \
                patch("walklib.evidence.scan", side_effect=scan), contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(self.walk.execute(), 0)
        self.assertTrue((self.root / "golden-path.json").exists())
        self.assertFalse(self.s.private.exists())

    def test_offline_help_and_plan_need_no_environment_or_credentials(self):
        for flag in ("--help", "--plan"):
            completed = subprocess.run([str(HERE / "walk"), flag], capture_output=True)
            self.assertEqual(completed.returncode, 0)
        self.assertNotIn("local", parser().format_help())


if __name__ == "__main__":
    unittest.main()
