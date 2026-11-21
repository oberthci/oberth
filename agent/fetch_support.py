"""Local signed release fixtures for public-shell installation tests."""
import base64
from contextlib import contextmanager
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import tempfile
import threading
import unittest


SOURCE = Path(__file__).resolve().parent / "skills/oberth/fetch-oberth"
KEY_MARKER = "RELEASE_KEY_PLACEHOLDER"
KEY_START = "<<'OBERTH_RELEASE_PUBLIC_KEY'\n"


def embedded_key(script):
    return script.split(KEY_START, 1)[1].split("\nOBERTH_RELEASE_PUBLIC_KEY", 1)[0]


def with_key(script, key):
    return script.replace(embedded_key(script), key.strip(), 1)


def check_pin(script, pin):
    embedded = embedded_key(script)
    if pin is None:
        if embedded != KEY_MARKER:
            raise ValueError("Missing canonical pin requires the documented placeholder.")
    elif KEY_MARKER in embedded or embedded.strip() != pin.strip():
        raise ValueError("Embedded release key differs from the canonical pin.")



@contextmanager
def serve(files, requests):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            pass

        def do_GET(self):
            requests.append(self.path)
            data = files.get(self.path)
            if isinstance(data, tuple) and data[0] == "redirect":
                self.send_response(302)
                self.send_header("Location", data[1])
                self.end_headers()
                return
            self.send_response(200 if data is not None else 404)
            if isinstance(data, tuple) and data[0] == "truncated":
                self.send_header("Content-Length", str(len(data[1]) + 10))
                data = data[1]
            elif isinstance(data, tuple) and data[0] == "stream":
                if data[1]["announced"]:
                    self.send_header("Content-Length", str(data[1]["total"]))
            elif data is not None:
                self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            if isinstance(data, tuple) and data[0] == "stream":
                counter = data[1]
                try:
                    while counter["bytes"] < counter["total"]:
                        self.wfile.write(b"x" * 16384)
                        self.wfile.flush()
                        counter["bytes"] += 16384
                except (BrokenPipeError, ConnectionResetError):
                    pass
                return
            try:
                self.wfile.write(data or b"")
            except (BrokenPipeError, ConnectionResetError):
                pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, kwargs={"poll_interval": 0.01})
    thread.start()
    try:
        yield "http://127.0.0.1:" + str(server.server_port)
    finally:
        server.shutdown()
        server.server_close()
        thread.join()


