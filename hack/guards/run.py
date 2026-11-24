"""Run plan-backed source, text, history and reachability checks."""
import ast
import json
import os
from pathlib import Path
import re
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT/"hack"))
import history
from tools import go_tool as GO
MESSAGES = json.loads(Path(__file__).with_name("messages.json").read_text())
RULE = re.compile(r"^.+:\d+: \[([A-Z][A-Z0-9-]*)\](?: |$)")
KEYWORDS = re.compile(r"keywords: \[([A-Z, ]+)\]")
SKIPS = r'\.Skip(f|Now)?[[:space:]]*\([[:space:]]*(\)|"[[:space:]]*")'

class NamesError(ValueError): pass

def finding(rule, file=".", line=1, detail=""):
    file = str(file).encode("unicode_escape").decode("ascii")
    if rule == "NAME": return f"{file}:{line}: [{rule}]"
    reason, remedy = MESSAGES[rule.rstrip("0123456789")]
    return f"{file}:{line}: [{rule}] {reason} {remedy}" + (f" Detail: {detail}" if detail else "")

def command(args, data=None, binary=False, env=None):
    result = subprocess.run(args, input=data, text=not binary, capture_output=True, check=False, env=GO.environment() if args[0] == "go" else env)
    if result.returncode: result.stdout += result.stderr
    return result

def grep(args, data=None, program=("git", "grep")):
    result = command([program[0], "--attr-source="+checked_bytes(["git", "hash-object", "-t", "tree", os.devnull]).decode().strip(), *program[1:], "--no-textconv", *args], data)
    if result.returncode not in (0, 1): raise ValueError(result.stdout.strip())
    return result.stdout.splitlines()

def text_paths(check):
    data = json.loads(Path("docs/plans/retired.json").read_text())
    return [".", *(":!" + e["path"].rstrip("/") for e in data["exemptions"] if check in e["checks"])]

def hits(args, rule, data=None, program=("git", "grep")):
    return [finding(rule, file, line, term) for file, line, term in (row.split(":", 2) for row in grep(args, data, program))]

def vocabulary():
    data = json.loads(Path("docs/plans/retired.json").read_text())
    if any(not r["tokens"] or any(not t or t != t.strip() for t in r["tokens"]) for r in data["rules"]):
        raise ValueError("Retirement tokens must be nonempty exact selectors.")
    return [hit for rule in data["rules"] for hit in hits(
        ["-nIwoF", *(v for t in rule["tokens"] for v in ("-e", t)), "--", *text_paths("retired_vocabulary")], rule["id"])]

def checked_bytes(args, data=None):
    result = command(args, data, binary=True)
    if result.returncode: raise ValueError("Unable to read or parse guard input.")
    return result.stdout

def name_hits(pattern, place, value, offset=0):
    return [finding("NAME", place, line) for line, text in enumerate(value.splitlines(), offset+1) if pattern.search(text)]

def masked_path(pattern, name):
    return "/".join("[masked]" if "[masked]" in part else part for part in pattern.sub("[masked]", name).split("/"))

def name_pattern(terms, indices):
    return re.compile("|".join(re.escape(t) for i, t in enumerate(terms, 1) if i in indices) or r"(?!)", re.I)

def name_matches(head, ref, terms):
    selected = [*terms, *history.CREDIT_TEXT, "#", "`", "~"]
    result = command(["git", "grep", "--threads=2", "-a", "-n", "-z", "-i", "-F", "-f", "-", *([head] if ref else []), "--", ":/"],("\n".join(selected)+"\n").encode(),binary=True)
    if result.returncode not in (0, 1): raise ValueError("Unable to read tracked contents.")
    return sorted(re.findall(rb"([^\0]*)\0([0-9]+)\0([^\n]*)\n", result.stdout),key=lambda row:(row[0],int(row[1])))

def names(ref=""):
    try: return name_rows(ref)
    except Exception:
        raise NamesError from None

