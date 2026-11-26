"""Coordinator-invoked stages for two candidate installations in one VM slot."""
import hashlib
import json
from pathlib import Path
import re
import tarfile
import urllib.parse
from walkassert import Blocked, digest, require
from walkcandidate import target as candidate_target
from walktwoacceptance import controllers, schedule
from walkevidence import write_json
import walksetup

CRDS = {name + ".argoproj.io" for name in (
    "clusterworkflowtemplates", "cronworkflows", "workflowartifactgctasks", "workfloweventbindings",
    "workflows", "workflowtaskresults", "workflowtasksets", "workflowtemplates")}


def configuration(s):
    value = s.config.values.get("two_installations", {})
    instances = value.get("instances", [])
    require(len(instances) == 2, "Supply two installation descriptions")
    require([i.get("name") for i in instances] == ["first", "second"], "Name installations first and second")
    spaces = [i.get(k, "") for i in instances for k in ("namespace", "pipeline_namespace")]
    argo = value.get("argo", {})
    spaces.append(argo.get("namespace", ""))
    require(len(set(spaces)) == 5 and all(re.fullmatch(r"[a-z][a-z0-9-]{0,62}", n) for n in spaces),
            "Choose five distinct valid namespaces")
    require(not set(spaces) & {"default", "kube-system", "kube-public", "kube-node-lease"},
            "Use disposable namespaces")
    require(instances[0]["namespace"] == "oberth", "First installation must exercise default collisions")
    ports = [i.get(k) for i in instances for k in ("https_node_port", "ssh_node_port")]
    require(all(type(p) is int and 30000 <= p <= 32767 for p in ports) and len(set(ports)) == 4,
            "Choose four distinct NodePorts")
    require(ports[:2] == [30443, 30022], "First installation must use default ports")
    for instance in instances:
        url = urllib.parse.urlsplit(instance.get("url", ""))
        require(url.scheme == "https" and url.hostname in ("localhost", "127.0.0.1") and url.port
                and not url.username and not url.password and url.path in ("", "/")
                and not url.query and not url.fragment, "Use an explicit loopback HTTPS forward")
        require(type(instance.get("ssh_port")) is int and 0 < instance["ssh_port"] < 65536,
                "Supply a forwarded SSH port")
    forwards = [p for i in instances for p in (urllib.parse.urlsplit(i["url"]).port, i["ssh_port"])]
    require(len(set(forwards)) == 4, "Use four distinct client forwards")
    require(re.fullmatch(r"[a-z][a-z0-9-]{0,52}", argo.get("release", "")), "Name the independent Argo release")
    return value


def capped_scope():
    relative = next((line.split(":", 2)[2] for line in Path("/proc/self/cgroup").read_text().splitlines()
                     if line.startswith("0::")), None)
    require(relative is not None, "Run in a memory-capped cgroup v2 scope")
    root = Path("/sys/fs/cgroup")
    current = root / relative.lstrip("/")
    # Walk ancestors because a scope may contain nested process cgroups.
    for directory in (current, *current.parents):
        if not directory.is_relative_to(root):
            break
        maximum, swap = directory / "memory.max", directory / "memory.swap.max"
        if maximum.is_file() and swap.is_file():
            cap = maximum.read_text().strip()
            if cap.isdecimal() and 0 < int(cap) <= 8 * 1024**3 and swap.read_text().strip() == "0":
                return
    raise Blocked("Use a scope capped at 8 GiB or less with no swap")


def preflight(s):
    configuration(s)
    capped_scope()
    require(s.args.mode == "fresh", "Candidate stages use the provided fresh slot")
    facts = walksetup.preflight(s, tools=("helm", "skopeo"))
    namespaces = json.loads(s.k("get", "namespaces", "-o", "json")[1])["items"]
    if s.args.two_installations == "install":
        require({n["metadata"]["name"] for n in namespaces}
                <= {"default", "kube-system", "kube-public", "kube-node-lease"},
                "Provide a fresh VM cluster slot")
        require(not receipt(s).parent.exists(), "Inspect previous candidate state before another install")
    return facts


