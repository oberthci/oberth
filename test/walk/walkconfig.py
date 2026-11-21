"""Read the externally supplied, loopback-only walk environment."""
from dataclasses import dataclass
import json
from pathlib import Path
import urllib.parse
from walkassert import require


@dataclass
class Environment:
    values: dict
    root: Path
    slot: str

    @classmethod
    def load(cls, path, slot):
        values = json.loads(path.read_text())
        require(slot in ("walk-1", "walk-2"), "Choose --slot walk-1 or walk-2")
        for key in (
            "state",
            "private",
            "docker_config",
            "upstream_token",
            "known_hosts",
            "signing_key",
            "signing_public",
            "signing_password",
            "upstream_api",
            "registry_http",
            "registry_name",
            "registry_inside",
            "docker_context",
            "cluster_image",
            "upstream_url",
            "upstream_host",
            "upstream_owner",
        ):
            require(isinstance(values.get(key), str) and values[key], "Environment needs " + key)
        for key in ("upstream_api", "registry_http"):
            parsed = urllib.parse.urlsplit(values[key])
            require(parsed.scheme == "http" and parsed.hostname in ("localhost", "127.0.0.1"),
                    key + " must name the local VM forward")
        require(values["upstream_host"] in ("localhost", "127.0.0.1"), "SSH must use the local VM forward")
        require(type(values.get("upstream_port")) is int and 0 < values["upstream_port"] < 65536,
                "Environment needs an upstream_port between 1 and 65535")
        require(urllib.parse.urlsplit("//" + values["registry_inside"]).port is not None,
                "registry_inside must include the port reachable by Pods")
        return cls(values, path.resolve().parent, slot)

    def path(self, key):
        return (self.root / Path(self.values[key]).expanduser()).resolve()

    def __getitem__(self, key):
        return self.values[key]

    @property
    def context(self):
        return "k3d-" + self.slot

    @property
    def ports(self):
        number = int(self.slot[-1])
        return {"api": 6550 + number, "ssh": 33022 + 1000 * number, "https": 33443 + 1000 * number}

    @property
    def kube(self):
        return self.path("state") / (self.slot + ".kubeconfig")

    @property
    def base(self):
        return "https://localhost:" + str(self.ports["https"])
