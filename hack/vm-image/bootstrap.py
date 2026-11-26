#!/usr/bin/env python3
"""Provision the disposable guest's pinned tools and local walk environment."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import subprocess
import tempfile
import urllib.request


ROOT = Path("/etc/oberth-walk")
HOME = Path("/home/walk")
BIN = Path("/usr/local/bin")
RESOLV = Path("/etc/resolv.conf")
TOOLS = ("curl", "cosign", "docker", "k3d", "kubectl", "git", "ssh", "ssh-keygen", "openssl", "python3", "skopeo")
MAX_TOOL_BYTES = 256 << 20


def run(*args, **kwargs):
    return subprocess.run(args, check=True, capture_output=True, timeout=300, **kwargs).stdout.decode().strip()


def write(path, body, mode=0o600):
    path.write_text(body)
    path.chmod(mode)


def install_tool(pin, directory):
    if not pin["url"].startswith("https://"):
        raise ValueError("Walk tool source must use HTTPS.")
    with urllib.request.urlopen(pin["url"], timeout=60) as source:
        if not source.geturl().startswith("https://"):
            raise ValueError("Walk tool redirect must use HTTPS.")
        body = source.read(MAX_TOOL_BYTES + 1)
    if len(body) > MAX_TOOL_BYTES or hashlib.sha256(body).hexdigest() != pin["sha256"]:
        raise ValueError("Walk tool checksum differs: " + pin["name"])
    if pin.get("format") == "deb":
        # Native Git/SSH uses the same distribution's libc, PCRE and zlib
        # already present in the pinned guest; no package resolver runs.
        with tempfile.TemporaryDirectory(dir=directory) as temporary:
            package = Path(temporary) / "tool.deb"
            package.write_bytes(body)
            run("dpkg-deb", "--extract", str(package), "/")
        return
    path = directory / pin["name"]
    path.write_bytes(body)
    path.chmod(0o755)


def environment(images, gateway):
    return {"state": "state", "private": "private", "docker_config": str(HOME / ".docker"),
            "docker_context": "walk", "cluster_image": images["cluster"],
            "registry_name": "k3d-walk-registry", "registry_http": "http://127.0.0.1:5001",
            "registry_inside": "k3d-walk-registry:5000", "upstream_api": "http://127.0.0.1:3300/api/v1/",
            "upstream_url": "ssh://git@" + gateway + ":33022/oberthci", "upstream_host": "127.0.0.1",
            "upstream_port": 33022, "upstream_owner": "oberthci", "upstream_token": "private/upstream.auth",
            "known_hosts": "private/known_hosts", "signing_key": "private/signing.key",
            "signing_public": "private/signing.pub", "signing_password": "private/signing.password"}


def registry_command(image):
    return ("k3d", "registry", "create", "walk-registry", "--port", "127.0.0.1:5001",
            "--image", image, "--delete-enabled", "--no-help")


def check(root=None):
    root = root or ROOT
    for tool in TOOLS:
        if not shutil.which(tool):
            raise ValueError("Install the required executable: " + tool)
    config = json.loads((root / "environment.json").read_text())
    for name in ("upstream_token", "known_hosts", "signing_key", "signing_public", "signing_password"):
        if not (root / config[name]).is_file():
            raise ValueError("Provide environment file: " + name)
    return config


def provision(pin):
    images = pin["images"]
    ROOT.mkdir(mode=0o755, exist_ok=True)
    private = ROOT / "private"
    private.mkdir(mode=0o700, exist_ok=True)
    (ROOT / "state").mkdir(mode=0o700, exist_ok=True)
    # The pinned minimal guest has no system resolver daemon. Give its
    # namespace a resolver before downloading the reviewed provisioning bytes.
    if RESOLV.is_symlink():
        RESOLV.unlink()
    write(RESOLV, "".join("nameserver " + address + "\n" for address in pin["nameservers"]), 0o644)
    for tool in pin["tools"]:
        install_tool(tool, BIN)
    run("git", "--version")
    run("systemctl", "start", "docker")
    docker_config = HOME / ".docker"
    docker_config.mkdir(mode=0o700, exist_ok=True)
    run("docker", "--config", str(docker_config), "context", "create", "walk", "--docker", "host=unix:///var/run/docker.sock")
    for image in images.values():
        run("docker", "pull", image)
    # The cluster command consumes these fixed helper tags. Their bytes were
    # pulled by digest above; no tag resolution is needed by the local daemon.
    for name in ("tools", "proxy"):
        run("docker", "tag", images[name], images[name].split("@")[0])
    write(BIN / "skopeo", "#!/bin/sh\nexec docker --context walk run --rm --network host -v /home/walk/candidate:/home/walk/candidate:ro " + images["skopeo"] + ' "$@"\n', 0o755)
    run(*registry_command(images["registry"]))
    gateway = run("docker", "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}")
    provision_upstream(images["upstream"], gateway, private)
    password = secrets.token_urlsafe(32)
    write(private / "signing.password", password)
    run("cosign", "generate-key-pair", "--output-key-prefix", str(private / "signing"), env={**os.environ, "COSIGN_PASSWORD": password})
    write(ROOT / "environment.json", json.dumps(environment(images, gateway)))
    run("chown", "-R", "walk:walk", str(ROOT), str(docker_config))
    check()


def provision_upstream(image, gateway, private):
    run("docker", "run", "-d", "--name", "walk-upstream", "-p", "3300:3000", "-p", "33022:22",
        "-e", "FORGEJO__security__INSTALL_LOCK=true", "-e", "FORGEJO__database__DB_TYPE=sqlite3",
        "-e", "FORGEJO__service__DISABLE_REGISTRATION=true", "-e", "FORGEJO__server__SSH_PORT=33022",
        "-e", "FORGEJO__server__SSH_DOMAIN=" + gateway, "-e", "FORGEJO__server__ROOT_URL=http://" + gateway + ":3300/", image)
    # Wait on service readiness with a bounded native client, before the admin CLI.
    run("curl", "--fail", "--silent", "--retry", "60", "--retry-all-errors", "--retry-delay", "1", "--max-time", "2", "http://127.0.0.1:3300/api/healthz")
    run("docker", "exec", "-u", "git", "walk-upstream", "forgejo", "admin", "user", "create",
        "--username", "oberthci", "--email", "ci@oberth.ci", "--admin", "--random-password", "--must-change-password=false")
    value = run("docker", "exec", "-u", "git", "walk-upstream", "forgejo", "admin", "user", "generate-access-token",
                "--username", "oberthci", "--token-name", "walk", "--scopes", "all", "--raw")
    write(private / "upstream.auth", value)
    host = run("docker", "exec", "walk-upstream", "cat", "/data/ssh/ssh_host_ed25519_key.pub").split()
    write(private / "known_hosts", "[" + gateway + "]:33022 " + " ".join(host[:2]) + "\n[127.0.0.1]:33022 " + " ".join(host[:2]) + "\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check", action="store_true")
    args = parser.parse_args()
    if args.check:
        check()
    else:
        provision(json.loads((ROOT / "pins.json").read_text()))


if __name__ == "__main__":
    main()
