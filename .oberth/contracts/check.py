"""Contract ratchet. Canonical owner: Oberth; emitted by its contract profile.

Baselines retain exact existing findings, never an allowance for new strings.
Review baseline reductions when removing debt; baseline increases need review.
"""
from collections import Counter
from datetime import date, datetime, timezone
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
RESIDUE = re.compile(r"/ho" + r"me/|(?:^|[=\"'])%[0-9]+\b|\bpa" + r"ne\s+%[0-9]+\b|road" + r"map#")
DEPRECATED = re.compile(r"\blegacy\b|\bdeprecat(?:e[ds]?|ing)\b|\b(?:sunset|remove)-by\b|\bsunset(?:s|ting)?\s+(?:(?:the|this|old|v\d+)\s+){0,3}(?:API|feature|endpoint|interface|alias|mode|shim|support)\b|\bfor now\b|\bback-?compat\w*\b(?!-[\w])|\bfor\s+compat(?:ibility)?\b|\bcompat(?:ibility)?[- ]alias\b|\bcompat(?:ibility)?[- ]window\b|\bcompat(?:ibility)?[- ]shim\b", re.I)
COMPAT = re.compile(r"\b(?:backward[s]?[- ]compat(?:ibility)?|compat(?:ibility)?[- ](?:mode|layer))\b|(?<![\w/-])shim\b(?![/\w-])", re.I)
DATED_UNTIL = re.compile(r"\buntil\s+(?:\d{4}-\d{2}-\d{2}|v?\d+(?:\.\d+)+|(?:the\s+)?next\s+(?:release|version)\b)", re.I)
REMOVAL = re.compile(r"\bremov\w*\s+(?:after|in|by|at)\s+(?:\d{4}-\d{2}-\d{2}|v?\d+(?:\.\d+)+)\b|\bthrough\s+\d{4}-\d{2}-\d{2}\b", re.I)
RETAINED_COMPAT = re.compile(r"\b(?:kept|keep|retain\w*|remains?\s+\w+)\s+for\s+(?:(?:backwards?|API)\s+){0,2}compat\w*\b", re.I)
OPERATIONAL_UNTIL = re.compile(r"\b(?:blocks?|polls?|waits?|blocking|polling|waiting|locked)\s+until\b", re.I)
UNTIL = re.compile(
    r"\b(?:(?:supported|removed|honou?red|accepted(?:\s+at\s+the\s+floor)?|workaround|(?:historical\s+collision|residual\s+integrity)\s+risk|onboarded)\s+until|stays?\s+local\s+until|(?:API|feature|endpoint|interface|alias|flag|option|field|support|fallback|mode|path)\s+(?:(?:is|are|will|be|only|still)\s+){0,3}(?:retained|kept|supported|available|enabled|accepted|remains?)\s+(?:available\s+)?until)\s+(?![:=,.)])\S"
    r"|\b(?:until\s+the\s+minimum\s+supported\b|(?:pattern|copy|script|tool)\s+locally\s+until\b|existing\s+installs?\s+continue\s+working\s+until\b|stays?\s+false\s+until\s+(?:\w+\s+)?qualification\s+(?:lifts?|removes?)\s+the\s+gate\b|keeps?\s+(?:its|the)\s+(?:old|previous|existing)\b[^.;]{0,60}\bpolicy\b[^.;]{0,40}\buntil\b)", re.I)

ENV = re.compile(r'''(?:Getenv|LookupEnv|getenv|os\.getenv|environ\.get|env\.(?:String|Bool|Int|Duration))\s*\(\s*["']([A-Z][A-Z0-9_]+)["']''')
ANNOTATION = re.compile(r'''["']((?:[\w.-]+\.)?oberth\.ci/[\w.-]+)["']''')
FLAG = re.compile(r'''\b(?:String|Bool|Int|Duration|StringVar|BoolVar|IntVar|DurationVar)(?:P)?\s*\([^\n)]*?["']([a-z][a-z0-9-]+)["']''')
SEAM = re.compile(r"_TEST_|_SKIP|ALLOW_INSECURE|_DEV_")