def receipt(s):
    return s.config.path("state") / (s.config.slot + "-two") / "receipt.json"


def target(s):
    if s.args.two_installations != "install":
        require(receipt(s).is_file(), "Complete the installation stage first")
        previous = json.loads(receipt(s).read_text())
        require(previous["manifest_sha256"] == digest(s.args.candidate / "SHA256SUMS"),
                "Candidate checksum manifest changed")
    facts = candidate_target(s, fixture=False, publish=s.args.two_installations == "install")
    s.two_candidate = {"commit": facts["candidate_commit"], "cli_sha256": facts["cli_sha256"],
                       "manifest_sha256": facts["manifest_sha256"],
                       "configuration": configuration(s)}
    if s.args.two_installations != "install":
        # Run IDs and commits are supplied after the coordinator pushes the fixtures.
        def identity(value):
            value = json.loads(json.dumps(value))
            for instance in value["configuration"]["instances"]:
                for field in ("run_id", "commit_sha", "repo"):
                    instance.pop(field, None)
            return value
        require(identity(previous) == identity(s.two_candidate), "Candidate or installation configuration changed")
    return facts


def profile_env(s, instance):
    home = s.config.path("state") / (s.config.slot + "-two") / instance["name"]
    return {"HOME": str(home), "XDG_CONFIG_HOME": str(home / ".config"),
            "XDG_CACHE_HOME": str(home / ".cache"), "XDG_DATA_HOME": str(home / ".local/share"),
            "OBERTH_PROFILE": s.config.context + "-" + instance["namespace"]}


def cli(s, instance, arguments, *, install=False, **kwargs):
    return s.user("two-install" if install else "two-read", {"arguments": arguments},
                  env=profile_env(s, instance), **kwargs)


def install_args(s, instance):
    return ["install", "--context", s.config.context, "--chart", str(s.private / "candidate-chart/oberth"),
            "--profile", profile_env(s, instance)["OBERTH_PROFILE"], "--namespace", instance["namespace"],
            "--pipeline-namespace", instance["pipeline_namespace"], "--url", instance["url"],
            "--ssh-port", str(instance["ssh_port"]), "--https-node-port", str(instance["https_node_port"]),
            "--ssh-node-port", str(instance["ssh_node_port"]), "--parallel", "1", "--yes"]


def crds(s):
    items = json.loads(s.user("two-crds")[1])["items"]
    result = {}
    for item in items:
        meta = item["metadata"]
        if meta["name"].endswith(".argoproj.io"):
            result[meta["name"]] = {"uid": meta["uid"], "spec": item["spec"],
                                   "labels": meta.get("labels", {}), "annotations": meta.get("annotations", {})}
    require(CRDS <= set(result), "Expected all eight shared workflow definitions")
    return result


def independent(s):
    argo = configuration(s)["argo"]
    chart = s.private / "candidate-chart/oberth"
    dependencies = list((chart / "charts").glob("argo-workflows-*.tgz"))
    require(len(dependencies) == 1, "Candidate must contain one pinned Argo chart")
    with tarfile.open(dependencies[0]) as archive:
        metadata = archive.extractfile("argo-workflows/Chart.yaml").read().decode()
    version = re.findall(r"^version: ([^\s]+)$", metadata, re.M)
    require(len(version) == 1, "Argo chart version is missing")
    # Keep the independent controller's chart-default cluster-wide scope.
    s.user("two-argo-install", {"release": argo["release"], "chart": dependencies[0],
           "namespace": argo["namespace"], "chart_version": version[0]}, timeout=600)
    s.two_definitions = crds(s)
    require(all(row["annotations"].get("meta.helm.sh/release-name") == argo["release"]
                and row["annotations"].get("meta.helm.sh/release-namespace") == argo["namespace"]
                and row["labels"].get("app.kubernetes.io/managed-by") == "Helm"
                for row in s.two_definitions.values()), "Independent release must own shared definitions")
    return {"chart_version": version[0], "shared_definitions": len(s.two_definitions)}


