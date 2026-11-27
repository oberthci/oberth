#!/usr/bin/env python3
"""Read-only, allowlisted capacity observations; never an execution owner."""
import argparse
import datetime as dt
import hashlib
import json
import re
import subprocess
import sys
import tempfile
import time

MAX_FRAME = 32 * 1024 * 1024


def items(value):
    return value.get("items", []) if isinstance(value, dict) else []


def identity(obj):
    meta = obj.get("metadata", {})
    return {k: meta.get(k) for k in ("namespace", "name", "uid", "resourceVersion", "creationTimestamp")}


def pick(value, names):
    return {k: value[k] for k in names if k in value}


def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


def observation(frame):
    """No environment, commands, manifests, annotations or event messages escape."""
    workflows = {w["metadata"]["uid"]: w for w in items(frame.get("workflows")) if w.get("metadata", {}).get("uid")}
    objects = [*workflows.values(), *items(frame.get("pods")), *items(frame.get("owners"))]
    owners = {o["metadata"]["uid"]: o for o in objects if o.get("metadata", {}).get("uid")}
    pvcs = {(p.get("metadata", {}).get("namespace"), p.get("metadata", {}).get("name")): p for p in items(frame.get("pvcs"))}
    stats = {p.get("podRef", {}).get("uid"): p for p in frame.get("stats", {}).get("pods", [])}
    rows, runs = [], []
    for w in workflows.values():
        labels, spec, status = w.get("metadata", {}).get("labels", {}), w.get("spec", {}), w.get("status", {})
        runs.append({"workflow": identity(w), "repo": labels.get("oberth.ci/repo"), "commit": labels.get("oberth.ci/sha"),
                     "spec_identity": w.get("metadata", {}).get("annotations", {}).get("oberth.ci/spec-identity"),
                     "entrypoint": spec.get("entrypoint"), "priority": spec.get("priority", 0),
                     "pod_priority_class": spec.get("podPriorityClassName"), "parallelism": spec.get("parallelism"),
                     "phase": status.get("phase"), "started_at": status.get("startedAt"), "finished_at": status.get("finishedAt")})
    for p in items(frame.get("pods")):
        meta, spec, status = p.get("metadata", {}), p.get("spec", {}), p.get("status", {})
        missing, current, seen = [], p, set()
        while current.get("metadata", {}).get("uid") not in workflows:
            refs = [r for r in current.get("metadata", {}).get("ownerReferences", []) if r.get("controller")]
            uid = refs[0].get("uid") if len(refs) == 1 else None
            if not uid or uid in seen or uid not in owners:
                current = {}
                break
            seen.add(uid)
            parent = owners[uid]
            if parent.get("metadata", {}).get("namespace") != meta.get("namespace"):
                current = {}
                break
            current = parent
        w = workflows.get(current.get("metadata", {}).get("uid"), {})
        labels = w.get("metadata", {}).get("labels", {})
        node_id = meta.get("annotations", {}).get("workflows.argoproj.io/node-id")
        node = w.get("status", {}).get("nodes", {}).get(node_id, {})
        usage = stats.get(meta.get("uid"), {})
        if not w:
            missing.append("workflow_uid_join")
        claims = []
        for volume in spec.get("volumes", []):
            claim = volume.get("persistentVolumeClaim", {}).get("claimName")
            if claim:
                pvc = pvcs.get((meta.get("namespace"), claim), {})
                claims.append({"volume": volume.get("name"), "claim_name": claim, "pvc": identity(pvc)})
                if not pvc:
                    missing.append("pvc_uid:" + volume.get("name", ""))
        for field, val in (("repo", labels.get("oberth.ci/repo")), ("commit", labels.get("oberth.ci/sha")),
                           ("template", node.get("templateName")), ("pod_uid", meta.get("uid")),
                           ("pod_spec", spec.get("containers")),
                           ("usage", usage)):
            if not val:
                missing.append(field)
        containers = []
        by_name = {c.get("name"): c for c in usage.get("containers", [])}
        for carrier, statuses in (("containers", "containerStatuses"), ("initContainers", "initContainerStatuses"), ("ephemeralContainers", "ephemeralContainerStatuses")):
            state = {c.get("name"): c for c in status.get(statuses, [])}
            for c in spec.get(carrier, []):
                name = c.get("name")
                measured = by_name.get(name, {})
                absent = []
                resources = c.get("resources", {})
                for resource in ("cpu", "memory", "ephemeral-storage"):
                    if resource not in resources.get("requests", {}):
                        absent.append("request:" + resource)
                for metric in ("cpu", "memory", "rootfs", "logs"):
                    if metric not in measured:
                        absent.append(metric)
                # Kubelet summary has neither cgroup memory peak nor throttle counters.
                absent.extend(("memory_peak", "cpu_throttling"))
                cs = state.get(name, {})
                containers.append({"name": name, "carrier": carrier, "restart_policy": c.get("restartPolicy"),
                                   "resources": resources, "usage": {k: pick(measured.get(k, {}),
                                   ("time", "usageNanoCores", "usageCoreNanoSeconds", "workingSetBytes", "usageBytes", "rssBytes", "availableBytes", "capacityBytes", "usedBytes")) for k in ("cpu", "memory", "rootfs", "logs")},
                                   "restart_count": cs.get("restartCount"),
                                   "terminated_reason": cs.get("state", {}).get("terminated", {}).get("reason"),
                                   "missing": absent})
        conditions = [pick(c, ("type", "status", "reason", "lastTransitionTime")) for c in status.get("conditions", [])]
        row = {"pod": identity(p), "workflow": identity(w), "repo": labels.get("oberth.ci/repo"),
               "commit": labels.get("oberth.ci/sha"), "spec_identity": w.get("metadata", {}).get("annotations", {}).get("oberth.ci/spec-identity"),
               "node_id": node_id, "template": node.get("templateName"), "node": spec.get("nodeName"),
               "phase": status.get("phase"), "reason": status.get("reason"), "conditions": conditions,
               "priority": spec.get("priority"), "priority_class": spec.get("priorityClassName"),
               "preemption_policy": spec.get("preemptionPolicy"), "overhead": spec.get("overhead"),
               "pod_resources": spec.get("resources"), "containers": containers,
               "volumes": [{"name": v.get("name"), **pick(v, ("emptyDir", "persistentVolumeClaim"))} for v in spec.get("volumes", []) if "emptyDir" in v or "persistentVolumeClaim" in v],
               "ephemeral_storage": usage.get("ephemeral-storage"),
               "volume_usage": [{"name": v.get("name"), **pick(v, ("time", "usedBytes", "capacityBytes", "availableBytes", "pvcRef"))} for v in usage.get("volume", [])],
               "claims": claims,
               "missing": missing}
        row["resources_sha256"] = digest({"containers": [pick(c, ("name", "carrier", "restart_policy", "resources")) for c in containers],
                                          **pick(row, ("overhead", "pod_resources", "volumes"))})
        rows.append(row)
    return {"observed_at": frame.get("observed_at"), "pods": rows, "workflows": runs,
            "node": {"identity": identity(frame.get("node", {})),
                     "allocatable": frame.get("node", {}).get("status", {}).get("allocatable"),
                     "conditions": [pick(c, ("type", "status", "reason", "lastTransitionTime")) for c in frame.get("node", {}).get("status", {}).get("conditions", [])],
                     "usage": {k: pick(frame.get("stats", {}).get("node", {}).get(k, {}), ("time", "availableBytes", "capacityBytes", "usedBytes", "usageNanoCores", "workingSetBytes")) for k in ("fs", "cpu", "memory")}},
            "events": [{"object": pick(e.get("involvedObject", {}), ("kind", "namespace", "name", "uid")),
                        "insufficient": [r for r in ("cpu", "memory", "ephemeral-storage", "pods") if "Insufficient " + r in e.get("message", "")],
                        **pick(e, ("reason", "type", "count", "firstTimestamp", "lastTimestamp", "eventTime"))} for e in items(frame.get("events"))],
            "missing": sorted(set(frame.get("missing", [])) | {k for k in ("pods", "workflows", "stats", "node", "events", "pvcs") if k not in frame}),
            "limitations": ["sampled usage can miss peaks", "PVC usage requires volume or external sampler coverage", "child Pods without captured owner chains remain unjoined", "control readiness and API latency require separate probes"],
            "sizing_eligible": False}