def retained_compatibility(line):
    # A compatibility path retained for old callers is a sunset claim. A
    # process shim retaining arguments or a sealed-state assertion is not.
    for sentence in re.split(r"[.;]", line):
        for match in COMPAT.finditer(sentence):
            before, after = sentence[:match.start()], sentence[match.end():]
            if re.search(r"\b(?:retain\w*|kept|keep)(?:\W+\w+){0,8}\W*$|\bremains?\s+for\b.*$", before, re.I):
                return True
            if re.match(r"\s+(?:(?:mode|path|layer)\s+)?(?:remain\w*\b|is\s+(?:kept|retained)\b|still\s+(?:exists?|supported)\b)", after, re.I):
                return True
    return False


def temporary_lifetime(line, go_source=False, prose=False):
    # A temporary resource or operation does not promise a feature's removal.
    # In Go, inspect comments and string prose, not local names or types.
    prose = prose or bool(re.match(r"^\s*(?://|/\*|\*|#)", line))
    if go_source:
        comment = re.search(r"//(.*)$|^\s*\*(.*)$", line)
        if comment:
            prose = True
            line = comment[1] or comment[2] or ""
        elif re.fullmatch(r"\s*temporary\s+[\w.*\[\]]+\s*,?\s*(?:`[^`]*`)?\s*", line, re.I):
            return False
        elif re.match(r"^\s*(?:func|var|const|type|return)\b", line) or re.match(r"^\s*[\w.]+\s*:?=", line):
            strings = re.finditer(r'"(?:\\.|[^"\\])*"|`[^`]*`', line)
            line = " ".join(quoted.group() for quoted in strings)
    # The word is the trigger. Exempt concrete resources and code identifiers,
    # never an open-ended list of feature nouns.
    resource = r"(?:files?|dirs?|directories|directory|buffers?|outputs?|keys?|credentials?|tokens?|listeners?|storage|snapshots?|roots?|pods?|units?|binaries|binary|downloads?|caches?|databases?|objects?|paths?|artifacts?)"
    feature = r"(?:API|feature|flag|option|setting|field|override|hack|copy|support|compatibility|workaround)"
    modifier = r"(?:(?:TLS|SA|S3|R2|prefix-scoped|probe|test|activation|banner|cosign|client|report|parent-side|Go|env|mounted-root|body/error|owned|raw|release|per-check|tool)\s+){0,3}"
    literals = list(re.finditer(r'"(?:\\.|[^"\\])*"|\'(?:\\.|[^\'\\])*\'|`[^`]*`', line))
    offset = 0
    for sentence in re.split(r";|\.(?=\s|$)", line):
        sentence_start = offset
        offset += len(sentence) + 1
        # Only an explicitly scoped runtime action qualifies; the operation
        # phrase alone cannot waive a temporary feature or compatibility mode.
        if re.search(r"\btemporarily\s+(?:redirect|disable)\s+(?:stdout|stderr|the\s+API)\s+(?:during|for|within)\s+(?:(?:this|the|each|a|an|single|current)\s+){0,2}(?:test|request|operation|call|transaction)\b", sentence, re.I) and len(re.findall(r"\btemporar(?:y|ily)\b", sentence, re.I)) == 1:
            continue
        for match in re.finditer(r"\btemporar(?:y|ily)\b", sentence, re.I):
            before, after = sentence[:match.start()], sentence[match.end():]
            # Identifier punctuation only exempts code, never prose labels.
            position = sentence_start + match.start()
            literal = next((item for item in literals if item.start() <= position < item.end()), None)
            if not prose and literal:
                # Hyphenated fixture names and f-string placeholders are data
                # identifiers; surrounding prose still gets its own trigger.
                if re.match(r"-\w", after) and re.fullmatch(r"[\w.-]+", literal.group()[1:-1]):
                    continue
                if re.search(r"(?i)\b(?:f|fr|rf)$", line[:literal.start()]) and re.search(r"\{\s*$", before) and re.match(r"\s*\}", after):
                    continue
            if not prose and literal is None:
                if re.match(r"\s*,\s*\w+\s*:?=", after):
                    continue
                if re.search(r"[!+]\s*$", before) and re.match(r"\s*(?:$|[{},)])", after):
                    continue
                if re.search(r"\breturn\s+$", before) and re.match(r"\s*(?:$|#)", after):
                    continue
                if re.match(r"\s*:?=|\.\w|\(", after):
                    continue
                if re.fullmatch(r"\s*", before) and re.match(r":\s*(?:[A-Z]\w*|str|int|bool|float|bytes)\b", after):
                    continue
                if re.match(r"\s*(?:[:,)]|$)", after) and re.search(r"(?:\bas\s+|[.(,]\s*)$", before):
                    continue
                if re.search(r"\b(?:if|elif|while)\s+$", before) and re.match(r"\s+(?:is|in)\b|\s*\{", after):
                    continue
                if re.search(r"[\"']\s*:\s*$", before) and re.match(r"\s*[,})]", after):
                    continue
            if re.search(r"\bDownloaded and verified\s+$", before) and re.fullmatch(r"\s+[\w-]+\s+v(?:\$\{[A-Z_]+\}|\d[.\w-]*)[\"']?\s*", after):
                continue
            if not prose and re.fullmatch(r"\(\)\s*", after):
                continue
            if re.match(r"\s+" + modifier + resource + r"\b", after, re.I):
                # A resource does not excuse a feature introduced after it.
                tail = re.sub(r"^\s+" + modifier + resource + r"\b", "", after, count=1, flags=re.I)
                if not re.match(r"\s+(?:" + feature + r"\b|\[[\w-]+\])", tail, re.I):
                    continue
            # Only adjacent failure wording is exempt. An error mentioned
            # later in a feature description cannot waive the feature lifetime.
            if re.match(r"\s+(?:(?:API|network|server|service|authentication|connection|delete|DNS|lookup)\s+){0,3}(?:outages?|failures?|errors?|unavailable)\b", after, re.I):
                continue
            # Runtime buffer swaps are resource operations, not API lifetimes.
            if re.match(r"\s+swapping\s+to\s+bytes\.Buffer\b", after, re.I):
                continue
            return True
    return False