def install(s, index):
    instance = configuration(s)["instances"][index]
    home = Path(profile_env(s, instance)["HOME"])
    require(not home.exists(), "Start with fresh installation homes")
    home.mkdir(parents=True, mode=0o700)
    baseline = first_identity(s) if index == 1 else None
    cli(s, instance, install_args(s, instance), install=True, timeout=600)
    cli(s, instance, ["install", "--context", s.config.context, "--verify"], timeout=600)
    require(crds(s) == s.two_definitions, "Installation changed shared definitions")
    if baseline is not None:
        require(first_identity(s) == baseline, "Second install changed first identity")
    return {"installed_once": instance["name"], "verified": True}


def first_identity(s):
    first = configuration(s)["instances"][0]
    revision = json.loads(s.user("two-helm-status", {"namespace": first["namespace"]})[1])["version"]
    identity = {"revision": revision}
    for kind, name in (("secret", "oberth-admin"), ("deployment", "oberth")):
        raw = s.user("two-identity", {"namespace": first["namespace"], "kind": kind, "object": name})[1]
        identity[kind] = hashlib.sha256(raw).hexdigest()
    return identity


def default_collision(s):
    baseline = first_identity(s)
    for yes in (False, True):
        # Distinct empty homes and no selected profile exercise both no-answer paths.
        instance = dict(configuration(s)["instances"][1], name="no-answers-" + str(yes))
        env = profile_env(s, instance)
        env.pop("OBERTH_PROFILE")
        home = Path(env["HOME"])
        require(not home.exists(), "Collision probe needs a fresh home")
        home.mkdir(parents=True, mode=0o700)
        command = ["install", "--context", s.config.context, "--chart", str(s.private / "candidate-chart/oberth")]
        code, raw, errors = s.user("two-install", {"arguments": command + (["--yes"] if yes else [])},
                                  env=env, input=b"n\n", timeout=600, accept=(0, 1, 2, 3))
        require(first_identity(s) == baseline, "No-answer install changed first identity")
        require(code in (1, 2, 3) and (raw + errors).strip() and b"Apply? [y/N]" not in raw + errors,
                "No-answer install must refuse before review")
        diagnostic = raw + errors
        flags = (b"--namespace", b"--https-node-port", b"--ssh-node-port")
        require(b"does not own" in diagnostic and all(flag in diagnostic for flag in flags),
                "Expected an installation ownership refusal")
        require(not (home / ".config/oberth").exists(), "Refusal saved an installation profile")
    return {"interactive_and_yes_refused": True, "first_identity_unchanged": True}


def retained(s):
    if s.args.two_installations == "install" and all(row["status"] == "pass" for row in s.results):
        write_json(receipt(s), s.two_candidate)
    return {"installations_retained_for_coordinator": True}


def steps(stage):
    from walklib import Step
    rows = [Step("two-preflight", "Provided VM slot and memory-capped scope are usable", preflight),
            Step("two-target", "Reuse checksum-verified candidate bytes", target, ("two-preflight",))]
    if stage == "install":
        actions = [("independent-controller", "Independent cluster-wide Argo owns shared definitions", independent),
                   ("first-install", "First install succeeds once with a fresh profile", lambda s: install(s, 0)),
                   ("default-collision", "No-answer install refuses without changing first identity", default_collision),
                   ("second-install", "Second install succeeds once without taking shared definitions", lambda s: install(s, 1))]
    else:
        action = controllers if stage == "controllers" else schedule
        actions = [(stage, "Observe exact controller ownership and isolation" if stage == "controllers"
                    else "Authored schedules have two unique native occurrences each", action)]
    for name, assertion, action in actions:
        rows.append(Step(name, assertion, action, (rows[-1].name,), phase="checks"))
    rows.append(Step("cleanup", "Retain installations for the next coordinator stage", retained))
    return tuple(rows)
