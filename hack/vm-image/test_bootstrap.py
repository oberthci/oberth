"""Exercise guest provisioning inputs without a VM or a container daemon."""
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch


HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[1]


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


bootstrap = module("bootstrap", HERE / "bootstrap.py")
cloud_init = module("cloud_init", HERE / "cloud_init.py")
sys.path.insert(0, str(ROOT / "test/walk"))
from walkconfig import Environment


class GuestProvisioningTest(unittest.TestCase):
    def test_provisioning_connects_pinned_tools_and_local_fixtures(self):
        pins = json.loads((ROOT / ".oberth/pins/walk-tools.json").read_text())
        calls = []
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            for name in ("bin", "home"):
                (root / name).mkdir()

            def run(*args, **kwargs):
                calls.append(args)
                if args[:3] == ("docker", "network", "inspect"):
                    return "172.17.0.1"
                if args[0] == "cosign":
                    for suffix in (".key", ".pub"):
                        Path(args[-1] + suffix).touch()
                return "ssh-ed25519 synthetic"

            resolv = root / "resolv.conf"
            resolv.symlink_to(root / "absent-stub-resolver")
            with patch.multiple(bootstrap, ROOT=root / "environment", HOME=root / "home", BIN=root / "bin", RESOLV=resolv), patch.object(bootstrap, "run", side_effect=run), patch.object(bootstrap, "install_tool") as install, patch.object(bootstrap.shutil, "which", return_value="tool"):
                bootstrap.provision(pins)
            self.assertFalse(resolv.is_symlink())
            self.assertEqual(resolv.read_text(), "nameserver 1.1.1.1\nnameserver 8.8.8.8\n")
            self.assertEqual(install.call_count, len(pins["tools"]))
            self.assertEqual({call.args[0]["name"] for call in install.call_args_list}, {"k3d", "kubectl", "cosign"})
            self.assertIn(("git", "--version"), calls)
            self.assertEqual([args[2] for args in calls if args[:2] == ("docker", "pull")], list(pins["images"].values()))
            self.assertIn(bootstrap.registry_command(pins["images"]["registry"]), calls)
            script = (root / "bin/skopeo").read_text()
            self.assertTrue(script.startswith("#!/bin/sh\nexec docker --context walk "))
            self.assertIn(pins["images"]["skopeo"], script)
            self.assertEqual(len((root / "environment/private/known_hosts").read_text().splitlines()), 2)
            config = Environment.load(root / "environment/environment.json", "walk-1")
            self.assertEqual(config["upstream_url"], "ssh://git@172.17.0.1:33022/oberthci")

    def test_cloud_init_embeds_the_admitted_script_and_pins(self):
        doc = cloud_init.document("ssh-ed25519 synthetic\n", ROOT)
        files = {item["path"]: item["content"] for item in doc["write_files"]}
        self.assertEqual(files["/etc/oberth-walk/bootstrap.py"], (HERE / "bootstrap.py").read_text())
        self.assertEqual(json.loads(files["/etc/oberth-walk/pins.json"]), json.loads((ROOT / ".oberth/pins/walk-tools.json").read_text()))
        self.assertEqual(doc["runcmd"], [["python3", "/etc/oberth-walk/bootstrap.py"]])
        self.assertEqual(doc["users"][0]["groups"], ["docker"])
        workflow = (ROOT / ".oberth/walk-vm.yaml").read_text()
        self.assertIn("python3 /work/src/hack/vm-image/cloud_init.py --public id_ed25519.pub > cloud-init", workflow)
        self.assertIn("cloud-init status --wait && python3 /etc/oberth-walk/bootstrap.py --check", (ROOT / "hack/walk-vm.sh").read_text())

    def test_every_download_and_container_has_a_fixed_digest(self):
        pins = json.loads((ROOT / ".oberth/pins/walk-tools.json").read_text())
        self.assertEqual({tool["name"] for tool in pins["tools"]}, {"k3d", "kubectl", "cosign"})
        for tool in pins["tools"]:
            self.assertIn(tool["version"], tool["url"])
            self.assertRegex(tool["sha256"], r"^[0-9a-f]{64}$")
        for image in pins["images"].values():
            self.assertRegex(image, r":[^/@]+@sha256:[0-9a-f]{64}$")

    def test_check_fails_for_each_missing_tool(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self.environment(root)
            for missing in bootstrap.TOOLS:
                with self.subTest(tool=missing), patch.object(bootstrap.shutil, "which", side_effect=lambda name: None if name == missing else "/bin/" + name):
                    with self.assertRaisesRegex(ValueError, "required executable: " + missing):
                        bootstrap.check(root)
            with patch.object(bootstrap.shutil, "which", return_value="/bin/tool"):
                bootstrap.check(root)

    def test_environment_is_consumed_by_the_real_walk_loader(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            self.environment(root)
            config = Environment.load(root / "environment.json", "walk-1")
            self.assertEqual(config["docker_context"], "walk")
            self.assertEqual(config["registry_inside"], "k3d-walk-registry:5000")
            (root / "private/known_hosts").unlink()
            with patch.object(bootstrap.shutil, "which", return_value="/bin/tool"):
                with self.assertRaisesRegex(ValueError, "Provide environment file: known_hosts"):
                    bootstrap.check(root)

    def test_corrupt_download_never_becomes_an_executable(self):
        body = b"pinned executable"
        pin = {"name": "tool", "url": "https://oberth.ci/tool", "sha256": hashlib.sha256(body).hexdigest()}
        for content in (body, b"corrupt"):
            with self.subTest(content=content), tempfile.TemporaryDirectory() as temporary:
                response = io.BytesIO(content)
                response.geturl = lambda: pin["url"]
                with patch.object(bootstrap.urllib.request, "urlopen", return_value=response):
                    if content == body:
                        bootstrap.install_tool(pin, Path(temporary))
                        self.assertEqual((Path(temporary) / "tool").stat().st_mode & 0o777, 0o755)
                    else:
                        with self.assertRaisesRegex(ValueError, "checksum differs"):
                            bootstrap.install_tool(pin, Path(temporary))
                        self.assertFalse((Path(temporary) / "tool").exists())

    def environment(self, root):
        pins = json.loads((ROOT / ".oberth/pins/walk-tools.json").read_text())
        config = bootstrap.environment(pins["images"], "172.17.0.1")
        (root / "environment.json").write_text(json.dumps(config))
        (root / "private").mkdir()
        for name in ("upstream_token", "known_hosts", "signing_key", "signing_public", "signing_password"):
            (root / config[name]).touch()