def sunset_statement(line, go_source=False, prose=False):
    line = re.sub(r"\b(?:no|without)\s+compat(?:ibility)?[- ](?:window|shim)\b", "", line, flags=re.I)
    line = re.sub(r"\bno\s+compat(?:ibility)?[- ]alias\b", "", line, flags=re.I)
    if re.search(r"\bcompat(?:ibility)?[- ]alias\s+remains?\s+absent\b", line, re.I):
        line = re.sub(r"\bcompat(?:ibility)?[- ]alias\b", "alias", line, flags=re.I)
    line = re.sub(r"\bfloors?\s+until(?=[^.;]*\btimestamp\b)", "floor while", line, flags=re.I)
    line = re.sub(r"\bfor\s+compat(?:ibility)?\s+(?:checks|tests)\b", "", line, flags=re.I)
    line = re.sub(r"\bfail\s+closed\s+for\s+compat(?:ibility)?\b", "fail closed", line, flags=re.I)
    # Runtime failures and timestamp floors are completion conditions.
    line = re.sub(r"\btemporarily\s+(?:unavailable|unable|unreachable)\b", "", line, flags=re.I)
    line = re.sub(r"\bfloor\s+until\s+its\s+latest\s+timestamp\s+expires\b", "", line, flags=re.I)
    if DEPRECATED.search(line) or temporary_lifetime(line, go_source, prose) or DATED_UNTIL.search(line) or REMOVAL.search(line) or RETAINED_COMPAT.search(line) or retained_compatibility(line):
        return True
    # Field/variable names and runtime waiting are not compatibility promises.
    if go_source and re.match(r"^\s*(?:var\s+)?(?:Until\s+[\w.*\[\]]+|until\s+(?:time\.Time|string|bool|u?int\d*|float\d*|[A-Z]\w*))\s*(?:`[^`]*`)?\s*$", line):
        return False
    line = OPERATIONAL_UNTIL.sub("", line)
    # Cache/lock/readiness lifetimes describe runtime completion, not removal
    # of a compatibility path. Explicit temporary claims and dates above win.
    line = re.sub(r"\buntil\s+(?:replacement,\s*eviction,\s*or\s*shutdown|all\s+cryptographic\s+reads\s+finish|different\s+key\s+material\s+is\s+installed|a\s+(?:successful\s+)?probe)\b", "", line, flags=re.I)
    return bool(UNTIL.search(line))