def name_rows(ref):
    if not (external := os.environ.get("OBERTH_GUARDS_NAMES_FILE")): return [finding("NAME-POLICY")]
    file = Path(external).resolve(strict=True)
    root = Path(checked_bytes(["git", "rev-parse", "--show-toplevel"]).decode().strip()).resolve()
    if file.is_relative_to(root): return [finding("NAME-POLICY", detail="Names file must resolve outside the repository.")]
    raw = file.read_text().splitlines()
    terms = [term.removeprefix("host:") for term in raw]
    if not terms or any(not term.strip() for term in terms):
        return [finding("NAME-POLICY", detail="names: empty file or blank pattern")]
    hosts = {i for i, term in enumerate(raw, 1) if term.startswith("host:")}
    pattern, plain, host = name_pattern(terms, set(range(1,len(terms)+1))), name_pattern(terms, set(range(1,len(terms)+1))-hosts), name_pattern(terms,hosts)
    scope = json.loads((ROOT/"hack/guards/host_paths.json").read_text())
    files = {template.format(host=terms[i-1].casefold()) for i in hosts for template in scope["files"]}
    allowed = lambda name: name.startswith(tuple(scope["prefixes"])) or name in files
    head = checked_bytes(["git", "rev-parse", "--verify", "--end-of-options", (ref or "HEAD")+"^{commit}"]).decode().strip()
    paths = ["git", "ls-tree", "-rz", "--full-tree", head] if ref else ["git", "ls-files", "--full-name", "-z", "--", ":/"]
    rows = []
    for entry in filter(None, checked_bytes(paths).decode(errors="surrogateescape").split("\0")):
        metadata, name = entry.split("\t", 1) if ref else ("", entry)
        place = masked_path(pattern, name)
        rows.extend(name_hits(plain if allowed(name) else pattern, place, name))
        if metadata.startswith("120000 "):
            rows.extend(name_hits(plain if allowed(name) else pattern, place, checked_bytes(["git", "cat-file", "blob", metadata.split()[2]]).decode(errors="replace")))
        elif not ref and (root/name).is_symlink():
            rows.extend(name_hits(plain if allowed(name) else pattern, place, os.readlink(root/name)))
        elif allowed(name) and name.endswith((".json", ".yaml", ".yml")) and (metadata.startswith("100") if ref else (root/name).is_file()):
            raw = checked_bytes(["git","cat-file","blob",metadata.split()[2]]) if ref else (root/name).read_bytes()
            if name.endswith((".yaml", ".yml")):
                raw = checked_bytes(["go", "-C", str(ROOT), *GO.args(ROOT, "run", "-p=2", "-buildvcs=false", "./hack/guards", "--yaml")[1:]], raw)
            if history.structured_credit(raw,host): rows.append(finding("CREDIT",place))
    document, depth, fence, seen = "", 0, "", False
    for name, line, text in name_matches(head,ref,terms):
        name = name.decode(errors="surrogateescape").removeprefix(head+":")
        value = text.decode(errors="replace")
        if name != document: document, depth, fence, seen = name, 0, "", False
        boundary = re.match(r"^ {0,3}(`{3,}(?=[^`]*$)|~{3,})(.*)$", value)
        if boundary and (not fence or re.fullmatch(re.escape(fence[0])+"{"+str(len(fence))+r",}\s*", value.strip())):
            fence = "" if fence else boundary[1]
        heading = re.match(r"^ {0,3}(#{1,6})(?:[ \t]+(.*?)(?:[ \t]+#+)?)?[ \t]*$",value) if not fence and not boundary else None
        if heading and name in scope["sections"]:
            if len(heading[1]) <= 2: depth = 0
            if heading[2] == scope["sections"][name]:
                if seen or len(heading[1]) != 2:
                    rows.append(finding("NAME-SECTION", masked_path(pattern,name),int(line),
                                        "duplicate Agent plugins heading" if seen else "Agent plugins heading must be level 2"))
                depth, seen = (2 if not seen and len(heading[1]) == 2 else 0), True
        rows.extend(name_hits(plain if allowed(name) or depth else pattern,masked_path(pattern,name),value,int(line)-1))
        if history.source_attribution(value,host) and (credit := finding("CREDIT",masked_path(pattern,name),int(line))) not in rows:
            rows.append(credit)
    address = command(["git", "config", "--get", "user.email"], binary=True).stdout.strip()
    for sha, kind, raw in history.metadata(head):
        rows.extend(name_hits(pattern, kind.decode()+"/"+sha, history.public_metadata(raw, address).decode(errors="replace")))
    for tag in checked_bytes(["git", "for-each-ref", "--format=%(refname)", "refs/tags"]).decode().splitlines():
        rows.extend(name_hits(pattern, masked_path(pattern, tag), tag))
    return rows

