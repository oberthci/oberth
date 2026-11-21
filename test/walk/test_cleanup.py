"""Cleanup is limited to exact resources created by this invocation."""
import io
import json
from pathlib import Path
import tempfile
import unittest
import urllib.error
from unittest.mock import patch
from walkassert import Blocked, Failure, HERE
from walkconfig import Environment
from walkio import Session
from walkhost import Runtime
from types import SimpleNamespace
from walklib import parser
from walkux import UX
import walkcleanup


class Response(io.BytesIO):
    def __init__(self, value=b"", headers=None):
        super().__init__(value)
        self.headers = headers or {}


class CleanupTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        root = Path(temporary.name)
        path = root / "env.json"
        path.write_text((HERE / "env.example").read_text())
        config = Environment.load(path, "walk-1")
        args = parser().parse_args([])
        self.s = Session(
            config,
            args,
            UX(HERE / "golden-path.json", "fresh", lambda: 0),
            Runtime(
                lambda: 0, lambda: "1" * 32, SimpleNamespace(busy=False, samples=[(0, 0, 0)], sample=lambda: (0, 0, 0))
            ),
        )

    def test_warm_cleanup_reuses_recovery_when_install_writes_no_new_secret_file(self):
        s = self.s
        state = s.config.path("state")
        state.mkdir(parents=True)
        s.data.installed = True
        s.data.recovery = state / "walk-1.secrets"
        s.data.recovery.write_text("OPENBAO_ROOT_TOKEN=test-value\n")
        for suffix in ("", ".pub"):
            (s.private / ("upstream" + suffix)).write_text("test upstream")
        s.binary.write_bytes(b"verified CLI")
        with patch.object(s, "k", return_value=(0, b'{"metadata":{"uid":"same-cluster"}}', b"")):
            walkcleanup.preserve_warm(s)
        self.assertEqual(s.data.recovery.read_text(), "OPENBAO_ROOT_TOKEN=test-value\n")
        self.assertEqual(json.loads((state / "walk-1-warm.json").read_text())["cluster_uid"], "same-cluster")

    def test_cleanup_deletes_only_exact_key_and_repository_names(self):
        self.s.data.key_id, self.s.data.repository_created = 7, True
        with patch.object(self.s, "upstream") as upstream:
            result = walkcleanup.cleanup(self.s)
        self.assertTrue(result["owned_key_repository_images_removed"])
        self.assertEqual(upstream.call_args_list[0].args, ("user/keys/7",))
        self.assertEqual(upstream.call_args_list[1].args, ("repos/oberthci/" + self.s.data.name,))
        self.assertTrue(all(c.kwargs["method"] == "DELETE" for c in upstream.call_args_list))

    def test_cleanup_continues_other_owned_actions_after_one_failure(self):
        self.s.data.key_id, self.s.data.repository_created = 7, True
        with patch.object(self.s, "upstream", side_effect=[Failure("key unavailable"), None]) as upstream:
            with self.assertRaisesRegex(Failure, "Cleanup incomplete"):
                walkcleanup.cleanup(self.s)
        self.assertEqual(upstream.call_count, 2)

    def test_lost_create_response_can_be_cleaned_by_exact_title_and_public_key(self):
        (self.s.private / "upstream.pub").write_text("ssh-ed25519 owned comment")
        keys = [{"id": 1, "title": "unrelated", "key": "ssh-ed25519 owned comment"},
                {"id": 2, "title": self.s.data.name, "key": "ssh-ed25519 different comment"},
                {"id": 3, "title": self.s.data.name, "key": "ssh-ed25519 owned comment"}]
        with patch.object(self.s, "upstream", side_effect=[keys, None]) as upstream:
            walkcleanup.cleanup(self.s)
        self.assertEqual(upstream.call_args_list[-1].args, ("user/keys/3",))

    def test_foreign_cleanup_name_is_refused_before_any_network(self):
        self.s.data.name = "another-owner"
        with patch.object(self.s, "upstream") as upstream:
            with self.assertRaises(Failure):
                walkcleanup.cleanup(self.s)
        upstream.assert_not_called()
        with self.assertRaises(Failure):
            walkcleanup.registry_remove(self.s, "foreign/repository")

    def test_registry_delete_disabled_blocks_before_allocating_any_resources(self):
        with patch("walkcleanup.request", side_effect=Failure("HTTP 405", {"http_status": 405})):
            with self.assertRaisesRegex(Blocked, "Enable named manifest deletion"):
                walkcleanup.registry_ready(self.s)
        self.assertFalse(self.s.data.repository_created)
        self.assertEqual(self.s.data.tools_repository, "")

    def test_nonexistent_owned_digest_404_proves_registry_delete_capability(self):
        with patch("walkcleanup.request", side_effect=Failure("HTTP 404", {"http_status": 404})) as request:
            walkcleanup.registry_ready(self.s)
        self.assertEqual(request.call_args.kwargs["method"], "DELETE")
        self.assertIn("/v2/walk-tools/" + self.s.data.name + "/", request.call_args.args[0])

    def test_registry_cleanup_includes_signature_tags_and_survives_duplicate_digest_404(self):
        name = "walk-release/" + self.s.data.name
        self.s.data.image_repository = name
        requests = []
        identity = "sha256:" + "a" * 64
        class Transport:
            deleted = set()
            def open(inner, request, timeout):
                if isinstance(request, str):
                    return Response(json.dumps({"tags": ["release", "duplicate", "signature"]}).encode())
                requests.append((request.get_method(), request.full_url))
                if request.get_method() == "HEAD":
                    if request.full_url in inner.deleted or request.full_url.endswith("duplicate"):
                        raise urllib.error.HTTPError(request.full_url, 404, "gone", {}, io.BytesIO())
                    inner.last_tag = request.full_url
                if request.get_method() == "DELETE":
                    inner.deleted.add(inner.last_tag)
                return Response(headers={"Docker-Content-Digest": identity})
        with patch("walkcleanup.urllib.request.build_opener", return_value=Transport()):
            walkcleanup.registry_remove(self.s, name)
        self.assertEqual(sum(method == "DELETE" for method, _ in requests), 2)
        self.assertTrue(any(url.endswith("signature") for _, url in requests))
        self.assertTrue(all("/v2/" + name + "/" in url for _, url in requests))

    def test_registry_cleanup_deletes_the_index_and_refuses_a_still_readable_tag(self):
        class Transport:
            removed = False
            retain = False
            def open(inner, request, timeout):
                if request.get_method() == "DELETE":
                    self.assertTrue(request.full_url.endswith("sha256:" + "a" * 64))
                    inner.removed = True
                elif request.get_method() == "HEAD":
                    self.assertIn("application/vnd.oci.image.index.v1+json", request.headers["Accept"])
                    self.assertIn(
                        "application/vnd.docker.distribution.manifest.list.v2+json", request.headers["Accept"]
                    )
                    if inner.removed and not inner.retain:
                        raise urllib.error.HTTPError(request.full_url, 404, "gone", {}, io.BytesIO())
                return Response(headers={"Docker-Content-Digest": "sha256:" + "a" * 64})
        walkcleanup.registry_tag_remove(Transport(), "http://localhost:5001/v2/owned", "fixture")
        retained = Transport()
        retained.retain = True
        with self.assertRaisesRegex(Failure, "remains readable"):
            walkcleanup.registry_tag_remove(retained, "http://localhost:5001/v2/owned", "fixture")

    def test_private_cleanup_cannot_follow_a_symlink_to_other_files(self):
        outside = self.s.private.parent / "unrelated"
        outside.mkdir()
        (outside / "keep").write_text("retained")
        (self.s.private / "link").symlink_to(outside, target_is_directory=True)
        (self.s.private / "credential").write_text("obsolete private value")
        walkcleanup.remove_private(self.s)
        self.assertFalse(self.s.private.exists())
        self.assertEqual((outside / "keep").read_text(), "retained")

    def test_private_cleanup_refuses_an_unowned_directory(self):
        self.s.private = self.s.private.parent
        with self.assertRaises(Failure):
            walkcleanup.remove_private(self.s)


if __name__ == "__main__":
    unittest.main()
