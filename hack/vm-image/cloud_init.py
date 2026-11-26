#!/usr/bin/env python3
"""Embed the admitted guest provisioning inputs in cloud-init."""
import argparse
import json
from pathlib import Path


def document(public, root):
    files = {"/etc/oberth-walk/bootstrap.py": (root / "hack/vm-image/bootstrap.py").read_text(),
             "/etc/oberth-walk/pins.json": (root / ".oberth/pins/walk-tools.json").read_text()}
    return {"users": [{"name": "walk", "groups": ["docker"], "shell": "/bin/bash", "lock_passwd": True,
                       "sudo": "ALL=(ALL) NOPASSWD:ALL", "ssh_authorized_keys": [public.strip()]}],
            "ssh_pwauth": False, "disable_root": True, "resize_rootfs": True,
            "growpart": {"mode": "auto", "devices": ["/"]},
            "write_files": [{"path": path, "permissions": "0644", "content": body} for path, body in files.items()],
            "runcmd": [["python3", "/etc/oberth-walk/bootstrap.py"]]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--public", required=True, type=Path)
    args = parser.parse_args()
    print("#cloud-config")
    print(json.dumps(document(args.public.read_text(), Path(__file__).resolve().parents[2])))


if __name__ == "__main__":
    main()