def tool_output(rule, result):
    lines = [line for line in result.stdout.splitlines() if not re.fullmatch(r"exit status \d+", line)]
    if result.returncode and (rule == "HISTORY" or not any(RULE.search(line) for line in lines)):
        return [finding(rule, detail="\n".join(lines))]
    return lines

def walk_steps(tracked):
    file = "test/walk/walklib.py"
    if file not in tracked.split("\0"): return ""
    tree = ast.parse(Path(file).read_text())
    tables = [node.value for node in tree.body if isinstance(node, ast.Assign)
              and any(isinstance(t, ast.Name) and t.id == "STEPS" for t in node.targets)]
    if len(tables) != 1 or not isinstance(tables[0], (ast.Tuple, ast.List)):
        raise ValueError("Walk needs one literal STEPS table.")
    steps = tables[0].elts
    if not all(isinstance(s, ast.Call) and isinstance(s.func, ast.Name) and s.func.id == "Step" and len(s.args) >= 3 for s in steps):
        raise ValueError("Walk table needs named Step entries and actions.")
    names = [ast.literal_eval(s.args[0]) for s in steps]
    if any(not isinstance(name, str) or not name.strip() for name in names) or len(names) != len(set(names)):
        raise ValueError("Walk step names must be nonempty and unique.")
    return "\n".join(names)

def walk_result(steps):
    tracked = command(["git", "ls-tree", "-rz", "--name-only", "HEAD", "--", "test/walk/results/"]).stdout
    files = sorted(p for p in tracked.split("\0") if re.fullmatch(r"test/walk/results/[^/]+\.json", p))
    if not files:
        return "", []
    file = files[-1]
    result = json.loads(command(["git", "show", "HEAD:"+file]).stdout)
    if (not isinstance(result, dict) or set(result) != {"commit", "steps"}
            or not isinstance(result["commit"], str) or not re.fullmatch(r"[0-9a-f]{40}", result["commit"])
            or not isinstance(result["steps"], list)):
        raise ValueError(file+": expected commit and steps")
    head = command(["git", "rev-parse", "HEAD"]).stdout.strip()
    if result["commit"] != head:
        return "", [f"walk result: {file} stale; ran on {result['commit']}; checking {head}"]
    seen, passed = set(), []
    for row in result["steps"]:
        if (not isinstance(row, dict) or set(row) != {"name", "verdict", "evidence_sha256"}
                or row["name"] not in steps.splitlines() or row["name"] in seen or row["verdict"] not in ("pass", "fail", "blocked", "skipped")
                or not isinstance(row["evidence_sha256"], str) or not re.fullmatch(r"[0-9a-f]{64}", row["evidence_sha256"])):
            raise ValueError(file+": invalid, duplicate or undefined walk step: "+str(row))
        seen.add(row["name"])
        if row["verdict"] == "pass":
            passed.append(row["name"])
    return "\n".join(passed), []

