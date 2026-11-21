"""Validate the closed policy vocabulary before source inspection."""
import json
import re
import subprocess
import sys

def keys(value, allowed, at, required=()):
    assert isinstance(value, dict), (at, "expected object")
    assert not set(value) - set(allowed), (at, "unknown key", sorted(set(value)-set(allowed)))
    assert set(required) <= set(value), (at, "missing key", sorted(set(required)-set(value)))

def dependencies(data):
    fields = "version production_commands product_command paths components shared allow rules fixture_only packages shared_per_component package_allow shared_dependencies test_allow retired reference_packages harness".split()
    keys(data, fields, "dependencies", fields)
    assert data["version"] == 2, ("dependencies.version", data["version"])
    commands = data["production_commands"]
    assert isinstance(commands, list) and all(isinstance(p, str) and re.fullmatch(r"cmd/[\w-]+(?:/[\w-]+)*", p) for p in commands), ("production_commands", "expected exact command paths")
    assert len(commands) == len(set(commands)), ("production_commands", "duplicate command")
    assert data["product_command"] in commands, ("product_command", "expected one listed production command")
    owners = set(data["components"])
    assert all(re.fullmatch(r"C(?:[1-9]|1[0-4])", c) for c in owners), ("components", sorted(owners))
    for field in ("allow", "shared_per_component"):
        assert set(data[field]) == owners, (field, "component keys", sorted(set(data[field]) ^ owners))
    def patterns(values, at):
        assert isinstance(values, list) and all(isinstance(p, str) and p.strip() == p and p for p in values), (at, "paths")
        for pattern in values:
            if pattern.startswith("@"):
                valid = {"@shared", "@components"} | {"@"+c for c in owners} | {"@components:except:"+c for c in owners}
                assert pattern in valid, (at, "unknown component reference", pattern)
    for field in ("components", "allow", "shared_per_component", "package_allow", "shared_dependencies", "test_allow"):
        for name, values in data[field].items():
            patterns(values, field+"."+name)
    for field in ("shared", "retired", "reference_packages"):
        patterns(data[field], field)
    for rule in data["rules"]:
        at = "rules."+str(rule.get("id"))
        keys(rule, ("id","from","to","effect","symbols","transitive"), at, ("id","from","to","effect"))
        assert rule["effect"] in {"deny","allow_if_listed"}, (at+".effect", rule["effect"])
        assert type(rule["id"]) is int, (at+".id", rule["id"])
        assert "transitive" not in rule or rule["transitive"] is True, (at+".transitive", rule.get("transitive"))
        patterns(rule["from"], at+".from"); patterns(rule["to"], at+".to")
        assert isinstance(rule.get("symbols", {}), dict), (at+".symbols", "expected owner map")
        for owner, symbols in rule.get("symbols", {}).items():
            assert owner == "*" or owner in owners, (at+".symbols", "unknown owner", owner)
            assert isinstance(symbols, list) and all(isinstance(s, str) and s for s in symbols), (at+".symbols", owner)
    ids = [r["id"] for r in data["rules"]]
    assert len(ids) == len(set(ids)), ("rules.id", "duplicate id")
    for name, record in data["packages"].items():
        keys(record, ("owner","kind","contents","consumers"), "packages."+name, ("owner","kind","consumers"))
        assert record["owner"] in owners, ("packages."+name+".owner", record["owner"])
        assert record["kind"] in {"runtime","shared","test_only"}, ("packages."+name+".kind", record["kind"])
        assert set(record["consumers"]) <= owners, ("packages."+name+".consumers", sorted(set(record["consumers"])-owners))
    for rule in data["fixture_only"]:
        keys(rule, ("from","to","build_scope"), "fixture_only", ("from","to"))
        patterns(rule["from"], "fixture_only.from"); patterns(rule["to"], "fixture_only.to")
    keys(data["harness"], ("trees",), "harness", ("trees",))
    patterns(data["harness"]["trees"], "harness.trees")

def retired(data):
    keys(data, ("version","selector_semantics","rules","exemptions"), "retired", ("version","rules","exemptions"))
    for rule in data["rules"]:
        at = "retired.rules."+str(rule.get("id"))
        keys(rule, ("id","owner","scope","tokens","replacement","bare_words"), at, ("id","owner","tokens","replacement"))
        assert all(isinstance(t,str) and t and t == t.strip() for t in rule["tokens"]), (at+".tokens", "padded or empty selector")
        assert re.fullmatch(r"C(?:[1-9]|1[0-4])", rule["owner"]), (at+".owner", rule["owner"])
    for entry in data["exemptions"]:
        keys(entry, ("path","owner","checks","ends","reason"), "exemptions", ("path","owner","checks","ends","reason"))
        assert re.fullmatch(r"C(?:[1-9]|1[0-4])", entry["owner"]), ("exemptions.owner", entry["owner"])
        assert entry["ends"] == "permanent", ("exemptions.ends", entry["ends"])
        assert set(entry["checks"]) <= {"retired_vocabulary","leftovers"}, ("exemptions.checks", entry["checks"])


def changes(ref):
    """Keep pin decisions isolated from implementation and visible in gate output."""
    def git(*args):
        return subprocess.check_output(["git", *args], text=True)
    def pinned(revision):
        shown = subprocess.run(["git", "show", revision+":docs/plans/dependencies.json"], capture_output=True, text=True)
        if shown.returncode:
            return set()  # history from before the plan data existed pins nothing
        return set(json.loads(shown.stdout)["shared"])
    errors = []
    for commit in git("rev-list", "--reverse", "main.."+ref).split():
        after = pinned(commit)
        parents = git("rev-parse", commit+"^@").split()
        inherited = [pinned(parent) for parent in parents]
        if after in inherited:
            continue
        for parent, before in zip(parents, inherited):
            print("GATE FILES CHANGED:")
            for action, packages in (("added", after-before), ("removed", before-after)):
                for package in sorted(packages):
                    print(f"pinned contract {action}: {package}")
            paths = git("diff", "--name-only", parent, commit).splitlines()
            if any(not (p.startswith("docs/plans/") and p.endswith(".json")) for p in paths):
                errors.append(commit+": pinned contracts require a commit containing only plan JSON data")
    if errors:
        raise ValueError("; ".join(errors))


if __name__ == "__main__":
    try:
        changes(sys.argv[1])
    except (ValueError, KeyError, TypeError, subprocess.CalledProcessError) as error:
        raise SystemExit(str(error))
