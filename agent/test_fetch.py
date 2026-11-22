"""Verify signed standalone installation against the release producer's layout."""
from pathlib import Path
import json
import shutil
import unittest

from fetch_support import DOWNLOAD, FetchCase, LATEST, SOURCE, KEY_MARKER, check_pin, embedded_key, serve


class FetchContract(FetchCase):
    def test_standalone_c13_inventory_and_base64_bundle_install_without_execution(self):
        with serve(self.files, self.requests) as origin:
            result = self.fetch(origin)
        self.assertEqual(result.returncode, 0, result.stderr)
        target = self.home / ".local/bin/oberth"
        self.assertEqual(target.read_bytes(), self.binary)
        self.assertTrue(target.is_symlink())
        self.assertEqual(target.stat().st_mode & 0o777, 0o755)
        self.assertIn(str(target).encode(), result.stdout)
        self.assertFalse((self.home / "executed").exists())
        self.assertFalse((self.root / ".oberth").exists())

    def test_signed_unrelated_entries_are_ignored(self):
        for extra in (b"extra-image  unrelated.oci\n", b"malformed unrelated metadata\n", b"0  ../unrelated\n"):
            sums = self.files["/v1.2.3/SHA256SUMS"] + extra
            self.release(manifest=sums)
            with serve(self.files, self.requests) as origin:
                result = self.fetch(origin, "--force")
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_wrong_signature_checksum_and_truncation_preserve_current(self):
        for fault in ("other key", "bad signature", "replayed signature", "checksum", "truncated"):
            with self.subTest(fault=fault):
                self.release()
                path = "/v1.2.3/" + self.artifact
                bundle_path = "/v1.2.3/SHA256SUMS.sigstore.json"
                if fault == "other key":
                    self.private = self.root / "other.pem"
                    self.openssl("genpkey", "-algorithm", "EC", "-pkeyopt", "ec_paramgen_curve:P-256", "-out", str(self.private))
                    self.release()
                    self.private = self.root / "private.pem"
                elif fault == "bad signature":
                    self.files[bundle_path] = b'{"messageSignature":{"signature":"aW52YWxpZA=="}}'
                elif fault == "replayed signature":
                    self.files["/v1.2.3/SHA256SUMS"] += b"new signed entry\n"
                elif fault == "checksum":
                    self.files[path] = b"wrong binary"
                else:
                    self.files[path] = ("truncated", self.binary[:10])
                target = self.preserved()
                with serve(self.files, self.requests) as origin:
                    self.assert_refused(self.fetch(origin, "--force"), target)

    def test_redirects_off_the_release_hosts_are_refused(self):
        foreign_requests = []
        target = self.preserved()
        with serve({}, foreign_requests) as foreign:
            for downloader in self.available("curl", "wget"):
                for path in tuple(self.files):
                    for destination in (foreign + "/redirect", "/same-host", "http://release-assets.githubusercontent.com/asset"):
                        with self.subTest(downloader=downloader, path=path, destination=destination):
                            saved = self.files[path]
                            self.files[path] = ("redirect", destination)
                            with serve(self.files, self.requests) as origin:
                                result = self.fetch(origin, "--force", overrides=self.tools(downloader=downloader))
                                self.assert_refused(result, target)
                                self.assertIn(destination.encode(), result.stderr)
                            self.files[path] = saved
        self.assertEqual(foreign_requests, [])
        self.assertNotIn("/same-host", self.requests)

    def asset_redirects(self, asset, origin):
        """Serve every release file from the asset host, as GitHub does; latest goes through the tag address."""
        assets = {}
        for path in tuple(self.files):
            assets["/asset" + path] = self.files[path]
            self.files[path] = ("redirect", asset + "/asset" + path)
        self.files[LATEST] = ("redirect", origin + DOWNLOAD + "/v1.2.3/latest-record")
        self.files["/v1.2.3/latest-record"] = ("redirect", asset + "/asset" + LATEST)
        return assets

    def test_redirects_to_the_asset_host_are_followed(self):
        for downloader in self.available("curl", "wget"):
            with self.subTest(downloader=downloader):
                self.release()
                assets, asset_requests = {}, []
                target = self.preserved()
                with serve(assets, asset_requests) as asset, serve(self.files, self.requests) as origin:
                    self.asset_host = asset
                    assets.update(self.asset_redirects(asset, origin))
                    result = self.fetch(origin, "--force", overrides=self.tools(downloader=downloader))
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(target.read_bytes(), self.binary)
                self.assertIn("/asset/v1.2.3/" + self.artifact, asset_requests)
                self.assertIn("/asset" + LATEST, asset_requests)

    def test_tampered_bytes_and_long_chains_after_redirect_are_refused(self):
        for downloader in self.available("curl", "wget"):
            for fault in ("tampered", "chain"):
                with self.subTest(downloader=downloader, fault=fault):
                    self.release()
                    assets, asset_requests = {}, []
                    target = self.preserved()
                    with serve(assets, asset_requests) as asset, serve(self.files, self.requests) as origin:
                        self.asset_host = asset
                        assets.update(self.asset_redirects(asset, origin))
                        path = "/asset/v1.2.3/" + self.artifact
                        if fault == "tampered":
                            assets[path] = b"tampered binary"
                        else:
                            for hop in range(4):
                                assets[path + str(hop)] = ("redirect", asset + path + str(hop + 1))
                            assets[path] = ("redirect", asset + path + "0")
                        result = self.fetch(origin, "--force", overrides=self.tools(downloader=downloader))
                    self.assert_refused(result, target)
                    if fault == "tampered":
                        self.assertIn(b"Release checksum is invalid.", result.stderr)
                    else:
                        self.assertIn(b"redirected elsewhere", result.stderr)
                        self.assertIn(path + "1", asset_requests)
                        self.assertNotIn(path + "2", asset_requests)

    def test_selected_artifact_refuses_missing_duplicate_or_invalid_digest(self):
        original = self.files["/v1.2.3/SHA256SUMS"]
        selected = next(line for line in original.splitlines(keepends=True) if line.split()[1].decode() == self.artifact)
        for manifest in (original.replace(selected, b""), original + selected, original.replace(selected, selected.upper()), original.replace(selected, b"bad  " + self.artifact.encode() + b"\n"), original.replace(selected, selected.rstrip() + b" extra\n")):
            self.release(manifest=manifest)
            target = self.preserved()
            with serve(self.files, self.requests) as origin:
                self.assert_refused(self.fetch(origin, "--force"), target)

    def test_available_downloaders_and_checksums_work(self):
        downloaders = self.available("curl", "wget")
        checksums = self.available("sha256sum", "shasum")
        with serve(self.files, self.requests) as origin:
            for downloader in downloaders:
                for checksum in checksums:
                    with self.subTest(downloader=downloader, checksum=checksum):
                        result = self.fetch(origin, "--force", overrides=self.tools(downloader=downloader, checksum=checksum))
                        self.assertEqual(result.returncode, 0, result.stderr)

    def test_old_curl_is_used_without_a_version_gate(self):
        target = self.preserved()
        tools = self.tools()
        wrapper = Path(tools["PATH"]) / "curl"
        wrapper.unlink()
        wrapper.write_text('#!/bin/sh\n[ "$1" != --version ] || { printf "curl 7.81.0\\n"; exit 0; }\nexec ' + shutil.which("curl") + ' "$@"\n')
        wrapper.chmod(0o700)
        with serve(self.files, self.requests) as origin:
            result = self.fetch(origin, "--force", overrides=tools)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(target.read_bytes(), self.binary)

    def test_named_missing_prerequisites_refuse_in_one_line(self):
        target = self.preserved()
        downloader = self.available("curl", "wget")[0]
        checksum = self.available("sha256sum", "shasum")[0]
        for missing, reason in (("openssl", b"Missing openssl."), (downloader, b"Downloader is missing."), (checksum, b"Checksum tool is missing.")):
            with self.subTest(tool=missing), serve(self.files, self.requests) as origin:
                self.assert_refused(self.fetch(origin, "--force", overrides=self.tools(omit=(missing,))), target)
                self.assertIn(reason, self.fetch(origin, "--force", overrides=self.tools(omit=(missing,))).stderr)
        self.assertEqual(self.requests, [])

    def test_unsafe_version_never_enters_a_download_path(self):
        target = self.preserved()
        with serve(self.files, self.requests) as origin:
            for version in ("../v1.2.3", "--version", "v1.2.3\nv2.3.4", "v01.2.3"):
                self.assert_refused(self.fetch(origin, "--force", version), target)
        self.assertEqual(self.requests, [])

    def test_only_top_level_version_is_selected(self):
        records = (
            {"components": {"tool": {"version": "v0.0.0"}}, "version": "v1.2.3"},
            {"version": "v1.2.3", "nested": [{"version": "v9.0.0"}]},
            {"description": 'braces { } [ ] and escaped "version":"other"', "version": "v1.2.3"},
        )
        for record in records:
            with self.subTest(record=record):
                self.release(record=json.dumps(record).encode())
                self.files[LATEST] = json.dumps({"metadata": {"version": "v9.0.0"}, "version": "v1.2.3"}).encode()
                with serve(self.files, self.requests) as origin:
                    result = self.fetch(origin, "--force")
                self.assertEqual(result.returncode, 0, result.stderr)

    def test_nested_version_cannot_supply_or_override_the_root_version(self):
        for record in (
            b'{"nested":{"version":"v1.2.3"}}',
            b'{"version":"v0.0.0","nested":{"version":"v1.2.3"}}',
            b'{"version":"v1.2.3","version":"v1.2.3"}',
        ):
            self.release(record=record)
            target = self.preserved()
            with serve(self.files, self.requests) as origin:
                self.assert_refused(self.fetch(origin, "--force"), target)

    def test_available_downloaders_close_oversized_streams(self):
        self.template = self.template.replace('"$work/binary" 536870912', '"$work/binary" 4096')
        for downloader in self.available("curl", "wget"):
            for announced in (False, True):
                with self.subTest(downloader=downloader, announced=announced):
                    counter = {"bytes": 0, "total": 64 * 1024 * 1024, "announced": announced}
                    path = "/v1.2.3/" + self.artifact
                    self.files[path] = ("stream", counter)
                    self.requests.clear()
                    target = self.preserved()
                    with serve(self.files, self.requests) as origin:
                        result = self.fetch(origin, "--force", overrides=self.tools(downloader=downloader))
                    self.assert_refused(result, target)
                    reason = b"Release file exceeds its limit." if downloader == "wget" else b"Release download failed."
                    self.assertIn(reason, result.stderr)
                    self.assertIn(path, self.requests)
                    self.assertLess(counter["bytes"], counter["total"])

    def test_signed_record_binds_the_requested_version(self):
        target = self.preserved()
        self.release(version="v1.2.2")
        for path, data in list(self.files.items()):
            if path.startswith("/v1.2.2/"):
                self.files[path.replace("/v1.2.2/", "/v1.2.3/")] = data
        self.files[LATEST] = b'{"version":"v1.2.3","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}'
        with serve(self.files, self.requests) as origin:
            result = self.fetch(origin, "--force")
        self.assert_refused(result, target)
        self.assertIn(b"version does not match", result.stderr)

    def test_signed_record_checksum_and_version_are_required(self):
        target = self.preserved()
        for fault in ("checksum", "version"):
            self.release()
            if fault == "checksum":
                self.files["/v1.2.3/release.json"] += b"changed"
            else:
                self.release(record=b'{"commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}')
            with serve(self.files, self.requests) as origin:
                self.assert_refused(self.fetch(origin, "--force"), target)

    def test_advertised_size_caps_accept_n_refuse_n_plus_one(self):
        self.template = self.template.replace('"$work/binary" 536870912', '"$work/binary" 128').replace("metadata_max=1048576", "metadata_max=2048")
        for field in ("binary", "sums", "bundle", "record", "latest", "version"):
            for extra in (0, 1):
                with self.subTest(field=field, extra=extra):
                    version = "v1.2." + "3" * (123 + extra) if field == "version" else "v1.2.3"
                    self.release(version=version, binary=self.binary.ljust(128 + extra if field == "binary" else 128, b"\n"))
                    path = {"sums": "/v1.2.3/SHA256SUMS", "bundle": "/v1.2.3/SHA256SUMS.sigstore.json", "record": "/v1.2.3/release.json", "latest": LATEST}.get(field)
                    if path:
                        padded = self.files[path].ljust(2048 + extra, b" ")
                        if field == "sums":
                            self.release(binary=self.binary.ljust(128, b"\n"), manifest=padded)
                        elif field == "record":
                            self.release(binary=self.binary.ljust(128, b"\n"), record=padded)
                        else:
                            self.files[path] = padded
                    target = self.preserved()
                    with serve(self.files, self.requests) as origin:
                        result = self.fetch(origin, "--force")
                    if extra:
                        self.assert_refused(result, target)
                    else:
                        self.assertEqual(result.returncode, 0, result.stderr)

    def test_placeholder_and_missing_home_refuse_before_network(self):
        target = self.preserved()
        for overrides in ({"HOME": ""}, None):
            if overrides is None:
                self.template = SOURCE.read_text().replace(embedded_key(SOURCE.read_text()), KEY_MARKER)
            with serve(self.files, self.requests) as origin:
                self.assert_refused(self.fetch(origin, "--force", overrides=overrides), target)
        self.assertEqual(self.requests, [])

    def test_supported_shells_agree(self):
        shells = [[shutil.which("dash")], [shutil.which("busybox"), "sh"], [shutil.which("bash"), "--posix"]]
        shells = [shell for shell in shells if shell[0]]
        self.assertTrue(shells, "Fetch contracts need a supported shell")
        with serve(self.files, self.requests) as origin:
            for shell in shells:
                with self.subTest(shell=shell):
                    result = self.fetch(origin, "--force", shell=shell)
                    self.assertEqual(result.returncode, 0, result.stderr)

    def test_embedded_release_key_contract(self):
        pin = SOURCE.parents[3] / "internal/releaseverify/release-cosign.pub"
        self.assertTrue(pin.is_file(), "Canonical release pin file is missing")
        try:
            check_pin(SOURCE.read_text(), pin.read_text() or None)
        except ValueError as error:
            self.fail(str(error))
        check_pin(self.template, self.public.read_text())
        with self.assertRaises(ValueError):
            check_pin(self.template, "different pin")
        with self.assertRaises(ValueError):
            check_pin(self.template, None)
        with self.assertRaises(ValueError):
            check_pin(SOURCE.read_text().replace(embedded_key(SOURCE.read_text()), KEY_MARKER), self.public.read_text())

    def test_damaged_key_refuses_before_download(self):
        target = self.preserved()
        self.template = self.template.replace(embedded_key(self.template), "invalid key")
        with serve(self.files, self.requests) as origin:
            self.assert_refused(self.fetch(origin, "--force"), target)
        self.assertEqual(self.requests, [])

    def test_latest_refuses_downgrade_but_explicit_version_allows_it(self):
        for installed in ("v1.2.4", "v1.3.0", "v2.0.0", "v10.0.0"):
            with self.subTest(installed=installed):
                target = self.preserved(installed)
                self.requests.clear()
                with serve(self.files, self.requests) as origin:
                    result = self.fetch(origin, "--force")
                    self.assert_refused(result, target)
                    self.assertIn(b"Latest resolves to v1.2.3.", result.stdout)
                    self.assertIn(b"Latest is older than installed version.", result.stderr)
                    self.assertIn(b"Pass the desired version explicitly", result.stderr)
                    self.assertEqual(self.requests, [LATEST])
                    result = self.fetch(origin, "--force", "v1.2.3")
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(target.read_bytes(), self.binary)

    def test_latest_allows_equal_and_newer_versions(self):
        for installed in ("v1.2.3", "v1.2.2", "v1.1.9", "v0.99.99"):
            with self.subTest(installed=installed):
                target = self.preserved(installed)
                with serve(self.files, self.requests) as origin:
                    result = self.fetch(origin, "--force")
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn(b"Latest resolves to v1.2.3.", result.stdout)
                self.assertEqual(target.read_bytes(), self.binary)

    def test_fetch_version_uses_the_release_grammar(self):
        pattern = (SOURCE.parents[3] / "internal/workflowpolicy/release-version.pattern").read_text().strip()
        self.assertIn("version_pattern='" + pattern + "'", SOURCE.read_text())
        for version in ("v1.2.3-rc.1", "v1.2.3+build", "v01.2.3", "v1.2"):
            with self.subTest(version=version):
                target = self.preserved()
                self.requests.clear()
                with serve(self.files, self.requests) as origin:
                    self.assert_refused(self.fetch(origin, "--force", version), target)
                self.assertEqual(self.requests, [])


if __name__ == "__main__":
    unittest.main()
