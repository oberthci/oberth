"""Refusals and outside-in evidence must reject plausible broken products."""
import errno
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import Mock, patch
from walkassert import Failure, SECRET, build_log, pushed_run, refusal, release_record, ui_data
from workflows import fixtures

spec = importlib.util.spec_from_file_location("readonly_probe", Path(__file__).parent / "fixture/check-readonly.py")
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


class ContractTests(unittest.TestCase):
    def test_store_only_release_declares_only_consumed_scratch(self):
        leaf = fixtures("tools", "marker", "registry:5000", "owned")["release"]["spec"]["templates"][0]
        self.assertEqual([volume["name"] for volume in leaf["volumes"]], ["private-files"])
        self.assertEqual(leaf["container"]["volumeMounts"],
                         [{"name": "private-files", "mountPath": "/run/private-files"}])
        self.assertFalse(any(item["name"].startswith("VAULT_") for item in leaf["container"]["env"]))
        self.assertEqual(leaf["container"]["command"][:3], ["oberth", "secretstore", "exec"])

    def test_writable_source_fails_the_actual_probe(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / ".git").mkdir()
            (root / "source-marker").write_text("marker")
            with self.assertRaisesRegex(AssertionError, "accepted a write"):
                probe.verify(root)

    def test_readonly_source_requires_denied_writes_and_denied_chmod(self):
        with patch.object(probe.os, "statvfs", return_value=Mock(f_flag=0)), \
                patch.object(Path, "open", side_effect=OSError(errno.EROFS, "read-only")):
            with patch.object(Path, "chmod", side_effect=OSError(errno.EROFS, "read-only")):
                probe.verify(Path("source"))
            with patch.object(Path, "chmod"):
                with self.assertRaisesRegex(AssertionError, "change source permissions"):
                    probe.verify(Path("source"))

    def test_unrelated_filesystem_failure_does_not_prove_readonly_delivery(self):
        with patch.object(probe.os, "statvfs", return_value=Mock(f_flag=0)), \
                patch.object(Path, "open", side_effect=OSError(errno.ENOENT, "missing")):
            with self.assertRaisesRegex(AssertionError, "other than denied"):
                probe.verify(Path("source"))

    def test_all_fixture_templates_are_ordinary_yaml_without_host_paths_or_security_overrides(self):
        documents = fixtures("tools", "marker", "registry:5000", "owned")
        for phase, document in documents.items():
            for template in document["spec"]["templates"]:
                with self.subTest(phase=phase, template=template["name"]):
                    self.assertNotIn("securityContext", template)
                    self.assertNotIn("resources", template.get("container", {}))
                    self.assertNotIn("securityContext", template.get("container", {}))
                    self.assertTrue(all("hostPath" not in volume for volume in template.get("volumes", [])))
                    if "container" in template and phase in ("build", "release", "readonly"):
                        self.assertEqual(template["inputs"]["artifacts"][0]["path"], "/work/src")
        denied = documents["ungranted"]["spec"]["templates"][0]
        self.assertEqual(denied["name"], "needs-secret")
        self.assertNotIn("volumes", denied)
        self.assertIn("--path=" + SECRET, denied["container"]["command"])
        dockerfile = (Path(__file__).parent / "tools.Dockerfile").read_text()
        self.assertNotIn("USER ", dockerfile)

    def test_ungranted_refusal_requires_code_every_scope_field_and_real_remedy(self):
        expected = {
            "code": "forbidden",
            "contains": ["walk/owner/fixture", ".oberth/build.yaml", "needs-secret", SECRET],
            "remedy": "oberth access allow",
        }
        error = {"code": "forbidden", "reason": " ".join(expected["contains"]),
                 "remedy": "Run oberth access allow " + " ".join(expected["contains"])}
        self.assertEqual(refusal(json.dumps(error), expected), error)
        terminal = "remote: forbidden: " + error["reason"] + " - " + error["remedy"]
        with self.assertRaises(Failure):
            refusal(terminal, expected)
        for missing in expected["contains"]:
            broken = {**error, "reason": error["reason"].replace(missing, "other")}
            with self.subTest(missing=missing), self.assertRaises(Failure):
                refusal(json.dumps(broken), expected)
        for broken in ({**error, "code": "invalid_input"}, {**error, "remedy": "see above"},
                       {**error, "reason": error["reason"].replace("needs-secret", "needs-secret-extra")}):
            with self.assertRaises(Failure):
                refusal(json.dumps(broken), expected)
        with self.assertRaises(Failure):
            refusal('remote: ungranted\nremote: grant failed', expected)

    def test_invalid_refusal_names_both_offenders_in_same_typed_response(self):
        expected = {"code": "invalid_input", "contains": ["offender-one", "offender-two"], "remedy": "remove"}
        with self.assertRaises(Failure):
            refusal(json.dumps({"code": "invalid_input", "reason": "offender-one", "remedy": "remove http"}), expected)
        multiple = ("remote: invalid_input: offender-one - remove http; "
                    "offender-two - remove http")
        with self.assertRaises(Failure):
            refusal(multiple, expected)
        multiline = "remote: invalid_input: offender-one\nremote: offender-two - remove http"
        with self.assertRaises(Failure):
            refusal(multiline, expected)

    def test_push_must_return_the_run_link_from_the_selected_origin(self):
        base = "https://localhost:34443"
        identifier = "a" * 32
        self.assertEqual(pushed_run("remote: watch: " + base + "/runs/" + identifier, base), identifier)
        for bad in ("push accepted", "https://outside.invalid/runs/" + identifier,
                    base + "/runs/" + identifier + "\n" + base + "/runs/" + "b" * 32):
            with self.assertRaises(Failure):
                pushed_run(bad, base)

    def test_wrong_commit_and_missing_clean_vcs_marker_fail(self):
        good = "walk: source exact clean\nwalk: stamped exact clean\nwalk: build passed"
        build_log(good, "exact")
        for bad in (good.replace("exact", "wrong"), good.replace("walk: stamped exact clean", "build succeeded")):
            with self.assertRaises(Failure):
                build_log(bad, "exact")

    def test_generic_ui_data_and_wrong_commit_cannot_pass(self):
        expected = {"id": "run-id", "sha": "exact", "status": "passed"}
        good = {"run": expected, "steps": [{"step": "build"}]}
        ui_data(good, expected)
        for bad in ({"run": expected, "steps": []}, {"run": {**expected, "sha": "wrong"}, "steps": good["steps"]},
                    {"run": expected, "steps": [{"step": "unrelated"}]}):
            with self.assertRaises(Failure):
                ui_data(bad, expected)

    def test_a_run_row_alone_is_not_a_durable_release_record(self):
        expected = {"run_id": "run", "repo": "repo", "sha": "exact", "ref": "v0.0.1"}
        record = {**expected, "kind": "release.published", "data": {"tag": "v0.0.1"}}
        self.assertEqual(release_record([record], expected), record)
        for bad in (
            [{**record, "kind": "run.finished"}],
            [{**record, "sha": "wrong"}],
            [{**record, "data": {"tag": "wrong"}}],
        ):
            with self.assertRaises(Failure):
                release_record(bad, expected)


if __name__ == "__main__":
    unittest.main()