def analysis():
    tracked = command(["git", "ls-files", "-z"]).stdout
    steps = walk_steps(tracked)
    passed, notes = walk_result(steps)
    os.environ.update(OBERTH_GUARDS_WALK_STEPS=steps, OBERTH_GUARDS_WALK_PASSED=passed)
    module = command(["go", "list", "-m"]).stdout.strip()
    result = command(GO.args(ROOT, "run", "-p=2", "-buildvcs=false", "./hack/guards", module), tracked)
    try:
        report = json.loads(result.stdout.partition("\n")[0])
        return module, dict(report, Lines=notes + report["Lines"])
    except ValueError:
        return module, {"Lines": tool_output("GO", result), "Roots": [], "Product": ""}

def analyzer(*args):
    return GO.tool(ROOT/"hack/tools/deadcode.mod", "golang.org/x/tools/cmd/deadcode", command, *args)

def unreachable(module, test, targets):
    result = analyzer("-json", "-test="+test, "-filter", "^"+re.escape(module)+"(/|$)", *targets)
    if result.returncode: return tool_output("GO", result)
    return [finding("DEADCODE", fn["Position"]["File"], fn["Position"]["Line"], fn["Name"])
            for package in json.loads(result.stdout) or [] for fn in package["Funcs"]]

def deadcode(module, report):
    rows = report["Lines"][:]
    if not report["Product"]: return rows
    rows.extend(unreachable(module, "true", ["./..."]))
    if any((hit := RULE.search(row)) and hit[1] == "GO" for row in rows):
        return rows
    product = Path(report["Product"])
    root = module+"/"+str(product)
    if any(p.is_symlink() for p in (product, *product.glob("*.go"))) or root not in (report["Roots"] or []):
        return rows + ([finding("PRODUCT", str(product))] if product.exists() or product.is_symlink() else
                       [f"reachability: skipped, {product} is not built yet"])
    built = command(["go", "build", "-p=2", "-buildvcs=false", "-buildmode=exe", "-o", os.devnull, root])
    if built.returncode:
        return rows + tool_output("GO", built)
    deps = command(["go", "list", "-deps", "-f", "{{.ImportPath}}", *report["Roots"]])
    if deps.returncode:
        return rows + tool_output("GO", deps)
    rows.extend(finding("REACH", pkg.removeprefix(module).lstrip("/") or ".", detail=pkg)
                for pkg in sorted(set(report["Packages"] or []) - set(deps.stdout.splitlines())))
    return list(dict.fromkeys(rows + unreachable(module, "false", report["Roots"])))

def pins(ref="HEAD"):
    return tool_output("PIN-POLICY", command(["python3", "-I", "hack/plans/policy.py", ref]))

def checks():
    policy = command(["python3", "-I", "hack/plans/parity.py", "--check"])
    if policy.returncode: return tool_output("POLICY", policy)
    module, report = analysis()
    rows = pins() + vocabulary() + names() + tool_output("SCHEMA", command(GO.args(ROOT, *"test -p=2 -buildvcs=false -count=1 -timeout=120s ./internal/store -run ^TestMigrationReleaseCoverage$".split())))
    markers = "|".join(s.strip() for s in KEYWORDS.search(Path(".golangci.yml").read_text())[1].split(","))
    rows.extend(hits(["-nIwE", markers, "--", *text_paths("leftovers"), ":!*.go", ":!.golangci.yml"], "LEFTOVER"))
    rows.extend(hits(["-nIE", SKIPS, "--", "*_test.go", *text_paths("leftovers")[1:]], "SKIP"))
    return rows + tool_output("HISTORY", command(["python3", "-I", "hack/history.py", "HEAD"])) + deadcode(module, report)

if __name__ == "__main__":
    try:
        mode = sys.argv[1] if len(sys.argv) > 1 else ""
        result = names(sys.argv[2]) + pins(sys.argv[2]) if mode == "names" else deadcode(*analysis()) if mode == "deadcode" else checks()
        print("\n".join(result))
        sys.exit(any(RULE.search(line) for line in result))
    except NamesError:
        sys.exit(finding("TOOL"))
    except (ValueError, KeyError, TypeError, OSError, AssertionError, SyntaxError, subprocess.SubprocessError) as error:
        sys.exit(finding("TOOL", detail=str(error)))
