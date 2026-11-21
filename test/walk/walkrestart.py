"""Prove a source step waits through a measured server outage and finishes green."""
import codecs
import json
import selectors
import subprocess
from datetime import datetime
from walkassert import Blocked, Failure, require, workflow_sha
from walkcommands import COMMANDS, SOURCE_LOADER, render
from walkstream import stop
from workflows import SOURCE_STEPS


def pod_sample(pod):
    meta, status = pod["metadata"], pod.get("status", {})
    ready = next((c for c in status.get("conditions", []) if c["type"] == "Ready" and c["status"] == "True"), {})
    containers = status.get("containerStatuses", [])
    exits = [c.get("state", {}).get("terminated", {}).get("finishedAt") for c in containers]
    return {"namespace": meta["namespace"], "name": meta["name"], "uid": meta["uid"],
            "created": meta["creationTimestamp"], "resource_version": meta["resourceVersion"],
            "deleting": meta.get("deletionTimestamp"), "ready": bool(ready) and not meta.get("deletionTimestamp"),
            "ready_at": ready.get("lastTransitionTime"),
            "exit_at": max(exits) if exits and all(exits) else None}


def dependency_start(pod):
    names = [c["name"] for c in pod.get("spec", {}).get("initContainers", [])]
    name = next((n for n in names if n in ("oberth-executor-token", SOURCE_LOADER)), None)
    require(name is not None, "Source Pod has no identifiable server-dependent initializer")
    status = next((c for c in pod.get("status", {}).get("initContainerStatuses", []) if c["name"] == name), {})
    state = status.get("state", {})
    started = state.get("running", state.get("terminated", {})).get("startedAt")
    return {"container": name, "started_at": started} if started else None


class Observation:
    def __init__(self, old, workflow=""):
        self.old_uid, self.exit_at, self.deleted = old["uid"], None, False
        self.servers, self.events = {old["uid"]: old}, []
        self.workflow, self.waiter, self.hold_seconds = workflow, None, 0
        self.restored = None

    def add(self, event):
        body = event["object"]
        pod = pod_sample(body)
        if pod["namespace"] != "oberth":
            if pod["namespace"] == "oberth-argo" and (
                body["metadata"].get("labels", {}).get("workflows.argoproj.io/workflow") == self.workflow
            ):
                self.observe_waiter(body)
            return
        if pod["uid"] == self.old_uid and pod["exit_at"]:
            self.exit_at = pod["exit_at"]
        if event["type"] == "DELETED":
            self.servers.pop(pod["uid"], None)
            self.deleted = self.deleted or pod["uid"] == self.old_uid
        else:
            self.servers[pod["uid"]] = pod
        self.events.append({"type": event["type"], **pod})

    def observe_waiter(self, body):
        observed = dependency_start(body)
        if observed and self.waiter is None:
            self.waiter = {"uid": body["metadata"]["uid"], **observed}

    def replacement(self):
        return next((p for p in self.servers.values() if p["ready"] and p["uid"] != self.old_uid), None)

    def proof(self):
        replacement = self.replacement()
        return {"new_server": replacement, "pod_events": self.events, "outage_start": self.exit_at,
                "outage_end": replacement["ready_at"] if replacement else None,
                "observed_waiter": self.waiter, "hold_seconds": round(self.hold_seconds, 3),
                "replica_restoration": self.restored}


class NativeCommand:
    def __init__(self, session, deadline, operation="pod-watch"):
        self.s, self.deadline, self.operation = session, deadline, operation

    def __enter__(self):
        require(COMMANDS[self.operation].role != "product", "Native inspectors cannot hide product user commands")
        self.s.sequence += 1
        self.stem = self.s.private / f"command-{self.s.sequence:04}"
        self.stdout = self.stem.with_suffix(".stdout").open("wb")
        self.stderr = self.stem.with_suffix(".stderr").open("wb")
        self.argv = render(self.s, self.operation, {})
        self.process = subprocess.Popen(self.argv, stdout=subprocess.PIPE, stderr=self.stderr,
                                        env=self.s.env, start_new_session=True)
        return self

    def events(self):
        decoder, buffer = json.JSONDecoder(), ""
        utf8 = codecs.getincrementaldecoder("utf-8")()
        with selectors.DefaultSelector() as selector:
            selector.register(self.process.stdout, selectors.EVENT_READ)
            while self.s.runtime.clock() < self.deadline:
                if not selector.select(max(0, self.deadline - self.s.runtime.clock())):
                    break
                raw = self.process.stdout.read1(65536)
                if not raw:
                    break
                self.stdout.write(raw)
                buffer += utf8.decode(raw)
                require(len(buffer) < 8 * 1024 * 1024, "Native watch frame exceeds its bound")
                while buffer.strip():
                    buffer = buffer.lstrip()
                    try:
                        event, end = decoder.raw_decode(buffer)
                    except ValueError:
                        break
                    buffer = buffer[end:]
                    if event.get("type") == "BOOKMARK":
                        continue
                    require(event.get("type") in ("ADDED", "MODIFIED", "DELETED"), "Native watch returned an error")
                    yield event

    def __exit__(self, *_):
        stop(self.process)
        self.process.stdout.close()
        self.stdout.close()
        self.stderr.close()
        command = {"step": self.s.current, "argv": self.argv, "exit_code": self.process.returncode,
                   "operation": self.operation, "capture": self.stem.name, "duration_seconds": 0,
                   "stopped_after_observation": True}
        self.s.record(command)
        if self.operation.endswith("-watch") and not self.stem.with_suffix(".stderr").read_bytes().strip():
            command["message"] = "Observed bounded Pod events; only scoped metadata is public"


