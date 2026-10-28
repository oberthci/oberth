#!/usr/bin/env python3
"""Run the admitted 32Mi step through Argo emissary in a real cgroup.

The observer runs outside the limited container. No cluster access, privileges,
Docker socket mount or changes to the step's memory limit are needed.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid


def run(args, **kwargs):
    return subprocess.run(args, check=True, text=True, capture_output=True,
                          timeout=120, **kwargs)


def digest(path):
    hasher = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1 << 20), b""):
            hasher.update(chunk)
    return hasher.hexdigest()


def readbacks(output):
    result = {}
    section = None
    for line in output.splitlines():
        if line.startswith("receipt "):
            section = line[len("receipt "):]
            result[section] = []
        elif section is not None:
            result[section].append(line)
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--docker", default="docker")
    parser.add_argument("--go", default="go")
    parser.add_argument("--argoexec", type=Path, required=True,
                        help="reviewed Linux Argo v4.0.8 executable")
    parser.add_argument("--baseline-binary", type=Path,
                        help="optional previous Linux Oberth binary; force its wrapper")
    parser.add_argument("--image", help="override the fixture BusyBox image (pin its digest)")
    parser.add_argument("--platform", default="linux/arm64")
    parser.add_argument("--samples", type=int, default=10)
    parser.add_argument("--receipt", type=Path, required=True)
    args = parser.parse_args()
    if args.samples < 1:
        parser.error("samples must be positive")
    root = Path(__file__).resolve().parent.parent
    argo = args.argoexec.resolve(strict=True)
    baseline = args.baseline_binary.resolve(strict=True) if args.baseline_binary else None
    receipt = {"schema": 1, "platform": args.platform, "memory_bytes": 32 << 20,
               "swap_bytes": 0, "argoexec_sha256": digest(argo), "samples": []}
    if baseline:
        receipt["baseline_sha256"] = digest(baseline)
    try:
        with tempfile.TemporaryDirectory(prefix="oberth-usage-memory-") as temporary:
            manifest = Path(temporary) / "template.json"
            environment = dict(os.environ, OBERTH_USAGE_MEMORY_EXPORT=str(manifest))
            run([args.go, "test", "-p", "2", "./internal/argojob", "-run",
                 "^TestStepUsageTiny32MiAdmission$", "-count=1"], cwd=root, env=environment)
            template = json.loads(manifest.read_text())
            container = template["container"]
            if (container["resources"]["limits"]["memory"] != "32Mi" or
                    container["resources"]["requests"]["memory"] != "16Mi" or
                    container["command"] != ["/bin/sh", "-ec"]):
                raise RuntimeError("admitted command or resources changed")
            image = args.image or container["image"]
            receipt["image"] = image
            receipt["image_id"] = json.loads(run([args.docker, "image", "inspect", image]).stdout)[0]["Id"]
            receipt["admission_warning"] = template["metadata"]["annotations"]["oberth.ci/step-usage-memory-warning"]
            for mode in (["admitted", "forced_old_wrapper"] if baseline else ["admitted"]):
                for number in range(args.samples):
                    name = "oberth-usage-memory-" + uuid.uuid4().hex[:16]
                    command = [args.docker, "run", "--name", name, "--platform", args.platform,
                               "--network=none", "--memory=32m", "--memory-swap=32m", "--cpus=2",
                               "--user=65534:65534", "--cap-drop=ALL", "--security-opt=no-new-privileges",
                               "--read-only", "--pids-limit=128", "--env=ARGO_CONTAINER_NAME=main",
                               "--tmpfs=/var/run/argo:rw,size=1m,mode=1777"]
                    for source, target in [(argo, "/probe/argoexec"), (manifest, "/probe/template")]:
                        # The template is public test data; the non-root emissary must read it.
                        if source == manifest:
                            manifest.chmod(0o644)
                        command += ["--mount", f"type=bind,src={source},dst={target},readonly"]
                    step = container["command"] + container.get("args", [])
                    if mode == "forced_old_wrapper":
                        command += ["--mount", f"type=bind,src={baseline},dst=/run/oberth/usage/oberth,readonly"]
                        step = ["/run/oberth/usage/oberth", "internal-step-exec", "--"] + step
                    command += [image, "/bin/sh", "-ec",
                                'cp /probe/template /var/run/argo/template; exec /probe/argoexec emissary -- "$@"',
                                "step"] + step
                    started = time.monotonic()
                    try:
                        process = subprocess.run(command, text=True, capture_output=True, timeout=60)
                        state = json.loads(run([args.docker, "inspect", name]).stdout)[0]["State"]
                        sample = {"mode": mode, "sample": number, "elapsed_seconds": time.monotonic() - started,
                                  "returncode": process.returncode, "state": state,
                                  "stdout": process.stdout, "stderr": process.stderr,
                                  "readbacks": readbacks(process.stdout)}
                        receipt["samples"].append(sample)
                        print(json.dumps({key: sample[key] for key in ("mode", "sample", "returncode", "elapsed_seconds")}))
                        if mode == "admitted" and (process.returncode != 0 or state["OOMKilled"] or
                                                   state["Running"] or state["Pid"] != 0 or
                                                   "tiny step OK" not in process.stdout or
                                                   sample["readbacks"].get("/sys/fs/cgroup/memory.max") != ["33554432"] or
                                                   sample["readbacks"].get("/sys/fs/cgroup/memory.swap.max") != ["0"]):
                            raise RuntimeError("admitted 32Mi tiny step failed; inspect receipt")
                    finally:
                        # This name belongs only to this invocation, including timeout cleanup.
                        run([args.docker, "rm", "-f", name])
            receipt["result"] = "PASS"
    except BaseException as error:
        receipt["result"] = "FAIL"
        receipt["error"] = str(error)
        raise
    finally:
        args.receipt.write_text(json.dumps(receipt, indent=2) + "\n")


if __name__ == "__main__":
    main()
