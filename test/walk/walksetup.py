"""Prepare the provided slot, signed release and disposable upstream fixture."""
import fcntl
import hashlib
import io
import json
from pathlib import Path
import re
import shlex
import shutil
import ssl
import tarfile
import urllib.parse
from walkevidence import write_json
from walkassert import Blocked, HERE, TEST_EMAIL, TEST_IDENTITY, TEST_NAME, digest, require
from workflows import fixtures
from walkcleanup import registry_ready


def preflight(s, *, tools=()):
    config = s.config
    load = s.runtime.load.sample()
    if load[0] >= 16:
        raise Blocked("Host 1-minute load is above the guard; retry below 16")
    config.path("state").mkdir(parents=True, exist_ok=True, mode=0o700)
    s.lock = (config.path("state") / (config.slot + ".lock")).open("a")
    try:
        fcntl.flock(s.lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError as error:
        raise Blocked("Slot is owned by another walk; wait for that invocation to finish") from error
    for tool in ("curl", "cosign", "docker", "k3d", "kubectl", "git", "ssh", "ssh-keygen", "openssl", *tools):
        if not shutil.which(tool, path=s.env["PATH"]):
            raise Blocked("Install the required executable: " + tool)
    for key in ("upstream_token", "known_hosts", "signing_key", "signing_public", "signing_password"):
        if not config.path(key).is_file():
            raise Blocked("Provide environment file: " + key)
    try:
        s.d("info", "--format", "{{.ServerVersion}}")
    except Exception as error:
        raise Blocked("Provide the running configured VM context before retrying: " + str(error)) from error
    registry_ready(s)
    return {"slot": config.slot, "private_id": s.private.name, "load_1m_start": load[0]}


def target(s):
    if getattr(s.args, "candidate", None) is not None:
        from walkcandidate import target as candidate
        return candidate(s)
    version = s.args.version
    require(re.fullmatch(r"v\d+\.\d+\.\d+", version), "Select an exact signed --version")
    source = (HERE.parent.parent / "internal/releaseverify/origin.go").read_text()
    origins = re.findall(r'^const Origin = "([^"\n]+)"$', source, re.M)
    require(len(origins) == 1, "Public release address is missing or duplicated")
    for filename in ("SHA256SUMS", "SHA256SUMS.sigstore.json", "oberth-linux-amd64"):
        s.run(
            [
                "curl",
                "--proto",
                "=https",
                "--tlsv1.2",
                "--fail",
                "--silent",
                "--show-error",
                "--output",
                s.private / filename,
                f"{origins[0]}/{version}/{filename}",
            ],
            timeout=240,
        )
    sums = s.private / "SHA256SUMS"
    s.run(["cosign", "verify-blob", "--key", HERE.parent.parent / "internal/releaseverify/release-cosign.pub", "--insecure-ignore-tlog",
           "--bundle", s.private / "SHA256SUMS.sigstore.json", sums])
    expected = [line.split()[0] for line in sums.read_text().splitlines()
                if line.split() and line.split()[-1] in ("oberth-linux-amd64", "./oberth-linux-amd64")]
    binary = s.private / "oberth-linux-amd64"
    require(len(expected) == 1 and digest(binary) == expected[0], "CLI differs from its signed checksum manifest")
    s.data.cli_sha256 = digest(binary)
    binary.rename(s.binary)
    s.binary.chmod(0o700)
    require(
        version in s.run([s.binary, "version"], display=["oberth", "version"])[1].decode(),
        "CLI reports a different release version",
    )
    tools_image(s)
    return {"cli_sha256": digest(s.binary), "version": version, "fixture_image": s.data.tools_image}


def tools_image(s):
    context = io.BytesIO()
    with tarfile.open(fileobj=context, mode="w:gz", dereference=True) as archive:
        archive.add(HERE / "tools.Dockerfile", arcname="Dockerfile")
        archive.add(s.binary, arcname="oberth")
        archive.add(Path(shutil.which("cosign", path=s.env["PATH"])), arcname="cosign")
    s.data.tools_repository = "walk-tools/" + s.data.name
    s.data.image_repository = "walk-release/" + s.data.name
    host = urllib.parse.urlsplit(s.config["registry_http"]).netloc
    tag = host + "/" + s.data.tools_repository + ":fixture"
    s.d("build", "--platform", "linux/amd64", "-t", tag, "-", input=context.getvalue(), timeout=900)
    s.d("push", tag, timeout=180)
    inspection = json.loads(s.d("image", "inspect", tag)[1])[0]
    require(inspection["Config"].get("User", "") in ("", "0", "0:0", "root"),
            "Fixture must retain the image default root user so writable source cannot pass")
    pinned = next(
        ref for ref in inspection["RepoDigests"] if ref.startswith(host + "/" + s.data.tools_repository + "@")
    )
    s.data.tools_image = pinned.replace(host + "/", s.config["registry_inside"] + "/", 1)


def cluster(s):
    c = s.config
    if s.args.mode == "warm":
        return reset_warm(s)
    s.run(["k3d", "cluster", "delete", c.slot], env={"KUBECONFIG": str(s.private / "unused.kubeconfig")})
    p = c.ports
    registry_port = urllib.parse.urlsplit("//" + c["registry_inside"]).port
    s.run(["k3d", "cluster", "create", c.slot, "--image", c["cluster_image"], "--servers", "1", "--agents", "0",
           "--api-port", f"127.0.0.1:{p['api']}", "--registry-use", c["registry_name"] + ":" + str(registry_port),
           "--port", f"127.0.0.1:{p['ssh']}:30022@server:0", "--port", f"127.0.0.1:{p['https']}:30443@server:0",
           "--kubeconfig-update-default=false", "--kubeconfig-switch-context=false", "--timeout", "180s"], timeout=240)
    c.kube.write_bytes(s.run(["k3d", "kubeconfig", "get", c.slot])[1])
    c.kube.chmod(0o600)
    s.k("wait", "node", "--all", "--for=condition=Ready", "--timeout=180s", timeout=200)
    namespaces = json.loads(s.k("get", "namespaces", "-o", "json")[1])["items"]
    require({n["metadata"]["name"] for n in namespaces} <= {"default", "kube-system", "kube-public", "kube-node-lease"},
            "Fresh slot contains non-stock namespaces; recreate the isolated slot")
    for suffix in (".secrets", "-upstream", "-upstream.pub", "-warm.json"):
        (c.path("state") / (c.slot + suffix)).unlink(missing_ok=True)
    return {"stock_namespaces": True}


def reset_warm(s):
    marker = s.config.path("state") / (s.config.slot + "-warm.json")
    if not marker.exists():
        raise Blocked("Warm mode requires a preceding fresh install in the same slot")
    warm = json.loads(marker.read_text())
    uid = json.loads(s.k("get", "namespace", "kube-system", "-o", "json")[1])["metadata"]["uid"]
    require(uid == warm["cluster_uid"] and digest(s.binary) == warm["cli_sha256"],
            "Warm state differs from cluster/release; run fresh first")
    s.data.recovery = s.config.path("state") / (s.config.slot + ".secrets")
    s.k("-n", "oberth-argo", "rollout", "status", "deployment", "--timeout=120s", timeout=150)
    s.user("namespace-delete", timeout=200)
    s.user("workflows-delete", timeout=150)
    return {"platform_preserved": True, "cluster_uid": uid}


def keys(s):
    for name in ("upstream", "admin-push", "agent-push"):
        previous = s.config.path("state") / (s.config.slot + "-upstream")
        if name == "upstream" and s.args.mode == "warm":
            shutil.copyfile(previous, s.private / name)
            shutil.copyfile(Path(str(previous) + ".pub"), s.private / (name + ".pub"))
        else:
            s.user("key", {"key": name})
    for name in ("admin", "agent"):
        s.user("csr", {"client": name})


def fixture(s):
    keys(s)
    public = (s.private / "upstream.pub").read_text()
    registered = s.upstream("user/keys", {"title": s.data.name, "key": public})
    s.data.key_id = registered["id"]
    s.ux.event(s.current, "manual", "Authorize the supplied upstream public key")
    s.data.repository_created = True
    repository = s.upstream("user/repos", {"name": s.data.name, "private": True, "auto_init": False})
    host = urllib.parse.urlsplit(s.config["upstream_url"])
    alias = "[" + host.hostname + "]:" + str(host.port)
    (s.private / "upstream.conf").write_text("Host *\n  IdentityFile " + shlex.quote(str(s.private / "upstream"))
        + "\n  UserKnownHostsFile " + shlex.quote(str(s.config.path("known_hosts"))) + "\n  HostKeyAlias " + alias
        + "\n  HostName " + s.config["upstream_host"] + "\n  Port " + str(s.config["upstream_port"])
        + "\n  StrictHostKeyChecking yes\n  IdentitiesOnly yes\n  BatchMode yes\n  ConnectTimeout 10\n")
    s.admin_repo.mkdir()
    s.user("git-init")
    s.user("git-name")
    s.user("git-email")
    s.user("git-origin", {"origin": repository["ssh_url"]})
    s.user("git-initialize")
    s.user("git-seed", {"remote": "origin", "ref": "main"}, upstream=True)
    shutil.copytree(
        HERE / "fixture", s.admin_repo, dirs_exist_ok=True, ignore=shutil.ignore_patterns("__pycache__", "*.pyc")
    )
    (s.admin_repo / "source-marker").write_text(s.data.marker + "\n")
    s.data.documents = fixtures(s.data.tools_image, s.data.marker, s.config["registry_inside"], s.data.image_repository)
    (s.admin_repo / ".oberth").mkdir()
    for phase in ("build", "release"):
        write_json(s.admin_repo / ".oberth" / (phase + ".yaml"), s.data.documents[phase])
    shutil.copyfile(s.config.path("signing_public"), s.admin_repo / "signing.pub")
    s.user("git-add", {"path": "."})
    s.user("git-commit", {"message": "Exercise pushed source and signed releases"})
    s.data.sha = s.g("rev-parse", "HEAD")[1].decode().strip()
    return {"repository": s.canonical, "commit_sha": s.data.sha}


def install(s):
    fingerprint = s.user("fingerprint")[1].decode().split()[1]
    resume = ["--resume-secrets-file", s.data.recovery] if s.data.recovery else []
    if getattr(s.args, "candidate", None) is not None:
        resume += ["--local-chart", str(s.private / "candidate-chart/oberth")]
    code, raw, _ = s.user("install", {"fingerprint": fingerprint, "resume": resume}, timeout=1800, accept=(0, 3))
    (s.private / "install.json").write_bytes(raw)
    installed = json.loads(raw)
    failed = [c["code"] for c in installed["checks"] if not c["ok"] and not c.get("warning")]
    require(
        failed == ([] if code == 0 else ["upstream_key_pending"]),
        "Install checks failed: " + json.dumps(installed["checks"]),
    )
    ca = s.home / ".config/oberth/installer-ca.crt"
    require(hashlib.sha256(ssl.PEM_cert_to_DER_cert(ca.read_text())).hexdigest() == installed["ca_sha256"],
            "Saved client trust differs from the installer CA pin")
    s.data.installed = True
    save_profile(s, agent=False)
    if code == 3:
        s.user("upstream", {"fingerprint": fingerprint})
    return {"server_url": installed["server_url"], "documented_upstream_followup": code == 3}


def save_profile(s, agent):
    name = "agent" if agent else "admin"
    profile = Path(s.profile(agent)["XDG_CONFIG_HOME"]) / "oberth"
    profile.mkdir(parents=True, exist_ok=True, mode=0o700)
    values = {"OBERTH_BASE_URL": s.config.base, "OBERTH_CLIENT_CERT": s.private / (name + ".csr.crt"),
              "OBERTH_CLIENT_KEY": s.private / (name + ".key"),
              "OBERTH_CA_CERT": s.private / "agent.csr.crt.ca.crt" if agent else profile / "installer-ca.crt"}
    path = profile / "env"
    s.user(
        "profile",
        {"profile_file": path},
        input="".join(f"export {k}={shlex.quote(str(v))}\n" for k, v in values.items()).encode(),
    )
    path.chmod(0o600)
    s.ux.event(s.current, "manual", "Save the " + name + " certificate connection settings once in its own profile")


def login(s):
    catalog = s.mcp("uplink_list", {})
    require(isinstance(catalog.get("uplinks"), list), "Authenticated uplink catalog is missing its collection")
    return {"saved_profile": True, "administrator_catalog_readable": True}


def bind(s, agent=False):
    name = "agent" if agent else "admin"
    ca = s.private / "agent.csr.crt.ca.crt" if agent else s.home / ".config/oberth/installer-ca.crt"
    result = s.uj("bind", {"repo": s.agent_repo if agent else s.admin_repo, "client": name, "ca": ca}, agent=agent)
    require(result["configured"], "Repository binding did not configure the remote")


def connect(s):
    s.user("repo")
    bind(s)
    return {"repository_bound": True}


def onboard(s):
    invitation = s.private / "invite.json"
    s.user("invite")
    s.secrets.append(json.loads(invitation.read_text())["secret"])
    s.user("redeem", agent=True)
    save_profile(s, agent=True)
    s.user("git-clone")
    s.user("git-name", agent=True)
    s.user("git-email", agent=True)
    require(s.g("rev-parse", "HEAD", agent=True)[1].decode().strip() == s.data.sha,
            "Invited checkout differs from the authored source")
    bind(s, agent=True)
    require(isinstance(s.uj("runs", agent=True).get("runs"), list),
            "Redeemed client profile cannot read its repository runs")
    return {"certificate_issued": True, "saved_agent_profile": True}
