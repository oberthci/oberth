"""Contract ratchet. Canonical owner: Oberth; emitted by its contract profile.

Baselines retain exact existing findings, never an allowance for new strings.
Review baseline reductions when removing debt; baseline increases need review.
"""
from collections import Counter
from datetime import date
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys

QUESTIONS = (
    "Which function does this add, strengthen or remove?",
    "Which contradiction did you hit, and which separation did you choose?",
    "What did you copy instead of import?",
    "What did you claim that is not measured?",
    "What did you add that has no sunset?",
)
TEXT = {".go", ".py", ".sh", ".bats", ".yaml", ".yml", ".json", ".md", ".html", ".js", ".ts", ".tf", ".tpl", ".txt"}
CONFESSION = re.compile(r"MUST match|byte-for-byte|keep in sync|in lockstep|mirror of|same as in", re.I)
GENERATOR = re.compile(r"generat\w*.*[\w./-]+\.(?:go|py|sh)\b|go:?generate\s+\S+", re.I)
RESIDUE = re.compile(r"/home/|(?:^|[=\"'])%[0-9]+\b|\bpane\s+%[0-9]+\b|roadmap#")
DEPRECATED = re.compile(r"\blegacy\b|\bdeprecated\b|\bfor now\b|\btemporary\b|\buntil\b", re.I)
ENV = re.compile(r'''(?:Getenv|LookupEnv|getenv|os\.getenv|environ\.get|env\.(?:String|Bool|Int|Duration))\s*\(\s*["']([A-Z][A-Z0-9_]+)["']''')
ANNOTATION = re.compile(r'''["']((?:[\w.-]+\.)?oberth\.ci/[\w.-]+)["']''')
FLAG = re.compile(r'''\b(?:String|Bool|Int|Duration|StringVar|BoolVar|IntVar|DurationVar)(?:P)?\s*\([^\n]*?["']([a-z][a-z0-9-]+)["']''')
SEAM = re.compile(r"_TEST_|_SKIP|ALLOW_INSECURE|_DEV_")


def files(root):
    # CI can supply a source archive without Git metadata. Enumerate that
    # artifact directly; local worktrees use Git's tracked/ignored boundary.
    if not (root / ".git").exists():
        names = []
        for parent, directories, filenames in os.walk(root):
            directories[:] = [d for d in directories if d not in (".git", "vendor", "node_modules", "__pycache__")]
            names.extend(str((Path(parent)/name).relative_to(root)) for name in filenames)
        return sorted(names)
    result = subprocess.run(["git", "-c", "safe.directory=" + str(root.resolve()), "ls-files", "-c", "-o", "--exclude-standard", "-z"], cwd=root, check=True, capture_output=True)
    return sorted(set(result.stdout.decode().split("\0")) - {""})


def scan(root, names, registry, deprecations):
    found = []
    pipeline = "\n".join((root / p).read_text(errors="replace") for p in names if p.startswith(".oberth/") and p.endswith((".yaml", ".yml")))
    ids = {d["id"] for d in deprecations}
    entries = registry["entries"]
    for name in names:
        if name.startswith(("vendor/", "node_modules/", ".git/")) or "/vendor/" in name:
            continue
        if re.search(r"(^|/)(handoffs/|RELEASE-TRAIN-[^/]*|READY-[^/]*)", name):
            found.append(("law5-path", name, name))
        path = root / name
        if not path.is_file() or path.suffix not in TEXT:
            continue
        # The guard and its negative fixtures contain forbidden patterns by
        # definition. No other source or documentation is exempted.
        if name.startswith((".oberth/contracts/", "gen/contracts/")):
            continue
        content = path.read_text(errors="replace")
        lines = content.splitlines()
        for line in lines:
            def add(rule):
                found.append((rule, name, line.strip()))
            if CONFESSION.search(line) and not GENERATOR.search(line):
                add("law1-confession")
            if path.suffix==".md" and "|" in line and re.search(r"\bdefault\b|[A-Z][A-Z0-9]+_[A-Z0-9_]+",line) and "generat" not in content[max(0,content.find(line)-300):content.find(line)].lower():
                add("law2-handtyped-table")
            if name.endswith("values.yaml") and re.match(r"^\s*[a-zA-Z][\w-]*:",line):
                key="value:"+line.split(":",1)[0].strip()
                if key not in entries: add("law3-surface")
            if RESIDUE.search(line):
                add("law5-residue")
            if name != "deprecations.yaml" and DEPRECATED.search(line) and not any(d in line for d in ids):
                add("law4-unregistered")
            if "nolint" in line and not re.search(r"nolint:[a-z0-9,]+\s+--\s+\S", line):
                add("rule11-suppression")
            if name.endswith("_test.go") and re.search(r'"go/(?:parser|ast)"', line):
                add("rule6-test-parser")
            if name.endswith(".go") and not any(part in name.split("/") for part in ("secret", "hygiene")):
                if re.search(r'"unsafe"|\.SetInt64\(0\)|\b(?:zeroBytes|wipeBytes|explicit_bzero)\(', line):
                    add("rule7-hygiene")
            if name.endswith((".go", ".py")):
                for kind, pattern in (("env", ENV), ("annotation", ANNOTATION), ("flag", FLAG)):
                    for match in pattern.finditer(line):
                        key = kind + ":" + match[1]
                        if key not in entries:
                            add("law3-surface")
                        if kind == "env" and SEAM.search(match[1]) and not name.endswith("_test.go"):
                            add("rule8-env-seam")
                        if kind == "env" and name.endswith("_test.go"):
                            if match[1] not in pipeline:
                                add("rule10-gate-setter")
                            # The test's skip diagnostic must name its pipeline
                            # lane; a gate with a setter alone is insufficient.
                            if "Skip" in content and not re.search(r"(?:lane|pipeline)", content, re.I):
                                add("rule10-skip-lane")
        if path.suffix == ".go":
            # Count the span between top-level declarations. gofmt puts each
            # top-level declaration at column zero; a 200-line span is debt.
            starts = [i for i, line in enumerate(lines) if re.match(r"^(?:func|type|var|const)\s", line)] + [len(lines)]
            for start, end in zip(starts, starts[1:]):
                if lines[start].startswith("func ") and end - start > 200:
                    found.extend([("rule6-function-length", name, lines[start].strip())] * (end-start-200))
        if name in (".golangci.yml", ".golangci.yaml"):
            # Existing exclusions are ratcheted individually; new broad
            # security suppressions cannot hide inside a global allowlist.
            for line in lines:
                if re.search(r"\bG(?:402|103|304)\b", line):
                    found.append(("rule11-security-exclusion", name, line.strip()))
    return found


