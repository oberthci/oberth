"""Bind exact reference gaps and two-sample budgets to the verified release binary."""
from copy import deepcopy
import json
import re
from walkassert import Failure, HERE, require
from walkrestart import outage_starts
from walkux import baseline_changes


def kind_for(version, sha, baseline):
    reference = baseline["reference"]
    return "reference" if (version, sha) == (reference["release"], reference["cli_sha256"]) else "candidate"


def normalize(text):
    text = re.sub(r"localhost:(?:34443|35443)", "localhost:<https>", text)
    text = re.sub(r"localhost:(?:34022|35022)", "localhost:<ssh>", text)
    text = re.sub(r"oberth-walk-[0-9a-f]{12}-[0-9a-f]{8}-[0-9a-f]{12}", "<workflow>", text)
    text = re.sub(r"walk-[0-9a-f]{12}", "walk-<id>", text)
    return re.sub(r"\b[0-9a-f]{8,64}\b", "<id>", text)


def signature(row):
    facts = row.get("facts", {})
    detail = facts.get("accepted_run", {}).get("run", {}).get("error", "")
    if "probe_log" in facts:
        detail = next((line for line in facts["probe_log"].splitlines() if line.startswith("AssertionError:")), "")
    if "cli_message" in facts:
        detail = facts["cli_message"] + "\nagent output: " + facts["agent_output"]
    if "views" in facts:
        detail = json.dumps(
            {name: view.get("retry_hint", {}).get("action") for name, view in facts["views"].items()}, sort_keys=True
        )
    if row.get("name") == "server-restart":
        detail = restart_detail(facts)
    return {"reason": normalize(row.get("reason", "")), "detail": normalize(detail)}


def restart_detail(facts):
    observation = {"outage_start": facts.get("outage_start"), "outage_end": facts.get("outage_end"),
                   "source_checks": deepcopy(facts.get("source_checks", []))}
    try:
        outage_starts(observation)
        overlap = set(observation["outage_starts"])
    except (Failure, KeyError, TypeError, ValueError):
        overlap = set()
    return json.dumps({
        "failed_steps": sorted((n.get("displayName"), n.get("message"))
                               for n in facts.get("native_nodes", {}).values()
                               if n.get("type") == "Pod" and n.get("phase") in ("Failed", "Error")),
        "started_inside_outage": sorted(c.get("waited_container") for c in observation["source_checks"]
                                        if c.get("uid") in overlap),
    }, sort_keys=True)


def gap_changes(old, new):
    require(
        (old["release"], old["cli_sha256"]) == (new["release"], new["cli_sha256"]), "Reference identity cannot change"
    )
    require(set(new["steps"]) <= set(old["steps"]), "Reference gap steps may only shrink")
    for name, rule in new["steps"].items():
        for field in ("requirements", "signatures", "missing_remedies"):
            before = {normalize(json.dumps(v, sort_keys=True)) for v in old["steps"][name][field]}
            after = {normalize(json.dumps(v, sort_keys=True)) for v in rule[field]}
            require(after <= before, "Reference gap policy may only shrink: " + name)


def approved(result, expected):
    baseline = json.loads((HERE / "golden-path.json").read_text())
    reference = kind_for(result["target"].split()[-1], result.get("cli_sha256", ""), baseline) == "reference"
    rules = {}
    if reference:
        data = json.loads((HERE / "reference-gaps.json").read_text())
        require(
            data["release"] == result["target"].split()[-1] and data["cli_sha256"] == result["cli_sha256"],
            "Reference gap identity differs",
        )
        rules = data["steps"]
    rows = [row for row in result["steps"] if row["name"] != "ux-gate"]
    require(
        len(rows) == len(expected) and {r["name"] for r in rows} == expected,
        "Baseline needs complete functional evidence",
    )
    allowed = set()
    for row in rows:
        if row["status"] == "pass":
            continue
        rule = rules.get(row["name"], {})
        require(
            row["status"] == "fail" and signature(row) in rule.get("signatures", []),
            "Unapproved failure: " + row["name"],
        )
        allowed.add(row["name"])
    for missing in result["ux"]["missing_remedies"]:
        rule = rules.get(missing["step"], {})
        require(
            normalize(missing["message"]) in rule.get("missing_remedies", []),
            "Unapproved next-action failure: " + missing["step"],
        )
    return allowed


def record_sample(baseline, sample):
    require(all(sample[n]["complete"] and sample[n]["seconds"] is not None for n in ("first_green", "first_release")),
            "Reference milestones must be measured")
    value = deepcopy(baseline)
    mode = sample["mode"]
    samples = value["samples"][mode]
    require(not any(s["evidence"] == sample["evidence"] for s in samples), "Reference sample is already recorded")
    samples.append(sample)
    value["samples"][mode] = samples[-2:]
    if len(samples) >= 2:
        profile = {
            "release": sample["release"],
            "samples": [s["evidence"] for s in samples[-2:]],
            "reason": sample.get("reason", ""),
        }
        for milestone in ("first_green", "first_release"):
            profile[milestone] = min((s[milestone] for s in samples[-2:]), key=lambda m: m["seconds"])
        value["profiles"]["reference"][mode] = profile
    baseline_changes(baseline, value)
    return value