def summarize(stream):
    counts = {"frames": 0, "pod_samples": 0, "run_pod_samples": 0, "joined_run_pod_samples": 0, "complete_measurement_samples": 0}
    missing = {}
    for frame in stream:
        counts["frames"] += 1
        for row in frame["pods"]:
            counts["pod_samples"] += 1
            # All observed Pods are denominators: unknown children cannot vanish.
            complete = not set(row["missing"]).intersection(("workflow_uid_join", "repo", "commit", "template", "pod_uid", "pod_spec"))
            counts["joined_run_pod_samples"] += int(complete)
            counts["complete_measurement_samples"] += int(not row["missing"] and all(not c["missing"] for c in row["containers"]))
            counts["run_pod_samples"] += int(bool(row["workflow"].get("uid")))
            for field in row["missing"]:
                missing[field] = missing.get(field, 0) + 1
            for c in row["containers"]:
                for field in c["missing"]:
                    name = "container:" + field
                    missing[name] = missing.get(name, 0) + 1
        for field in frame["missing"]:
            missing[field] = missing.get(field, 0) + 1
    return {**counts, "missing": missing, "sizing_eligible": False,
            "coverage_note": "All Pods remain in the denominator, including baseline and unknown children. Counts are observations, not complete executions; no sizing recommendation."}