class FetchCase(unittest.TestCase):
    def setUp(self):
        self.downloader = next((name for name in ("curl", "wget") if shutil.which(name)), None)
        self.checksum = next((name for name in ("sha256sum", "shasum") if shutil.which(name)), None)
        self.temp = tempfile.TemporaryDirectory(prefix="oberth test ")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.home = self.root / "home space"
        self.home.mkdir()
        self.script = self.root / "copied skill/fetch-oberth"
        self.script.parent.mkdir()
        self.private = self.root / "private.pem"
        self.public = self.root / "public.pem"
        self.openssl("genpkey", "-algorithm", "EC", "-pkeyopt", "ec_paramgen_curve:P-256", "-out", str(self.private))
        self.openssl("pkey", "-in", str(self.private), "-pubout", "-out", str(self.public))
        text = SOURCE.read_text()
        self.template = with_key(text, self.public.read_text())
        self.binary = b'#!/bin/sh\nprintf executed > "$HOME/executed"\n'
        self.artifact = "oberth-" + platform.system().lower() + "-" + {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[platform.machine()]
        self.files, self.requests = {}, []
        self.release()

    def openssl(self, *args):
        return subprocess.run(["openssl", *args], check=True, capture_output=True).stdout

    def release(self, *, version="v1.2.3", binary=None, manifest=None, record=None):
        binary = self.binary if binary is None else binary
        digest = hashlib.sha256(binary).hexdigest()
        # This is the release producer's flat inventory, including non-CLI files.
        names = ["oberth-" + system + "-" + arch for system in ("linux", "darwin") for arch in ("amd64", "arm64")]
        names += ["oberth-source-plugin-linux-amd64", "oberth-source-plugin-linux-arm64", "oberth.oci.tar", "oberth-source-plugin.oci.tar", "oberth.tgz", "release.json"]
        record = record if record is not None else json.dumps({"version": version, "commit": "a" * 40, "files": {name: digest for name in names if name != "release.json"}}, indent=2).encode()
        sums = manifest if manifest is not None else "".join((hashlib.sha256(record).hexdigest() if name == "release.json" else digest) + "  " + name + "\n" for name in sorted(names)).encode()
        self.files["/latest"] = json.dumps({"version": version, "commit": "a" * 40}).encode()
        self.files["/" + version + "/release.json"] = record
        self.files["/" + version + "/SHA256SUMS"] = sums
        payload = self.root / "sums"
        payload.write_bytes(sums)
        signature = self.openssl("dgst", "-sha256", "-sign", str(self.private), str(payload))
        bundle = {"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json", "messageSignature": {"messageDigest": {"algorithm": "SHA2_256", "digest": base64.b64encode(hashlib.sha256(sums).digest()).decode()}, "signature": base64.b64encode(signature).decode()}}
        self.files["/" + version + "/SHA256SUMS.sigstore.json"] = json.dumps(bundle, indent=2).encode()
        self.files["/" + version + "/" + self.artifact] = binary

    def prepare(self, origin):
        text, count = re.subn(r"^[ \t]*origin=.*$", lambda _: "origin=" + origin, self.template, flags=re.M)
        if count != 1:
            raise ValueError("Fetcher requires exactly one origin assignment.")
        text = text.replace("--proto '=https'", "--proto '=https,http'").replace("--https-only", "")
        self.script.write_text(text)

    def environment(self, overrides=None):
        env = dict(os.environ, HOME=str(self.home), TMPDIR=str(self.root))
        env.update(overrides or {})
        return env

    def fetch(self, origin, *args, overrides=None, shell=None):
        self.prepare(origin)
        command = shell or [shutil.which("sh")]
        return subprocess.run([*command, str(self.script), *args], cwd=self.root, env=self.environment(overrides), capture_output=True, timeout=20)

    def preserved(self, version="v1.0.0"):
        target = self.home / ".local/bin/oberth"
        target.parent.mkdir(parents=True, exist_ok=True)
        if target.is_symlink() or target.exists():
            target.unlink()
        (target.parent / ("oberth-" + version)).write_bytes(b"current executable")
        target.symlink_to("oberth-" + version)
        return target

    def assert_refused(self, result, target):
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(target.read_bytes(), b"current executable")
        self.assertFalse((self.home / "executed").exists())
        lines = result.stderr.decode().splitlines()
        self.assertEqual(len(lines), 1, lines)
        self.assertLessEqual(len(lines[0]), 201)
        self.assertEqual(list(target.parent.glob(".oberth-fetch*")), [])
        self.assertEqual(list(self.root.glob(".oberth-fetch*")), [])

    def available(self, *names):
        found = []
        for name in names:
            with self.subTest(tool=name):
                if not shutil.which(name):
                    self.skipTest(name + " is not installed")
                found.append(name)
        self.assertTrue(found, "Fetch contracts need " + " or ".join(names))
        return found

    def tools(self, *, downloader=None, checksum=None, omit=()):
        downloader = downloader or self.downloader
        checksum = checksum or self.checksum
        self.assertIsNotNone(downloader, "Fetch contracts need curl or wget")
        self.assertIsNotNone(checksum, "Fetch contracts need sha256sum or shasum")
        directory = self.root / ("commands-" + downloader + "-" + checksum + "-" + "-".join(omit))
        directory.mkdir(exist_ok=True)
        names = "awk cat chmod cp head ln mkdir mktemp mv openssl readlink rm rmdir tr uname wc".split() + [downloader, checksum]
        for name in names:
            path = shutil.which(name)
            if path and name not in omit and not (directory / name).exists():
                (directory / name).symlink_to(path)
        return {"PATH": str(directory)}