def restart(s, workflow, old, deadline):
    state = Observation(old, workflow)
    problem = None
    try:
        with NativeCommand(s, deadline) as watch:
            events = watch.events()
            initial = next(events, None)
            if initial is None:
                raise Blocked("Native Pod watch ended before the outage could start")
            state.add(initial)
            s.user("server-scale", {"replicas": 0}, timeout=30)
            wait_for_exit(state, events)
            s.user("workflow-resume", {"workflow": workflow})
            hold_outage(watch, state, events)
    except Failure as error:
        problem = error
        raise
    finally:
        state.restored = restore(s)
        pods = json.loads(s.k("-n", "oberth", "get", "pods", "-o", "json")[1])["items"]
        for pod in pods:
            state.add({"type": "SNAPSHOT", "object": pod})
        if problem is not None:
            problem.facts.update(state.proof())
    return state


def wait_for_exit(state, events):
    for event in events:
        state.add(event)
        if state.exit_at:
            return
        if state.deleted:
            break
    raise Blocked("Server container exit was not observed", {"pod_events": state.events})


def hold_outage(watch, state, events):
    held_since = None
    for event in events:
        state.add(event)
        if state.waiter and held_since is None:
            held_since = watch.s.runtime.clock()
            watch.deadline = min(watch.deadline, held_since + 15)
    if held_since is None:
        raise Blocked("No server-dependent container start was observed", {"pod_events": state.events})
    state.hold_seconds = watch.s.runtime.clock() - held_since
    if state.hold_seconds < 15:
        raise Failure("Server-dependent container outage hold was shorter than 15 seconds",
                      {"waiter": state.waiter, "hold_seconds": state.hold_seconds})


def restore(s):
    try:
        s.user("server-scale", {"replicas": 1}, timeout=30)
    finally:
        try:
            s.k("-n", "oberth", "rollout", "status", "deployment/oberth", "--timeout=180s", timeout=200)
        finally:
            deployment = json.loads(s.k("-n", "oberth", "get", "deployment", "oberth", "-o", "json")[1])
            require(deployment["spec"]["replicas"] == 1 and deployment["status"].get("readyReplicas") == 1,
                    "Server replica restoration was not verified")

    return {"replicas": deployment["spec"]["replicas"], "ready_replicas": deployment["status"]["readyReplicas"]}


def find_workflow(s, sha, deadline):
    with NativeCommand(s, deadline, "workflow-watch") as watch:
        for event in watch.events():
            workflow = event["object"]
            if workflow_sha(workflow) == sha:
                require(workflow["spec"].get("suspend") is True, "Restart fixture was not suspended before observation")
                return workflow
    raise Blocked("No suspended Workflow with the exact source commit was observed")


