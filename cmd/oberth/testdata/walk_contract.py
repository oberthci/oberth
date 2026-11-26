"""Exercise walk code for the Go CLI's installation and formatter contracts."""
import json
from pathlib import Path
import sys
from types import SimpleNamespace

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "test/walk"))
import walktwo
from walkconfig import Environment
from walkmessages import messages


def installation_commands(root):
    instances = []
    for name, namespace, offset in (("first", "oberth", 0), ("second", "second", 1000)):
        instances.append({"name": name, "namespace": namespace, "pipeline_namespace": namespace + "-runs",
                          "url": "https://localhost:" + str(34443 + offset), "ssh_port": 34022 + offset,
                          "https_node_port": 30443 + offset, "ssh_node_port": 30022 + offset})
    config = Environment({"state": str(root), "two_installations": {"instances": instances,
                         "argo": {"namespace": "independent", "release": "plain"}}}, root, "walk-1")
    items = [{"metadata": {"name": name, "uid": name}, "spec": {}} for name in sorted(walktwo.CRDS)]
    calls = []

    def user(name, values=None, **kwargs):
        if name in ("two-install", "two-read"):
            calls.append(values["arguments"])
        return 0, json.dumps({"items": items, "version": 1}).encode(), b""

    session = SimpleNamespace(config=config, private=root, user=user)
    session.two_definitions = walktwo.crds(session)
    for index in (0, 1):
        walktwo.install(session, index)
    return calls


if __name__ == "__main__":
    if sys.argv[1] == "profiles":
        print(json.dumps(installation_commands(Path(sys.argv[2]))))
    else:
        print(json.dumps([messages(text.encode()) for text in json.load(sys.stdin)]))