def fingerprint(findings):
    counts = Counter()
    for rule, name, line in findings:
        digest = hashlib.sha256((name + "\0" + line).encode()).hexdigest()
        counts[rule + ":" + digest] += 1
    return counts


def validate_registries(root):
    surface = json.loads((root / "surface-registry.json").read_text())
    for key, entry in surface["entries"].items():
        if not key.startswith(("env:", "annotation:", "flag:", "value:")) or not entry.get("doc_anchor") or not entry.get("test_ref"):
            raise ValueError("surface requires type/name, doc_anchor and test_ref: " + key)
    records = json.loads((root / "deprecations.yaml").read_text())["deprecations"]
    seen = set()
    for record in records:
        for key in ("id", "since", "until", "replacement", "owner"):
            if not record.get(key):
                raise ValueError("deprecation requires " + key)
        if record["id"] in seen:
            raise ValueError("duplicate deprecation id")
        seen.add(record["id"])
        # Dates are unambiguous across repositories. Version sunsets require
        # a canonical version source file, rather than a copied version or guessed tag.
        until = str(record["until"])
        if re.fullmatch(r"\d{4}-\d{2}-\d{2}", until):
            if date.fromisoformat(until) <= date.today():
                raise ValueError("expired deprecation: " + record["id"])
        else:
            version = lambda v: tuple(int(n) for n in str(v).removeprefix("v").split("."))
            source = (root / record.get("version_source", "")).resolve()
            if not source.is_relative_to(root.resolve()) or not source.is_file():
                raise ValueError("version sunset requires canonical version_source")
            current = re.search(record.get("version_pattern", r"(?m)^version:\s*v?(\d+\.\d+\.\d+)\s*$"), source.read_text())
            if not current or version(current[1]) >= version(until):
                raise ValueError("version sunset reached or version unreadable: " + record["id"])
    return surface, records


def check_adoption(root):
    contract = (root / "AGENT-CONTRACT.md").read_text()
    if "## TRIZ contract" not in contract:
        raise ValueError("missing TRIZ contract section")
    for word in ("Main useful function", "Harmful function", "Contradiction", "Separation", "Ideality", "Resource", "Trimming", "Guard"):
        if word not in contract:
            raise ValueError("missing vocabulary: " + word)
    review = root / ".gitea" if (root / ".gitea/pull_request_template.md").is_file() else root
    template = (review / "pull_request_template.md").read_text()
    if any(q not in template for q in QUESTIONS) or "blocks promotion" not in template:
        raise ValueError("missing review questions or blocking rule")
    if "contract-guard" not in (root / ".oberth/build.yaml").read_text():
        raise ValueError("guard is not wired into branch CI")
    issue = (review / "ISSUE_TEMPLATE/contract.md").read_text()
    for field in ("Direction:", "Repo:", "Contradiction:", "Function:", "Verdict:", "Evidence:", "Impact:", "Resolution:", "Guard:", "Done when:", "Source:", "Priority:"):
        if field not in issue:
            raise ValueError("issue template missing " + field)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("--baseline", action="store_true", help="record reviewed initial debt")
    parser.add_argument("--root", type=Path, default=Path(__file__).resolve().parents[2])
    args = parser.parse_args()
    root = args.root.resolve()
    check_adoption(root)
    if args.self_test:
        subprocess.run([sys.executable,"-I","-B",str(Path(__file__).with_name("test_check.py"))],check=True)
    registry, deprecations = validate_registries(root)
    findings = scan(root, files(root), registry, deprecations)
    actual = fingerprint(findings)
    baseline_path = root / ".oberth/contracts/baseline.json"
    if args.baseline:
        baseline_path.write_text(json.dumps({"findings": dict(sorted(actual.items())), "counts": dict(sorted(Counter(rule for rule, _, _ in findings).items()))}, indent=2) + "\n")
    baseline = json.loads(baseline_path.read_text())["findings"]
    new = actual - Counter(baseline)
    if new:
        for rule, name, line in sorted(set(findings)):
            if next(iter(fingerprint([(rule, name, line)]))) in new:
                print(f"{rule}: {name}: {line}")
        raise SystemExit("new contract violations; baseline increases require review")
    canonical = root / "gen/contracts"
    if canonical.is_dir():
        for name in ("check.py", "test_check.py"):
            if (canonical/name).read_bytes() != (root/".oberth/contracts"/name).read_bytes():
                raise ValueError("generated contract guard drift: " + name)
    for command in json.loads((root / ".oberth/contracts/generated-files.json").read_text())["checks"]:
        subprocess.run(command, cwd=root, check=True)
    print(f"contract guard: green; {sum(actual.values())} existing findings ({len(actual)} exact fingerprints)")


if __name__ == "__main__":
    main()
