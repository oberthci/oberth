"""Validate explicit CPU and memory budgets in Argo workflows and fragments."""
import argparse
from decimal import Decimal, InvalidOperation
from pathlib import Path
import re
import subprocess
import sys

import yaml


class UniqueLoader(yaml.SafeLoader):
    """Reject duplicate keys rather than silently accepting an override."""


def mapping(loader, node, deep=False):
    loader.flatten_mapping(node)
    result = {}
    for key, value in node.value:
        key = loader.construct_object(key, deep=deep)
        if key in result:
            raise ValueError("duplicate YAML key: " + str(key))
        result[key] = loader.construct_object(value, deep=deep)
    return result


UniqueLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, mapping)


def quantity(value, cpu=False):
    if isinstance(value, bool) or not isinstance(value, (str, int, float)):
        raise ValueError("quantity must be a number or string")
    match = re.fullmatch(r"([+]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?)([a-zA-Z]*)", str(value))
    factors = {"": 1, "n": Decimal("1e-9"), "u": Decimal("1e-6"),
               "m": Decimal(".001"), "k": 1000, "K": 1000,
               "M": 10**6, "G": 10**9, "T": 10**12, "P": 10**15, "E": 10**18,
               "Ki": 2**10, "Mi": 2**20, "Gi": 2**30,
               "Ti": 2**40, "Pi": 2**50, "Ei": 2**60}
    if not match or match[2] not in factors:
        raise ValueError("invalid resource quantity")
    result = Decimal(match[1]) * factors[match[2]]
    if not result.is_finite() or result <= 0:
        raise ValueError("resource quantity must be positive")
    return result


def containers(template):
    for kind in ("container", "script"):
        if kind in template:
            yield kind, template[kind]
    for kind in ("initContainers", "sidecars"):
        for item in template.get(kind, []):
            yield kind + "/" + str(item.get("name", "?")), item
    for item in template.get("containerSet", {}).get("containers", []):
        yield "containerSet/" + str(item.get("name", "?")), item


def check(document, label="workflow"):
    failures = []
    if not isinstance(document, dict):
        return failures
    kind = document.get("kind")
    if kind not in ("Workflow", "WorkflowTemplate", "ClusterWorkflowTemplate", "CronWorkflow"):
        return failures
    spec = document.get("spec", {})
    if kind == "CronWorkflow":
        spec = spec.get("workflowSpec", {})
    for metadata in (document.get("metadata") or {}, spec.get("workflowMetadata") or {}):
        for key in metadata.get("annotations") or {}:
            if key.startswith("oberth.ci/") and (key.split("/", 1)[1].startswith("size") or "weight" in key):
                failures.append(f"{label}: resource admission cannot use {key}")
    templates = spec.get("templates")
    if not isinstance(templates, list) or not templates:
        return failures + [f"{label}: missing templates"]
    # Patches can override an otherwise valid container budget at submission.
    for scope in [spec, *templates]:
        if "podSpecPatch" in scope:
            failures.append(f"{label}: podSpecPatch requires an explicit resource review")
    for template in templates:
        name = label + "/" + str(template.get("name", "?"))
        for role, box in containers(template):
            try:
                resources = box["resources"]
                for resource, ceiling in (("cpu", Decimal(4)), ("memory", Decimal(8 * 2**30))):
                    request = quantity(resources["requests"][resource], resource == "cpu")
                    limit = quantity(resources["limits"][resource], resource == "cpu")
                    if request > limit:
                        failures.append(f"{name}/{role}: {resource} request exceeds limit")
                    if resource == "cpu" and request > 1:
                        failures.append(f"{name}/{role}: CPU request exceeds 1000m policy cap")
                    if limit > ceiling:
                        failures.append(f"{name}/{role}: {resource} exceeds container ceiling")
            except (KeyError, TypeError, ValueError, InvalidOperation) as error:
                failures.append(f"{name}/{role}: invalid or missing explicit CPU/memory requests/limits ({error})")
    return failures


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("paths", nargs="*", type=Path, default=[Path(".oberth")])
    args = parser.parse_args()
    if args.self_test:
        tests = Path(__file__).with_name("test_workflow_resources.py")
        subprocess.run([sys.executable, "-B", str(tests)], check=True)
    failures = []
    workflows = 0
    paths = sorted({f for p in args.paths for f in (p.rglob("*") if p.is_dir() else [p])
                    if f.suffix in (".yaml", ".yml")})
    for path in paths:
        try:
            for document in yaml.load_all(path.read_text(), Loader=UniqueLoader):
                if isinstance(document, dict) and document.get("kind") in (
                        "Workflow", "WorkflowTemplate", "ClusterWorkflowTemplate", "CronWorkflow"):
                    workflows += 1
                failures.extend(check(document, str(path)))
        except (OSError, ValueError, yaml.YAMLError, TypeError, AttributeError) as error:
            failures.append(f"{path}: invalid workflow YAML ({error})")
    if workflows == 0:
        failures.append("no Argo workflows or fragments found")
    for failure in failures:
        print(failure, file=sys.stderr)
    print(f"resource guard: {workflows} workflows; {len(failures)} violations")
    return bool(failures)


if __name__ == "__main__":
    sys.exit(main())