def registered_sunset(line, records, name=None):
    # A bare id (or a substring of one) cannot authorize an exception.
    return any(all(record.get(key) for key in ("id", "since", "until", "replacement", "owner"))
               and ("paths" not in record or name in record["paths"])
               and re.search(r"(?<![\w-])" + re.escape(record["id"]) + r"(?![\w-])", line)
               for record in records)




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


def default_table_rows(lines):
    """Rows of tables whose header declares a default column.

    The word "default" in ordinary table prose (a law name, a branch, a
    profile) is not a copied default value; the header identifies one.
    """
    rows = set()
    active = False
    for index, line in enumerate(lines):
        if "|" not in line:
            active = False
            continue
        cells = [cell.strip() for cell in line.strip().strip("|").split("|")]
        if cells and all(re.fullmatch(r":?-{3,}:?", cell) for cell in cells):
            if index:
                headers = lines[index - 1].strip().strip("|").split("|")
                active = any(re.fullmatch(r"(?:current\s+|public\s+)?defaults?(?:\s+value)?(?:\s*\([^)]*\))?", header.strip("* `\t "), re.I) for header in headers)
        elif active:
            rows.add(index)
    return rows


def scan(root, names, registry, deprecations):
    found = []
    pipeline = "\n".join((root / p).read_text(errors="replace") for p in names if p.startswith(".oberth/") and p.endswith((".yaml", ".yml")))
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
        # Exact Wire-owned output paths and generator signatures only. Other
        # generated code and handwritten files remain subject to every rule.
        if name in (".oberth/check-surface-registry.py", "scripts/check-surface-registry.py") and content.startswith("#!/usr/bin/env python3\n# Generated from wire registry; regenerate with scripts/surface_registry.py.\n"):
            continue
        if name in ("internal/webhook/surface_registry_generated.go", "internal/annotations/surface_constants_generated.go") and content.startswith("// Generated from wire registry/surfaces.json; edit the registry and rerun scripts/surface_registry.py.\n"):
            continue
        if name == "tools/surface_registry_generated.py" and content.startswith("# Generated from wire registry/surfaces.json.\n"):
            continue
        if name == "docs/reference/surface-registry.md" and content.startswith("# Surface registry inventory\n\nGenerated from wire `registry/surfaces.json`."):
            continue
        lines = content.splitlines()
        # Surface metadata is structured: only an explicit sunset declares debt.
        sunset_lines = lines
        structured_sunsets = name == "registry/surfaces.json"
        if structured_sunsets:
            sunset_lines = [json.dumps(entry, sort_keys=True)
                            for entry in json.loads(content)["entries"]
                            if entry.get("until") is not None]
        elif name in ("deprecations.yaml", "registry/baseline.json"):
            sunset_lines = []
        for line in sunset_lines:
            if (structured_sunsets or sunset_statement(line, path.suffix == ".go", path.suffix in {".md", ".yaml", ".yml", ".txt", ".html", ".tpl", ".json"})) and not registered_sunset(line, deprecations, name):
                found.append(("law4-unregistered", name, line.strip()))
        default_rows = default_table_rows(lines) if path.suffix == ".md" else set()
        for index, line in enumerate(lines):
            def add(rule):
                found.append((rule, name, line.strip()))
            if CONFESSION.search(line) and not GENERATOR.search(line):
                add("law1-confession")
            if path.suffix==".md" and "|" in line and (index in default_rows or re.search(r"[A-Z][A-Z0-9]+_[A-Z0-9_]+",line)) and "generat" not in content[max(0,content.find(line)-300):content.find(line)].lower():
                add("law2-handtyped-table")
            if name.endswith("values.yaml") and re.match(r"^\s*[a-zA-Z][\w-]*:",line):
                key="value:"+line.split(":",1)[0].strip()
                if key not in entries: add("law3-surface")
            if RESIDUE.search(line):
                add("law5-residue")
            alternatives = re.findall(r"#nosec\b|//\s*(?:lint:(?:ignore|file-ignore)|gosec:(?:disable|enable))\b", line)
            if alternatives:
                specific = path.suffix == '.go' and re.fullmatch(
                    r"[ \t]*//lint:ignore[ \t]+SA[0-9]{4}[ \t]+(?!SA[0-9]{4}(?:\b|,)|//)\S[^\r\n]*", line)
                if not specific or len(alternatives) != 1:
                    add("rule11-suppression")
            for directive in re.finditer(r"//\s*nolint(?=[:\s]|$)", line):
                suppression = re.match(r"//nolint:([a-z][a-z0-9]*(?:,[a-z][a-z0-9]*)*)[ \t]+//[ \t]+\S", line[directive.start():])
                if not suppression or "all" in suppression[1].split(","):
                    add("rule11-suppression")
                    break
            if name.endswith("_test.go") and re.search(r'"go/(?:parser|ast)"', line):
                add("rule6-test-parser")
            if name.endswith(".go") and not any(part in name.split("/") for part in ("secret", "hygiene", "secretmem")):
                if re.search(r'"unsafe"|\.SetInt64\(0\)|\b(?:zeroBytes|wipeBytes|explicit_bzero)\(', line):
                    add("rule7-hygiene")
            if name.endswith((".go", ".py")):
                for kind, pattern in (("env", ENV), ("annotation", ANNOTATION), ("flag", FLAG)):
                    if kind == "annotation" and name.endswith("_test.go"):
                        continue  # Test literals include immutable historical fixtures, not public surfaces.
                    for match in pattern.finditer(line):
                        key = kind + ":" + match[1]
                        if kind == "annotation" and "label:" + match[1] in entries:
                            key = "label:" + match[1]
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
        if not key.startswith(("env:", "annotation:", "label:", "flag:", "value:")) or not entry.get("doc_anchor") or not entry.get("test_ref"):
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
        remaining = None
        removed = record.get("status") == "removed"
        if removed:
            removed_at = date.fromisoformat(record["removed"])
            if removed_at > date.today() or not record.get("paths"):
                raise ValueError("removed deprecation requires completed removal and paths")
            guard = record.get("removal_guard")
            for marker in ([guard] if guard else record["paths"]):
                path = (root / marker["path"]).resolve()
                if not path.is_relative_to(root.resolve()) or not marker.get("contains"):
                    raise ValueError("invalid removal path")
                present = path.is_file() and marker["contains"] in path.read_text()
                if guard and not present:
                    raise ValueError("removal guard missing: " + record["id"])
                if not guard and present:
                    raise ValueError("removed deprecation remains: " + record["id"])
        if re.fullmatch(r"\d{4}-\d{2}-\d{2}", until):
            remaining = (date.fromisoformat(until) - date.today()).days
            expired = remaining <= 0
        elif re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z", until):
            remaining = (datetime.fromisoformat(until.replace("Z", "+00:00")) - datetime.now(timezone.utc)).total_seconds() / 86400
            expired = remaining <= 0
        else:
            if not re.fullmatch(r"v?\d+\.\d+\.\d+", until):
                raise ValueError("invalid deprecation sunset")
            version = lambda v: tuple(int(n) for n in str(v).removeprefix("v").split("."))
            source = (root / record.get("version_source", "")).resolve()
            if not source.is_relative_to(root.resolve()) or not source.is_file():
                raise ValueError("version sunset requires canonical version_source")
            current = re.search(record.get("version_pattern", r"(?m)^version:\s*v?(\d+\.\d+\.\d+)\s*$"), source.read_text())
            if not current:
                raise ValueError("version sunset reached or version unreadable: " + record["id"])
            expired = version(current[1]) >= version(until)
        if expired and not removed:
            raise ValueError("expired deprecation: " + record["id"])
        if not removed and remaining is not None and remaining <= 30:
            print(f"WARNING: sunset {record['id']} expires {until} in {remaining:g} days; "
                  f"owner: {record['owner']}; track removal: {record['replacement']}", file=sys.stderr)
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
