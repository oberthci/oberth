"""Remember private credentials and retain the minimal warm-slot state."""
import json
import shlex
import shutil
from walkevidence import write_json
from walkassert import digest


def remember(s):
    for path in s.private.glob("*"):
        if path.is_file() and (path.suffix == ".key" or path.name in ("upstream", "admin-push", "agent-push")):
            s.secrets.append(path.read_text())
        if path.suffix == ".secrets":
            for line in path.read_text().splitlines():
                if "=" in line:
                    s.secrets.extend(shlex.split(line.split("=", 1)[1]))
    invitation = s.private / "invite.json"
    if invitation.exists():
        s.secrets.append(json.loads(invitation.read_text())["secret"])


def preserve_warm(s):
    if not s.data.installed:
        return
    state = s.config.path("state")
    source = s.private / "install.secrets"
    if not source.is_file() or "OPENBAO_ROOT_TOKEN=" not in source.read_text():
        source = s.data.recovery
    destination = state / (s.config.slot + ".secrets")
    if source != destination:
        shutil.copyfile(source, destination)
    destination.chmod(0o600)
    for suffix in ("", ".pub"):
        target = state / (s.config.slot + "-upstream" + suffix)
        shutil.copyfile(s.private / ("upstream" + suffix), target)
        target.chmod(0o600)
    uid = json.loads(s.k("get", "namespace", "kube-system", "-o", "json")[1])["metadata"]["uid"]
    write_json(state / (s.config.slot + "-warm.json"), {"cluster_uid": uid, "cli_sha256": digest(s.binary)})


