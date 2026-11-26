#!/usr/bin/env python3
"""Check provisioning against the exact pinned k3d parser, without a daemon."""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess


HERE = Path(__file__).resolve().parent


def check(binary):
    pins = json.loads((HERE.parents[1] / ".oberth/pins/walk-tools.json").read_text())
    pin = next(tool for tool in pins["tools"] if tool["name"] == "k3d")
    if hashlib.sha256(binary.read_bytes()).hexdigest() != pin["sha256"]:
        raise ValueError("k3d executable checksum differs.")
    spec = importlib.util.spec_from_file_location("bootstrap", HERE / "bootstrap.py")
    bootstrap = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(bootstrap)
    command = [str(binary.resolve()), *bootstrap.registry_command(pins["images"]["registry"])[1:]]
    if "--delete-enabled" not in command:
        raise ValueError("Registry command must enable deletion.")
    # Cobra parses flags before help, so this checks the production arguments
    # without running provisioning or contacting a container daemon.
    result = subprocess.run([*command, "--help"], check=True, capture_output=True, timeout=30)
    if b"--delete-enabled" not in result.stdout:
        raise ValueError("Pinned parser lacks registry deletion.")
    control = subprocess.run([*command, "--config", "unused", "--help"], capture_output=True, timeout=30)
    if control.returncode == 0 or b"unknown flag: --config" not in control.stderr:
        raise ValueError("Unsupported-flag control did not fail.")
    return {"version": pin["version"], "sha256": pin["sha256"], "command": command,
            "parser": "PASS", "unsupported_config_control": "PASS"}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--k3d", required=True, type=Path, help="Locally downloaded pinned executable")
    print(json.dumps(check(parser.parse_args().k3d), indent=2))
