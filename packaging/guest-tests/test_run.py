import argparse
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("guest_run", Path(__file__).with_name("run.py"))
guest = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guest)


class GuestBoundaryTests(unittest.TestCase):
    def test_vm_has_no_host_filesystem_or_network(self):
        args = argparse.Namespace(qemu="qemu", firmware="firmware", kernel="kernel")
        command = guest.qemu_command(args, "initramfs")
        for flag, value in (("-accel", "kvm"), ("-nic", "none"), ("-monitor", "none"),
                            ("-smp", "2"), ("-m", "2048")):
            self.assertEqual(command[command.index(flag) + 1], value)
        for forbidden in ("-virtfs", "-fsdev", "-drive", "-netdev", "tcg", "-readconfig"):
            self.assertNotIn(forbidden, command)
        self.assertIn("on,obsolete=deny,elevateprivileges=deny,spawn=deny,resourcecontrol=deny", command)

    def test_archive_refuses_escape(self):
        for path in ("/init", "../init", "a/../../init", "TRAILER!!!"):
            with self.subTest(path=path), self.assertRaises(ValueError):
                guest.archive({path: (0o755, b"binary")})

    def test_archive_has_only_guest_console_device(self):
        archive = guest.archive({"init": (0o755, b"binary")})
        entries = {}
        offset = 0
        while True:
            self.assertEqual(archive[offset:offset + 6], b"070701")
            fields = [int(archive[offset + 6 + n * 8:offset + 14 + n * 8], 16) for n in range(13)]
            offset += 110
            name = archive[offset:offset + fields[11] - 1].decode()
            offset = (offset + fields[11] + 3) & ~3
            data = archive[offset:offset + fields[6]]
            offset = (offset + fields[6] + 3) & ~3
            if name == "TRAILER!!!":
                break
            self.assertNotIn(name, entries)
            entries[name] = fields, data
        self.assertEqual(entries["init"][1], b"binary")
        self.assertEqual(entries["dev/console"][0][9:11], [5, 1])
        self.assertNotIn("dev/kvm", entries)


if __name__ == "__main__":
    unittest.main()
