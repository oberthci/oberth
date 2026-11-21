"""Check installation ownership, signals, atomic publication and downgrade consent."""
import os
from pathlib import Path
import shutil
import signal
import subprocess
import unittest

from fetch_support import FetchCase, serve


class InstallContract(FetchCase):
    def test_existing_client_reports_version_and_requires_force_for_explicit_replacement(self):
        for installed in ("v1.0.0", "v1.2.3", "v2.0.0"):
            with self.subTest(installed=installed):
                target = self.preserved(installed)
                self.requests.clear()
                with serve(self.files, self.requests) as origin:
                    result = self.fetch(origin, "v1.2.3")
                    self.assert_refused(result, target)
                    self.assertIn(installed.encode(), result.stdout)
                    self.assertIn(b"Pass --force to replace it.", result.stderr)
                    self.assertEqual(self.requests, [])
                    replacement = self.fetch(origin, "--force", "v1.2.3")
                self.assertEqual(replacement.returncode, 0, replacement.stderr)
                self.assertEqual(target.read_bytes(), self.binary)

    def test_unidentified_existing_binary_requires_force_without_execution(self):
        target = self.preserved()
        target.unlink()
        target.write_bytes(b"current executable")
        with serve(self.files, self.requests) as origin:
            self.assert_refused(self.fetch(origin), target)
            result = self.fetch(origin, "--force")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertFalse((self.home / "executed").exists())

    @unittest.skipIf(os.geteuid() == 0, "Permission checks require an unprivileged user.")
    def test_unwritable_folder_has_its_own_reason(self):
        target = self.preserved()
        target.parent.chmod(0o500)
        try:
            with serve(self.files, self.requests) as origin:
                result = self.fetch(origin, "--force")
            self.assert_refused(result, target)
            self.assertIn(b"Install directory is not writable.", result.stderr)
        finally:
            target.parent.chmod(0o700)

    def test_atomic_publication_failure_preserves_current(self):
        target = self.preserved()
        tools = self.tools()
        wrapper = Path(tools["PATH"]) / "mv"
        wrapper.unlink()
        wrapper.write_text("#!/bin/sh\nexit 1\n")
        wrapper.chmod(0o700)
        with serve(self.files, self.requests) as origin:
            self.assert_refused(self.fetch(origin, "--force", overrides=tools), target)

    def test_foreign_directory_is_kept_and_cleans_owned_lock(self):
        target = self.home / ".local/bin/oberth"
        target.mkdir(parents=True)
        with serve(self.files, self.requests) as origin:
            result = self.fetch(origin, "--force")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(target.with_name("oberth.previous").is_dir())
        self.assertTrue(target.is_symlink())
        self.assertEqual(list(target.parent.glob(".oberth-fetch*")), [])

    def barrier(self, origin):
        self.prepare(origin)
        tools = self.tools()
        ready, release = self.root / "ready", self.root / "release"
        for path in (ready, release):
            if path.exists():
                path.unlink()
            os.mkfifo(path)
        wrapper = Path(tools["PATH"]) / "mv"
        wrapper.unlink()
        wrapper.write_text('#!/bin/sh\nprintf ready > "$FETCH_READY"\nread -r go < "$FETCH_RELEASE"\nexec ' + shutil.which("mv") + ' "$@"\n')
        wrapper.chmod(0o700)
        env = self.environment(dict(tools, FETCH_READY=str(ready), FETCH_RELEASE=str(release)))
        process = subprocess.Popen([shutil.which("sh"), str(self.script), "--force"], cwd=self.root, env=env, start_new_session=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.addCleanup(self.stop, process)
        with ready.open() as message:
            self.assertEqual(message.read(), "ready")
        return process, release

    def stop(self, process):
        if process.poll() is None:
            os.killpg(process.pid, signal.SIGKILL)
        process.communicate(timeout=10)

    def test_live_owner_serializes_and_publishes_complete_verified_bytes(self):
        target = self.preserved()
        with serve(self.files, self.requests) as origin:
            process, release = self.barrier(origin)
            before = target.read_bytes()
            second = self.fetch(origin, "--force")
            during = target.read_bytes()
            # Two rename operations publish the version and then the active link.
            for _ in range(2):
                with release.open("w") as message:
                    message.write("go\n")
                if _ == 0:
                    with (self.root / "ready").open() as message:
                        self.assertEqual(message.read(), "ready")
            _, stderr = process.communicate(timeout=10)
        self.assertEqual(process.returncode, 0, stderr)
        self.assertNotEqual(second.returncode, 0)
        self.assertIn(b"Another fetch is installing.", second.stderr)
        self.assertIn(b"~/.local/bin/.oberth-fetch-lock", second.stderr)
        self.assertEqual(before, b"current executable")
        self.assertEqual(during, b"current executable")
        self.assertEqual(target.read_bytes(), self.binary)
        self.assertEqual(list(target.parent.glob(".oberth-fetch*")), [])

    def test_catchable_exits_release_the_lock(self):
        for sig in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
            with self.subTest(signal=sig):
                target = self.preserved()
                with serve(self.files, self.requests) as origin:
                    process, _ = self.barrier(origin)
                    os.killpg(process.pid, sig)
                    _, stderr = process.communicate(timeout=10)
                self.assertNotEqual(process.returncode, 0, stderr)
                self.assertEqual(target.read_bytes(), b"current executable")
                self.assertEqual(list(target.parent.glob(".oberth-fetch*")), [])

    def test_dead_owner_after_sigkill_is_taken_over(self):
        target = self.preserved()
        with serve(self.files, self.requests) as origin:
            process, _ = self.barrier(origin)
            os.killpg(process.pid, signal.SIGKILL)
            process.communicate(timeout=10)
            self.assertEqual(target.read_bytes(), b"current executable")
            result = self.fetch(origin, "--force")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(target.read_bytes(), self.binary)
        self.assertEqual(list(target.parent.glob(".oberth-fetch*")), [])
        self.assertEqual(list(self.root.glob(".oberth-fetch*")), [])


if __name__ == "__main__":
    unittest.main()
