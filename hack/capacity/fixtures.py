#!/usr/bin/env python3
"""Print off-production native G2 fixtures; never contact a cluster."""
import argparse
import copy
import json
import re


def pod(name, namespace, image):
    return {"apiVersion": "v1", "kind": "Pod", "metadata": {
        "name": "capacity-" + name, "namespace": namespace,
        "labels": {"app": "capacity-qualification"}}, "spec": {
        "restartPolicy": "Never", "automountServiceAccountToken": False,
        "containers": [{"name": "main", "image": image,
                        "command": ["python3", "-c", "print('native admission fixture')"],
                        "resources": {}}]}}


def admission(namespace, image):
    cases = []
    for name, resources in {
        "request-only-scratch": {"requests": {"ephemeral-storage": "16Gi"}},
        "request-only-memory": {"requests": {"memory": "1Gi"}},
        "limit-only": {"limits": {"cpu": "50m", "ephemeral-storage": "256Mi"}},
        "explicit-pairs": {"requests": {"cpu": "100m", "memory": "1Gi", "ephemeral-storage": "16Gi"},
                           "limits": {"memory": "2Gi", "ephemeral-storage": "16Gi"}},
        "empty": {},
    }.items():
        item = pod(name, namespace, image)
        item["spec"]["containers"][0]["resources"] = resources
        cases.append(item)
    shape = pod("sidecar-init", namespace, image)
    sidecar = copy.deepcopy(shape["spec"]["containers"][0])
    sidecar["name"], sidecar["restartPolicy"] = "sidecar", "Always"
    init = copy.deepcopy(shape["spec"]["containers"][0])
    init["name"] = "init"
    shape["spec"]["initContainers"] = [sidecar, init]
    cases.append(shape)
    return cases


def saturation(namespace, image, fault):
    template = pod("load", namespace, image)
    container = template["spec"]["containers"][0]
    container["resources"] = {"requests": {"cpu": "112m", "memory": "614Mi", "ephemeral-storage": "3Gi"},
                              "limits": {"memory": "614Mi", "ephemeral-storage": "3Gi"}}
    # A finite workload keeps failure recovery explicit. The over-limit variant
    # deliberately exceeds only a run container's memory limit.
    size = 700 if fault else 550
    container["command"] = ["python3", "-c",
                            f"import time; data=bytearray({size}*1024*1024); end=time.monotonic()+600; "
                            "exec('while time.monotonic()<end: sum(data[::4096])')"]
    return {"apiVersion": "batch/v1", "kind": "Job", "metadata": {
        "name": "capacity-overlimit" if fault else "capacity-full-load", "namespace": namespace},
        "spec": {"parallelism": 1 if fault else 80, "completions": 1 if fault else 80,
                 "backoffLimit": 0, "activeDeadlineSeconds": 900,
                 "template": {"metadata": {"labels": template["metadata"]["labels"]},
                              "spec": template["spec"]}}}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--image", required=True, help="Qualified image with Python 3, pinned by digest.")
    parser.add_argument("--case", required=True, choices=["admission", "saturation", "overlimit"])
    options = parser.parse_args()
    if not re.fullmatch(r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?", options.namespace):
        parser.error("Select a valid dedicated off-production namespace.")
    if not re.fullmatch(r"[^\s{}]+@sha256:[0-9a-f]{64}", options.image):
        parser.error("Use the qualified image's full sha256 digest.")
    items = admission(options.namespace, options.image) if options.case == "admission" else [
        saturation(options.namespace, options.image, options.case == "overlimit")]
    print(json.dumps({"apiVersion": "v1", "kind": "List", "items": items}, indent=2))


if __name__ == "__main__":
    main()
