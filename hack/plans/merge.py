#!/usr/bin/env python3
"""Merge reviewed decisions, then derive their hashes and source locations."""
import copy
import hashlib
import json
from pathlib import Path
import re
import runpy
import subprocess
import sys
import tempfile


ROOT = Path(__file__).resolve().parents[2]
API = runpy.run_path(str(ROOT / "hack/plans/parity.py"))
REVIEW = runpy.run_path(str(ROOT / "hack/plans/portable.py"))["reviewed_dispositions"]
MANIFEST = "docs/plans/dispositions.json"


def sha(text):
    return hashlib.sha256(text.encode()).hexdigest()


def revision(label):
    # Rebase/cherry-pick labels include the subject; merge labels are refs.
    parent = label.startswith("parent of ")
    name = label.removeprefix("parent of ").split(" ", 1)[0]
    return name + ("^" if parent else "")


def show(ref, path):
    return subprocess.check_output(["git", "show", ref + ":" + path], text=True)


def text_merge(texts):
    if texts[1] == texts[2]:
        return texts[1]
    if texts[0] == texts[1]:
        return texts[2]
    if texts[0] == texts[2]:
        return texts[1]
    # Git owns the scratch files; keep child scratch beside them, on disk.
    with tempfile.TemporaryDirectory(prefix="plan-merge-", dir=Path(sys.argv[1]).resolve().parent) as directory:
        paths = [Path(directory) / str(i) for i in range(3)]
        for path, text in zip(paths, texts):
            path.write_text(text)
        result = subprocess.run(["git", "merge-file", "-p", str(paths[1]), str(paths[0]), str(paths[2])],
                                capture_output=True, text=True)
    assert result.returncode == 0, "authored plan content conflicts"
    return result.stdout


def locations(sources):
    """Use section identities only during the merge; commit exact line citations."""
    expected = API["spec_deltas"](sources)
    result = {}
    for path, source in sources.items():
        heading, counts = [], {}
        for number, line in enumerate(source.splitlines(), 1):
            match = re.match(r"^(#{1,6}) (.+)$", line)
            if match:
                level, title = len(match[1]), match[2]
                heading = [(depth, text) for depth, text in heading if depth < level] + [(level, title)]
            location = f"{path}:{number}"
            if location in expected:
                section = path + "#" + "/".join(text for _, text in heading)
                counts[section] = counts.get(section, 0) + 1
                result[location] = section + ":" + str(counts[section])
    assert len(set(result.values())) == len(result), "ambiguous Delta section"
    return result


def normalize(data, sources):
    expected = API["spec_deltas"](sources)
    REVIEW(data, expected, lambda path: sha(sources[path]), API["compact"])
    ids = locations(sources)
    result = copy.deepcopy(data)
    del result["spec_sha256"]
    result["deltas"] = dict(sorted((ids[delta["spec"]], {**delta, "spec": ids[delta["spec"]]})
                                  for delta in result["deltas"]))
    return json.dumps(result, indent=2, ensure_ascii=False) + "\n"


def merge_manifest(refs, inputs=None):
    manifests = [json.loads(text) for text in inputs] if inputs else [json.loads(show(ref, MANIFEST)) for ref in refs]
    paths = set().union(*(data["spec_sha256"] for data in manifests))
    sources = [{path: show(ref, path) for path in paths} for ref in refs]
    normalized = [normalize(data, source) for data, source in zip(manifests, sources)]
    merged = json.loads(text_merge(normalized))
    current = {path: text_merge([source[path] for source in sources]) for path in paths}
    ids = {identity: location for location, identity in locations(current).items()}
    deltas = []
    for identity, delta in merged["deltas"].items():
        assert identity in ids, "stale Delta location: " + identity
        delta["spec"] = ids[identity]
        deltas.append(delta)
    merged["deltas"] = sorted(deltas, key=lambda delta: (delta["spec"].rsplit(":", 1)[0], int(delta["spec"].rsplit(":", 1)[1])))
    merged["spec_sha256"] = {path: sha(current[path]) for path in sorted(paths)}
    REVIEW(merged, API["spec_deltas"](current), lambda path: sha(current[path]), API["compact"])
    # Preserve the authored top-level field order and ordinary JSON formatting.
    order = list(manifests[0]) + [field for field in merged if field not in manifests[0]]
    return json.dumps({field: merged[field] for field in order if field in merged}, indent=2, ensure_ascii=False) + "\n"


def main():
    base, ours, theirs, path, *labels = sys.argv[1:]
    refs = [revision(label) for label in labels]
    inputs = [Path(file).read_text() for file in (base, ours, theirs)]
    manifest = merge_manifest(refs, inputs if path == MANIFEST else None)
    if path == MANIFEST:
        result = manifest
    else:
        pattern = r"(?m)^Spec input:.*$"
        assert all(len(re.findall(pattern, text)) == 1 for text in inputs), "missing spec input header"
        result = text_merge([re.sub(pattern, "Spec input: derived on merge.", text) for text in inputs])
        count = len(json.loads(manifest)["deltas"])
        header = f"Spec input: {MANIFEST} (override --dispositions); SHA256 {sha(manifest)}; {count} marked Delta locations."
        result = re.sub(pattern, lambda _: header, result)
    Path(ours).write_text(result)


if __name__ == "__main__":
    try:
        main()
    except (AssertionError, KeyError, ValueError, OSError, subprocess.CalledProcessError) as error:
        print("Plan merge needs review: " + str(error), file=sys.stderr)
        sys.exit(1)