def run(s, identifier, sha):
    deadline = s.runtime.clock() + s.args.run_timeout
    current = json.loads(s.http("/api/runs/" + identifier))["run"]
    require(current["sha"] == sha, "Restart run does not name the pushed commit")
    before = find_workflow(s, sha, deadline)
    workflow = before["metadata"]["name"]
    old = json.loads(s.k("-n", "oberth", "get", "pods", "-o", "json")[1])["items"]
    ready = [pod_sample(p) for p in old if pod_sample(p)["ready"]]
    require(len(ready) == 1, "Restart inspection requires one ready default server Pod")
    deployment = json.loads(s.k("-n", "oberth", "get", "deployment", "oberth", "-o", "json")[1])
    require(deployment["spec"]["replicas"] == 1, "Restart inspection requires the default single replica")
    state = restart(s, workflow, ready[0], deadline)
    require(state.replacement() is not None, "Server replacement did not become ready")
    facts = {"old_server": ready[0], **state.proof(), "source_commit": sha,
             "workflow_uid_before": before["metadata"]["uid"]}
    remaining = max(1, int(deadline - s.runtime.clock()))
    s.user("restart-run-wait", {"id": identifier, "timeout": remaining}, accept=(0, 1), timeout=remaining + 30)
    result = json.loads(s.http("/api/runs/" + identifier))["run"]
    native = json.loads(s.k("-n", "oberth-argo", "get", "workflow", workflow, "-o", "json")[1])
    facts["restart_run"] = {k: result.get(k) for k in ("id", "sha", "status", "error")}
    facts["workflow_uid"] = native["metadata"]["uid"]
    facts["native_phase"] = native.get("status", {}).get("phase")
    facts["native_nodes"] = {key: {name: node.get(name) for name in
                                 ("id", "type", "displayName", "phase", "children", "message")}
                             for key, node in native.get("status", {}).get("nodes", {}).items()}
    facts["source_checks"] = source_checks(s, native, sha)
    validate(facts, s.runtime.clock() <= deadline)
    return facts


def source_checks(s, native, sha):
    checks = []
    pods = json.loads(s.k("-n", "oberth-argo", "get", "pods", "-o", "json")[1])["items"]
    for pod in pods:
        meta = pod["metadata"]
        if meta.get("labels", {}).get("workflows.argoproj.io/workflow") != native["metadata"]["name"]:
            continue
        require(native["metadata"]["uid"] in [o["uid"] for o in meta.get("ownerReferences", [])],
                "Source Pod belongs to another Workflow UID")
        statuses = pod.get("status", {})
        containers = statuses.get("containerStatuses", []) + statuses.get("initContainerStatuses", [])
        _, output, _ = s.user("restart-step-log", {"pod": meta["name"]}, accept=(0, 1))
        _, load, _ = s.user("restart-load-log", {"pod": meta["name"]}, accept=(0, 1))
        observed = json.loads(s.k("-n", "oberth-argo", "get", "pod", meta["name"], "-o", "json")[1])
        require(observed["metadata"]["uid"] == meta["uid"], "Source Pod UID changed after its load observation")
        starts = [line.split()[0] for line in load.decode().splitlines() if "Downloading artifact" in line]
        waiter = dependency_start(pod) or {}
        checks.append({"uid": meta["uid"], "name": meta["name"], "load_start": starts[0] if starts else None,
                       "waited_container": waiter.get("container"),
                       "waited_started_at": waiter.get("started_at"),
                       "exact_source": ("walk: restart source " + sha + " clean") in output.decode(),
                       "restart_counts": [c.get("restartCount", 0) for c in containers]})
    return checks


def outage_starts(facts):
    start, end = facts["outage_start"], facts["outage_end"]
    if not start or not end:
        raise Blocked("Server container exit and replacement readiness were not both observed", facts)
    def stamp(value):
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    overlap = [c["uid"] for c in facts["source_checks"]
               if c["waited_started_at"] and stamp(start) <= stamp(c["waited_started_at"]) < stamp(end)]
    facts["outage_starts"] = overlap
    if not overlap:
        raise Blocked("No server-dependent container started inside the server outage", facts)


def validate(facts, within_deadline):
    outage_starts(facts)
    try:
        require(facts["workflow_uid"] == facts["workflow_uid_before"], "Restart run changed its Workflow UID")
        require(facts["restart_run"]["status"] == "passed",
                "Restart source run is not green: " + str(facts["restart_run"].get("error")))
        require(facts["native_phase"] == "Succeeded", "Restart Workflow is not green")
        nodes = list(facts["native_nodes"].values())
        require(not any(n.get("type") == "Retry" and len(n.get("children") or []) > 1 for n in nodes),
                "Restart run spent native retries")
        require(
            len([n for n in nodes if n.get("type") == "Pod"]) == SOURCE_STEPS,
            "Restart run did not execute every source step",
        )
        require(len(facts["source_checks"]) == SOURCE_STEPS and all(c["exact_source"] for c in facts["source_checks"]),
                "Not every restart step fetched the exact source commit")
        require(not any(n for c in facts["source_checks"] for n in c["restart_counts"]),
                "Restart source containers restarted")
        require(facts["replica_restoration"] == {"replicas": 1, "ready_replicas": 1},
                "Restart server replica restoration was not verified")
        require(within_deadline, "Restart source run exceeded its deadline")
    except Failure as error:
        raise Failure(str(error), facts) from error
