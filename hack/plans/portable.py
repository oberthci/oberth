"""Validate plan contracts using only the checked-in plan bundle."""
import hashlib
import html
import json
import re
import runpy


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def inventory(text):
    rows, category = {}, ""
    for line in text.splitlines():
        if line.startswith("## "):
            category = line[3:]
        if not line.startswith("| "):
            continue
        cells = [html.unescape(cell.strip()) for cell in line[1:-1].split("|")]
        if len(cells) != 6 or not re.fullmatch(r"C(?:[1-9]|1[0-4])", cells[1]):
            continue
        key = (category, cells[0])
        assert key not in rows, ("duplicate inventory row", key)
        rows[key] = cells
    return rows


def parity_contracts(api):
    root = api["ROOT"]
    data = json.loads((root / "docs/plans/dispositions.json").read_text())
    rows = inventory((root / "docs/plans/PARITY.md").read_text())
    baseline = data["baseline"]
    assert re.fullmatch(r"[0-9a-f]{40}", baseline["commit"]), "invalid parity baseline commit"
    declared = {}
    for delta in data["deltas"]:
        for entry in delta["rows"] + delta.get("predicates", []):
            item = entry.get("item")
            if item is None:
                continue
            assert type(item) is int, ("invalid ruling item", item)
            category = entry["category"]
            names = [entry["name"]] if "name" in entry else [name for cat, name in rows if cat == category and re.search(entry["name_regex"], name)]
            for name in names:
                declared.setdefault((category, name), set()).add(entry["disposition"])
    changed = 0
    for category, names in baseline["dispositions"].items():
        for name, previous in names.items():
            key = (category, name)
            assert key in rows, ("missing baseline inventory row", key)
            current = api["disposition_label"](rows[key][3])
            if current == previous:
                continue
            changed += 1
            kind = "NEW" if current.startswith("NEW") else current
            assert kind in declared.get(key, set()), ("unruled parity disposition change", key, previous, current)
    return {"rows": len(rows), "ruled_changes": changed}


def retired_contracts(root):
    data = json.loads((root / "docs/plans/retired.json").read_text())
    runpy.run_path(str(root / "hack/plans/policy.py"))["retired"](data)
    assert data["version"] == 1
    expected = {f"R{n:02}" for n in range(1, 22)}
    assert {rule["id"] for rule in data["rules"]} == expected
    assert len(data["rules"]) == 21, "duplicate retirement rule"
    readme = (root / "docs/plans/README.md").read_text()
    for rule in data["rules"]:
        assert rule["tokens"] and rule["replacement"].strip(), ("retirement selectors missing", rule["id"])
        assert f"| {rule['id']} |" in readme, ("retirement absent from README", rule["id"])
        assert not rule.get("bare_words") or rule["id"] == "R19", "bare words need the grouping retirement rule"
        assert set(rule.get("bare_words", [])) <= set(rule["tokens"]), "bare words need exact selectors"
        for token in rule["tokens"]:
            assert token.strip() and not re.search(r"[*?\n\r]", token), ("nonexact retirement selector", token)
            assert not re.fullmatch(r"[a-z]+", token) or token in rule.get("bare_words", []), ("bare English retirement selector", token)
    exemptions = data["exemptions"]
    assert {e["path"] for e in exemptions} == {"docs/plans/", "docs/releases/", "docs/archive/"}
    assert all(set(e["checks"]) == {"retired_vocabulary", "leftovers"} for e in exemptions)
    return {"rules": len(data["rules"]), "selectors": sum(len(r["tokens"]) for r in data["rules"])}


def disposition_contracts(api):
    root = api["ROOT"]
    data = json.loads((root / "docs/plans/dispositions.json").read_text())
    expected = api["spec_deltas"]()
    assert data["version"] == 2
    assert set(data["spec_sha256"]) == {p.rsplit(":", 1)[0] for p in expected}
    for file, sha in data["spec_sha256"].items():
        assert digest(root / file) == sha, ("stale spec digest", file)
    assert len(data["deltas"]) == len(expected)
    assert {d["spec"] for d in data["deltas"]} == set(expected)
    for delta in data["deltas"]:
        spec = expected[delta["spec"]]
        assert delta["markers"] == spec["markers"]
        assert api["compact"](delta["basis"]) in spec["text"], ("stale Delta basis", delta["spec"])
        assert delta["scope"] in {"implementation", "behaviour"}
        assert delta["scope"] != "implementation" or not delta["rows"] and not delta.get("predicates")
        for row in delta["rows"]:
            assert row["disposition"] in {"KEEP", "CHANGE", "RETIRED", "NEW"}
    sweeps = json.loads((root / "docs/plans/sweeps.json").read_text())
    assert sweeps["version"] == 1
    identities = [(s["source"], s["id"]) for s in sweeps["findings"]]
    assert len(identities) == len(set(identities))
    for entry in sweeps["findings"]:
        assert entry["source"] in sweeps["sources"]
        assert entry["disposition"] in {"KEEP", "RETIRED"}
        assert entry["disposition"] != "KEEP" or entry.get("justification")
    return len(expected)


def check(api):
    root = api["ROOT"]
    path = root / "docs/plans/requirements.json"
    assert path.stat().st_size < 100000, "requirement register exceeds compact budget"
    data = json.loads(path.read_text())
    requirements = api["requirement_contracts"](data)
    text = api["render_requirements"](data, digest(path))
    assert text == (root / "docs/plans/REQUIREMENTS.md").read_text(), "REQUIREMENTS.md differs; regenerate"
    return {"mode": "portable", "requirements": requirements,
            "dependencies": api["dependency_contracts"](), "retired": retired_contracts(root),
            "spec_deltas": disposition_contracts(api), "parity": parity_contracts(api)}
