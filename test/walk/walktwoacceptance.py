"""Read-only controller and installation isolation assertions."""
import json
import re
from walkassert import require

LABEL = "workflows.argoproj.io/controller-instanceid"


def objects(s, instance):
    return json.loads(s.user("two-objects", {"namespace": instance["pipeline_namespace"]})[1])["items"]


def labelled(items, namespace):
    require(items, "No native objects to inspect")
    require(all(item["metadata"].get("labels", {}).get(LABEL) == namespace for item in items),
            "Native object has the wrong controller label")


def quiet(raw, names):
    require(all(name.encode() not in raw for name in names), "Independent controller acted on our objects")


def unique_occurrences(items):
    parents = {item["metadata"]["uid"] for item in items if item["kind"] == "CronWorkflow"}
    require(parents, "No authored CronWorkflow to inspect")
    occurrences = {}
    for item in items:
        if item["kind"] != "Workflow":
            continue
        meta = item["metadata"]
        parent = next((o["uid"] for o in meta.get("ownerReferences", []) if o["kind"] == "CronWorkflow"), None)
        if parent:
            require(parent in parents, "Scheduled child has an unknown parent")
            stamp = meta.get("annotations", {}).get("workflows.argoproj.io/scheduled-time")
            require(stamp, "Scheduled child has no occurrence time")
            occurrences.setdefault((parent, stamp), []).append(meta["name"])
    require(all(len(names) == 1 for names in occurrences.values()), "Schedule fired twice for one occurrence")
    require(all(sum(p == parent for p, _ in occurrences) >= 2 for parent in parents),
            "Observe two occurrences per authored schedule")
    return len(occurrences)


def tool(s, instance, name, arguments, *, accept=(0,)):
    from walktwo import cli
    return cli(s, instance, ["tool", name, "-", "--json"],
               input=json.dumps(arguments).encode(), accept=accept)


def visible(repos, runs, own, foreign):
    require(not repos.get("next_cursor") and not runs.get("next_cursor"), "Isolation fixture exceeds one page")
    names = {row["repo"] for row in repos["repositories"]}
    ids = {row["id"] for row in runs["runs"]}
    require(own["repo"] in names and foreign["repo"] not in names, "Repository isolation failed")
    require(own["run_id"] in ids and foreign["run_id"] not in ids, "Run isolation failed")
    require(all(row["repo"] == own["repo"] for row in runs["runs"]), "Foreign repository run is visible")


def controllers(s):
    from walktwo import configuration
    config = configuration(s)
    first, second = config["instances"]
    for instance in (first, second):
        require(re.fullmatch(r"[0-9a-f]{32}", instance.get("run_id", ""))
                and re.fullmatch(r"[0-9a-f]{40}", instance.get("commit_sha", ""))
                and instance.get("repo"), "Supply exact pushed run, commit and repository")
    require(first["run_id"] != second["run_id"] and first["repo"] != second["repo"],
            "Use distinct run and repository controls")
    names = []
    for own, foreign in ((first, second), (second, first)):
        detail = json.loads(tool(s, own, "run_get", {"id": own["run_id"]})[1])
        require(detail.get("status") == "passed" and detail.get("repo") == own["repo"]
                and detail.get("commit_sha") == own["commit_sha"], "Exact pushed run did not pass")
        repos = json.loads(tool(s, own, "repo_list", {})[1])
        runs = json.loads(tool(s, own, "run_list", {"limit": 100})[1])
        visible(repos, runs, own, foreign)
        code, raw, errors = tool(s, own, "run_get", {"id": foreign["run_id"]}, accept=(0, 1, 2, 3))
        try:
            refusal = json.loads(raw or errors)
        except ValueError:
            refusal = {}
        require(code in (1, 3) and refusal.get("code") in ("not_found", "forbidden"),
                "Foreign run lookup needs an authority refusal")
        items = objects(s, own)
        labelled(items, own["namespace"])
        workflows = [item for item in items if item["kind"] == "Workflow"
                     and item["metadata"].get("labels", {}).get("oberth.ci/run") == own["run_id"]]
        require(len(workflows) == 1 and workflows[0].get("status", {}).get("phase") == "Succeeded",
                "Exact pushed Workflow did not succeed")
        names.extend(item["metadata"]["name"] for item in items if item["kind"] in ("Workflow", "CronWorkflow"))
    raw = s.user("two-controller-log", {"namespace": config["argo"]["namespace"],
                 "release": config["argo"]["release"]})[1]
    quiet(raw, names)
    return {"exact_runs_passed": True, "lists_and_foreign_lookups_isolated": True,
            "controller_labels_exact": True, "independent_controller_silent": True}


def schedule(s):
    from walktwo import configuration
    config = configuration(s)
    counts, names = {}, []
    for instance in config["instances"]:
        items = objects(s, instance)
        labelled(items, instance["namespace"])
        counts[instance["name"]] = unique_occurrences(items)
        names.extend(item["metadata"]["name"] for item in items if item["kind"] in ("Workflow", "CronWorkflow"))
    raw = s.user("two-controller-log", {"namespace": config["argo"]["namespace"],
                 "release": config["argo"]["release"]})[1]
    quiet(raw, names)
    return {"native_occurrences": counts, "independent_controller_silent": True}