def frames(source):
    while line := source.readline(MAX_FRAME + 1):
        if len(line) > MAX_FRAME:
            raise ValueError("observation exceeds 32 MiB")
        yield json.loads(line)


def read_native(args):
    # Fixed GET operations only. Never echo command errors or unfiltered objects.
    with tempfile.TemporaryFile() as output:
        result = subprocess.run(["kubectl", "--request-timeout=10s", *args], stdout=output,
                                stderr=subprocess.DEVNULL, check=False, timeout=15)
        if result.returncode or output.tell() > MAX_FRAME:
            raise ValueError("native read unavailable")
        output.seek(0)
        return json.load(output)


def collect(args):
    for index in range(args.samples):
        frame = {"observed_at": dt.datetime.now(dt.timezone.utc).isoformat(), "missing": []}
        for name, resource in (("pods", "pods"), ("workflows", "workflows.argoproj.io"), ("events", "events"), ("pvcs", "persistentvolumeclaims"), ("owners", "jobs.batch"), ("owners", "virtualmachineinstances.kubevirt.io")):
            frame.setdefault(name, {"items": []})
            for namespace in args.namespace:
                try:
                    frame[name]["items"].extend(items(read_native(["get", resource, "--namespace", namespace, "-o", "json"])))
                except (ValueError, subprocess.SubprocessError):
                    frame["missing"].append(resource + ":" + namespace)
        for name, query in (("node", ["get", "node", args.node, "-o", "json"]),
                            ("stats", ["get", "--raw", "/api/v1/nodes/" + args.node + "/proxy/stats/summary"])):
            try:
                frame[name] = read_native(query)
            except (ValueError, subprocess.SubprocessError):
                frame["missing"].append(name)
        print(json.dumps(observation(frame), separators=(",", ":")), flush=True)
        if index + 1 < args.samples:
            time.sleep(args.interval)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=("collect", "project", "summarize"))
    parser.add_argument("--namespace", action="append", default=[])
    parser.add_argument("--node")
    parser.add_argument("--interval", type=int, default=30, choices=range(20, 61))
    parser.add_argument("--samples", type=int, default=1, choices=range(1, 8641))
    args = parser.parse_args()
    if args.mode == "collect":
        names = [args.node, *args.namespace]
        if not args.node or not args.namespace or any(not re.fullmatch(r"[a-z0-9](?:[a-z0-9.-]{0,251}[a-z0-9])?", n) for n in names):
            parser.error("collect requires explicit node and namespace names")
        collect(args)
    elif args.mode == "project":
        for frame in frames(sys.stdin.buffer):
            print(json.dumps(observation(frame), separators=(",", ":")), flush=True)
    else:
        print(json.dumps(summarize(frames(sys.stdin.buffer)), sort_keys=True))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, TypeError, KeyError, AttributeError, OSError, subprocess.SubprocessError):
        sys.exit("Observation input unavailable or invalid.")
