#!/usr/bin/env python3
"""Rebuild the parity inventory at the pinned reference without importing it.

The portable --check needs only Python 3. Explicit --reference
regeneration also uses PyYAML and Go (standard library only). No server, cluster,
network, application build or application test execution is involved.
"""
import argparse
import collections
import functools
import fnmatch
import hashlib
import json
import runpy
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import tempfile


ROOT = Path(__file__).resolve().parents[2]
BASE = "5e1200a2"
REFERENCE = ROOT
ROWS = collections.defaultdict(dict)
# P5 policy-data exceptions: README names these two finite sanitizer symbols.
VM_TERMS = ("kubevirt", "firecracker", "cloud-hypervisor", "cloud_hypervisor", "qemu", "libvirt", "virtualbox", "vmware", "hyper-v", "hyperv", "kvm", "xen")


def neutral_vm(value):
    # Also normalize embedded table/column/function identifiers, not just prose.
    value = re.sub(VM_TERMS[0], "vm", str(value), flags=re.I)
    for term in VM_TERMS[1:]:
        pattern = re.escape(term) if len(term) > 3 else r"(?<![a-z])" + re.escape(term) + r"(?![a-z])"
        value = re.sub(pattern, "vm_runtime", value, flags=re.I)
    return value


@functools.cache
def read_source_map(path):
    file = Path(path).resolve(strict=True)
    assert not file.is_relative_to(ROOT), "Reference source map must be outside the repository."
    data = json.loads(file.read_text())
    assert set(data) == {"aliases", "public_replacements", "client_paths", "source_folders"}, "Invalid reference source map."
    return data


def source_map():
    path = os.environ.get("OBERTH_PARITY_SOURCE_MAP")
    return read_source_map(path) if path else {"aliases": {}, "public_replacements": {}, "client_paths": {}, "source_folders": []}


def neutral_source(value):
    """Use externally supplied agent aliases for reference source evidence."""
    value = str(value)
    for old, new in source_map()["aliases"].items():
        def spelling(match):
            text = match[0]
            return new.upper() if text.isupper() else new.capitalize() if text[0].isupper() else new
        value = re.sub(re.escape(old), spelling, value, flags=re.I)
    return neutral_vm(value)


def guard_names(value):
    assert neutral_vm(value) == value, "banned VM name in generated output/input"



def run(*args):
    return subprocess.check_output(args, cwd=REFERENCE, env={**os.environ, "GOMAXPROCS": "1"}, text=True)


def disposition_label(value):
    match = re.match(r"^(NEW \([^)]*\)|KEEP|CHANGE|RETIRED)(?=[:;]|$)", value)
    assert match, "invalid disposition label: " + value
    return match[1]


def compact(s):
    return re.sub(r"\s+", " ", str(s)).strip()


PRODUCT_REPLACEMENTS = {
        r"\bOpenBao\b": "secret store",
        r"\bVault\b": "secret store", r"\bGitHub\b": "forge", r"\bCodeberg\b": "forge",
        r"\bArgo(?: Workflows)?\b": "native workflow", r"\bKubernetes\b": "cluster",
        r"\bHelm\b": "chart", r"\bRekor\b": "audit witness", r"\bTrivy\b": "scanner",
        r"\bSigstore\b": "signature verifier", r"\bCloudflare\b": "tunnel provider",
        r"\bGoogle\b": "identity provider", r"\bGCP\b": "identity provider",
        r"\bDocker\b": "image builder", r"\bBuildKit\b": "image builder",
        r"(?:github|codeberg)/acme/[\w/-]+": "<qualified-repository>",
        r"https?://[^\s\"<>]+": "<endpoint>",
    }


def public(s):
    # Descriptions use generic terms; exact source identifiers and paths remain
    # evidence. Never import deployment examples from descriptions into a plan.
    s = compact(s)
    for pattern, replacement in (source_map()["public_replacements"] | PRODUCT_REPLACEMENTS).items():
        if pattern.startswith(r"\b") and pattern.endswith(r"\b"):
            # Never change identifiers such as an image name, namespace,
            # metric suffix, flag spelling or audit-action string.
            pattern = r"(?<![\w./:-])" + pattern[2:-2] + r"(?![\w/])"
        s = re.sub(pattern, replacement, s, flags=re.I)
    s = s.replace("native native workflow", "native workflow")
    s = re.sub(r"ConfigMap/oberth-sync-limits/[a-z][a-z0-9_-]*", "ConfigMap/oberth-sync-limits/<capability>", s)
    return neutral_vm(s)


def cell(s):
    return compact(s).replace("|", "&#124;").replace("<", "&lt;").replace(">", "&gt;")


def loc(f):
    return f'{f["File"]}:{f["Line"]}'


def add(category, name, owner, behavior, evidence, inspection="needs test", disposition="KEEP"):
    assert owner in {f"C{i}" for i in range(1, 15)}
    assert disposition.startswith(("KEEP", "CHANGE", "RETIRED", "NEW"))
    name = neutral_source(name)
    key = name
    assert key not in ROWS[category], (category, name)
    ROWS[category][key] = [name, owner, neutral_source(public(behavior)) + "; " + neutral_source(evidence), disposition, neutral_source(inspection), ""]


def literal(s):
    s = s.strip()
    if s.startswith('`') and s.endswith('`'):
        return s[1:-1]
    if s.startswith('"') and s.endswith('"'):
        try:
            return json.loads(s)
        except json.JSONDecodeError:
            return None
    return None


def source_facts():
    with tempfile.TemporaryDirectory(prefix="oberth-parity-", dir=os.environ.get("GOTMPDIR", str(Path.home() / ".cache/go-tmp"))) as tmp:
        facts = json.loads(run("go", "run", "-p=1", str(ROOT / "hack/plans/scan.go"), tmp))
        tools = json.loads(run("go", "run", "-p=1", str(Path(tmp) / "registry.go")))
    return facts, tools


def test_index():
    index = collections.defaultdict(list)
    for path in run("git", "ls-files", "*_test.go").splitlines():
        text = (REFERENCE / path).read_text()
        for m in re.finditer(r"(?m)^func (Test\w+)\(t \*testing.T\)", text):
            end = text.find("\nfunc ", m.end())
            body = text[m.start():end if end >= 0 else len(text)]
            index[Path(path).parent.as_posix()].append((path, m.group(1), body))
    return index


@functools.lru_cache(maxsize=None)
def inspection_for(file, token):
    # Only cite a named test if it actually mentions the exact contract name.
    # A package-level test is not evidence for every row in that package.
    for path, name, body in TESTS.get(str(Path(file).parent), []):
        if not Path(path).stem.startswith(Path(file).stem + "_"):
            continue
        if '"' + token + '"' in body or '`' + token + '`' in body:
            return name + " (" + path + "; candidate: differential coverage still required)"
    return "needs test"


def cli(facts):
    functions = {f["Name"]: f for f in facts if f["Kind"] == "function" and f["File"].startswith("cmd/oberth/")}
    calls = collections.defaultdict(list)
    cases = collections.defaultdict(list)
    for f in facts:
        if not f["File"].startswith("cmd/oberth/"):
            continue
        if f["Kind"] == "call":
            calls[f["Name"]].append(f)
        if f["Kind"] == "case":
            cases[f["Name"]].append(f)
    roots = {}
    for case in cases["runCLI"]:
        if case["Args"] != ["arguments[0]"]:
            continue
        for v in case["Values"] or []:
            name = literal(v)
            if name is None:
                continue
            target = next((c for c in case["Calls"] or [] if c in functions and c != "runCLI"), "runCLI")
            roots[name] = target
            add("CLI commands", "oberth " + name, "C11", "Dispatch to " + target + "; " + compact(case["Text"])[0:220], loc(case), inspection_for(case["File"], name))
    command_functions = collections.defaultdict(set)
    for command, target in roots.items():
        if target != "runCLI":
            command_functions[target].add(command)
    # Dispatchers obtain the verb from the first argument. Read every dispatch
    # case, equality guard, and map indexed by that verb (including hidden verbs).
    pending = list((name, target) for name, target in roots.items() if not name.startswith("-") and target != "runCLI")
    visited = set()
    while pending:
        prefix, target = pending.pop(0)
        if (prefix, target) in visited:
            continue
        visited.add((prefix, target))
        fn = functions[target]
        if target == "runMCPCall":
            continue  # Its first positional selects a tool, not a CLI command.
        text = fn["Text"]
        aliases = {"arguments[0]", "args[0]", "positional[0]"}
        for m in re.finditer(r"(\w+)(?:,\s*\w+)?\s*:?=\s*(?:arguments|args|positional)\[0\]", text):
            aliases.add(m[1])
        children = {}
        for case in cases[target]:
            if case["Args"][0] not in aliases:
                continue
            for expr in case["Values"] or []:
                child = literal(expr)
                if child is None or child.startswith("-"):
                    continue
                callee = next((c for c in case["Calls"] or [] if c in functions and c != target and c.startswith("run")), target)
                children[child] = (case, callee)
        for alias in aliases:
            for m in re.finditer(re.escape(alias) + r'\s*(?:==|!=)\s*"([a-z][a-z0-9-]*)"', text):
                child = m[1]
                if child in children:
                    continue
                tail = text[m.end():]
                # Only add guards that dispatch, reject a removed command, or
                # enumerate the selected verb; avoid generic selector validation.
                cm = re.search(r"return (run\w+)\(", tail[:500])
                callee = cm[1] if cm and cm[1] in functions else target
                evidence = {**fn, "Line": fn["Line"] + text[:m.start()].count("\n")}
                children[child] = (evidence, callee)
        for m in re.finditer(r'map\[string\]string\{([^}]+)\}\[action\]', text):
            for child in re.findall(r'"([a-z-]+)"\s*:', m[1]):
                children.setdefault(child, (fn, target))
        for child, (evidence, callee) in children.items():
            full = prefix + " " + child
            if "oberth " + full in ROWS["CLI commands"]:
                continue
            if prefix.count(" ") >= 2 or prefix.endswith(" " + child):
                continue
            disposition = "KEEP"
            behavior = "Dispatch via " + target + " to " + callee
            if child == "resources" and target == "runRunDetail":
                behavior = "Refuse removed per-step resource reporting"
                disposition = "RETIRED: explicit refusal at reference; REWRITE.md §1.5 removes compatibility surfaces"
            add("CLI commands", "oberth " + full, "C11", behavior, loc(evidence), inspection_for(fn["File"], child), disposition)
            command_functions[callee].add(full)
            if callee != target:
                pending.append((full, callee))
    # Include alternate executable entry points and internal helper dispatch.
    main = functions["main"]
    for token, target in [("internal-hold-fs", "holdfs.Run"), ("oberth-git-ssh", "runGitSSH")]:
        add("CLI commands", token if token.startswith("oberth-") else "oberth " + token, "C11", "Internal executable entry dispatch to " + target, loc(main))
    hook = next(f for f in facts if f["Kind"] == "function" and f["Name"] == "ReceiveHookInvocation")
    for token in sorted(set(re.findall(r'kind != "([a-z-]+)"', hook["Text"]))):
        add("CLI commands", token + " [hook executable basename]", "C11", "Native receive-hook entry; fixed argument shape; 30-second bound; invoked through main", loc(hook), inspection_for(hook["File"], token))
    groups = next(f for f in facts if f["Kind"] == "value" and f["Name"] == "commandGroups")
    for token in re.findall(r'"([a-z-]+)"\s*:', groups["Values"][0]):
        if token not in roots:
            add("CLI commands", "oberth " + token + " --help [stale group]", "C11", "Only group help is handled; ordinary execution has no dispatcher", loc(groups), "needs test", "RETIRED: stale help-only group; no command exists at the reference (REWRITE.md §1.5, #1655)")
    # Each registration gets its own row; helper registrations are not mistaken
    # for global flags. A static reverse call graph gives their consumers.
    reverse = collections.defaultdict(set)
    for fn, items in calls.items():
        for item in items:
            callee = item["Values"][0]
            if callee in functions:
                reverse[callee].add(fn)
    def consumers(fn):
        seen, todo, names = set(), [fn], set()
        while todo:
            current = todo.pop()
            if current in seen:
                continue
            seen.add(current)
            names.update(command_functions[current])
            todo.extend(reverse[current] - seen)
        return ", ".join(sorted(names)) or "internal parser; see registration function"
    methods = r"\.(?:String|Bool|Int|Int64|Uint|Uint64|Float64|Duration|Var|Func|BoolFunc)(?:Var)?$"
    for fn, items in sorted(calls.items()):
        for f in items:
            callee, args = f["Values"][0], f["Args"] or []
            if not re.search(methods, callee):
                continue
            # Exclude unrelated String()/Duration() methods by requiring the
            # documented flag-registration argument structure.
            is_var = callee.endswith("Var")
            index = 1 if is_var else 0
            if callee.endswith((".Func", ".BoolFunc")):
                index = 0
            if len(args) < index + 2:
                continue
            receiver = callee.rsplit(".", 1)[0]
            if not (receiver in {"flags", "flag", "options.flags"} or receiver.endswith(".flags")):
                continue
            if literal(args[index]) is None:
                # A registration loop in the client binding command declares
                # a finite table of name/help/default tuples.
                assert fn == "runClientRepository" and args[index] == "item.name", loc(f)
                for name, description in re.findall(r'\{&o\.\w+,\s*"([a-z-]+)",\s*"([^"\n]*)"\}', functions[fn]["Text"]):
                    add("CLI flags", "--" + name + " [" + fn + "]", "C11", description + '; type StringVar; default ""; consumers ' + consumers(fn), loc(f), inspection_for(f["File"], name))
                continue
            name = literal(args[index])
            description_index = 1 if callee.endswith((".Func", ".BoolFunc")) else len(args)-1
            desc = literal(args[description_index])
            if desc is None:
                desc = args[description_index]
            default = args[index+1] if not (callee.endswith(".Var") or callee.endswith("Func")) else "custom value"
            behavior = f"{desc}; type {callee.split('.')[-1]}; default {default}; consumers {consumers(fn)}; registration conditions in {fn} apply"
            add("CLI flags", "--" + name + " [" + fn + "]", "C11", behavior, loc(f), inspection_for(f["File"], name))
    # Manual switches outside flag.FlagSet are real flags too.
    for name, evidence, behavior in [
        ("--schema [version]", functions["runCLI"], "Print the migration catalog version"),
        ("--version [root]", functions["runCLI"], "Dispatch to version"),
        ("--help/-help/-h [command parser]", functions["parseOperatorFlags"], "Return command help before command execution"),
    ]:
        add("CLI flags", name, "C11", behavior, loc(evidence))
    for fn, function in functions.items():
        for match in re.finditer(r'(?:argument|arg)\s*(?:==|!=)\s*"(--[a-z][a-z-]*)"', function["Text"]):
            name = match[1]
            key = name + " [" + fn + "; manual switch]"
            if key not in ROWS["CLI flags"]:
                add("CLI flags", key, "C11", "Manual argument handling in " + fn + "; " + compact(function["Text"][max(0,match.start()-60):match.end()+180]), f'{function["File"]}:{function["Line"] + function["Text"][:match.start()].count(chr(10))}', inspection_for(function["File"], name))


def mcp(facts, tools):
    definitions = {literal(f["Args"][0]): f for f in facts if f["Kind"] == "call" and f["Values"] == ["tool"] and f["Args"] and literal(f["Args"][0])}
    assert len(tools) == len(definitions), (len(tools), len(definitions))
    def args(tool, schema, prefix, evidence):
        required = schema.get("required", [])
        for name, spec in sorted(schema.get("properties", {}).items()):
            path = prefix + name
            behavior = f'{"required" if name in required else "optional"}; {spec.get("type", "schema-defined")}; {spec.get("description", "")}'
            bounds = {k: v for k, v in spec.items() if k not in {"description", "properties", "type"}}
            if bounds:
                behavior += "; constraints " + json.dumps(bounds, sort_keys=True, separators=(",", ":"))
            add("MCP arguments", tool + "." + path, "C1", behavior, evidence, inspection_for("internal/api/mcp.go", tool))
            args(tool, spec, path + ".", evidence)
            if "items" in spec:
                args(tool, spec["items"], path + "[].", evidence)
    for tool in sorted(tools, key=lambda t: t["name"]):
        name = tool["name"]
        f = definitions[name]
        desc = tool["description"]
        # Preserve the first complete sentence; full definition is the oracle.
        first = re.split(r"\. (?=[A-Z])", desc)[0]
        schema = tool["inputSchema"]
        add("MCP tools", name, "C1", first + "; object input; additionalProperties=" + str(schema.get("additionalProperties", "unspecified")).lower(), loc(f), inspection_for(f["File"], name))
        args(name, schema, "", loc(f))


def http(facts):
    routes = {}
    for f in facts:
        if f["Kind"] != "call" or not re.search(r"\.Handle(?:Func)?$", f["Values"][0]) or not f["Args"]:
            continue
        expr = f["Args"][0]
        pattern = literal(expr)
        if pattern is not None and re.match(r"(?:GET|POST|PUT|DELETE|PATCH) /", pattern):
            routes[pattern] = (f, compact(f["Text"])[0:250])
        elif f["File"] == "internal/api/source.go":
            value = next(v for v in facts if v["Kind"] == "value" and v["File"].startswith("internal/sourcedelivery/") and v["Name"] == "LoadPath")
            routes["POST " + literal(value["Values"][0])] = (f, "Private source listener; authenticate, authorize and serve the source input")
        elif f["File"] == "internal/api/server.go" and expr == '"GET "+path':
            for path in re.findall(r'"(/[a-z]+)"', (REFERENCE / f["File"]).read_text().split("for _, path := range []string{")[1].split("}")[0]):
                routes["GET " + path] = (f, "Serve the " + path[1:] + " page")
        elif f["File"] == "internal/api/administration.go":
            table = next(v for v in facts if v["Kind"] == "value" and v["Name"] == "adminRoutes")
            for action, tool in re.findall(r'"([a-z-]+)"\s*:\s*"([a-z_]+)"', table["Values"][0]):
                routes["POST /api/admin/" + action] = (f, "Unscoped administrator JSON action " + tool + "; no query arguments; bounded body")
        elif f["File"] != "internal/api/router.go":
            raise AssertionError(("unresolved HTTP registration", loc(f), expr))
    for name, (f, behavior) in sorted(routes.items()):
        add("HTTP endpoints", name, "C6" if name == "POST /api/source/v1/load" else "C11" if f["File"].startswith("cmd/") else "C1", behavior, loc(f), inspection_for(f["File"], name))


def yaml_nodes(path):
    return list(yaml.compose_all(path.read_text()))


def walk_yaml(node, prefix=""):
    if isinstance(node, yaml.MappingNode):
        if not node.value:
            yield prefix, node
        for key, value in node.value:
            yield from walk_yaml(value, prefix + ("." if prefix else "") + key.value)
    elif isinstance(node, yaml.SequenceNode):
        if not node.value:
            yield prefix, node
        for i, value in enumerate(node.value):
            yield from walk_yaml(value, prefix + f"[{i}]")
    else:
        yield prefix, node


def helm():
    path = REFERENCE / "charts/oberth/values.yaml"
    templates = sorted((REFERENCE / "charts/oberth/templates").glob("*"))
    references = collections.defaultdict(list)
    # Every literal .Values chain is indexed, including whole-object reads.
    # Whole-object reads consume descendants too; empty maps are public values.
    for template in templates:
        template_text = template.read_text()
        aliases = {}
        # Resolve direct and derived aliases to a fixed point. A range adds a
        # wildcard because its variable denotes one map/list element.
        declarations = re.findall(r"{{-?\s*(range\s+)?(?:\$\w+,\s*)?(\$\w+)\s*:?=\s*(.*?)-?}}", template_text)
        for _ in range(len(declarations)+1):
            changed = False
            for ranged, alias, expression in declarations:
                canonical = expression
                for name, value in sorted(aliases.items(), key=lambda pair: -len(pair[0])):
                    canonical = re.sub(re.escape(name) + r"\b", ".Values." + value, canonical)
                # (.Values.parent | default dict).child is an alias for
                # parent.child, rather than parent.
                canonical = re.sub(r"\((\.Values(?:\.[A-Za-z0-9_*\[\]-]+)+)\s*\|\s*default\s+dict\)\.", r"\1.", canonical)
                direct = re.match(r"\(?\s*(?:\$\.)?\.?Values((?:\.[A-Za-z0-9_*\[\]-]+)+)", canonical)
                get = re.match(r'\(?\s*get\s+\(?\s*\.Values((?:\.[A-Za-z0-9_*\[\]-]+)+)\s+"([a-zA-Z_-]+)"', canonical)
                value = direct[1][1:] if direct else get[1][1:] + "." + get[2] if get else None
                if value:
                    value += ".*" if ranged else ""
                    if aliases.get(alias) != value:
                        aliases[alias] = value
                        changed = True
            if not changed:
                break
        for line, text in enumerate(template_text.splitlines(), 1):
            for name, value in sorted(aliases.items(), key=lambda pair: -len(pair[0])):
                text = re.sub(re.escape(name) + r"\b", ".Values." + value, text)
            for m in re.finditer(r"(?:\$\.)?\.?Values((?:\.[A-Za-z0-9_*\[\]-]+)+)", text):
                references[m[1][1:]].append(f"{template.relative_to(REFERENCE)}:{line}")
            for m in re.finditer(r'(?:get|index|hasKey)\s+\(?\s*\.Values((?:\.[A-Za-z0-9_-]+)+)\s+"([a-zA-Z0-9_-]+)"', text):
                references[m[1][1:] + "." + m[2]].append(f"{template.relative_to(REFERENCE)}:{line}")
    values = {}
    for name, node in walk_yaml(yaml_nodes(path)[0]):
        values[name] = node
        plain = re.sub(r"\[\d+\]", "", name)
        readers = sorted({r for key, refs in references.items() if plain == key or plain.startswith(key + ".") or key.startswith(plain + ".") for r in refs})
        value = node.value if isinstance(node, yaml.ScalarNode) else ("{}" if isinstance(node, yaml.MappingNode) else "[]")
        reader_text = ", ".join(readers) if readers else "no literal template reader (not a retirement decision)"
        behavior = "Default " + compact(value) + "; readers " + reader_text
        add("Helm values", name, "C9", behavior, f"charts/oberth/values.yaml:{node.start_mark.line+1}")
    for name, refs in sorted(references.items()):
        if name in values or any(key.startswith(name + ".") or key.startswith(name + "[") for key in values):
            continue
        add("Helm template-only values", name, "C9", "Template reads an optional key absent from defaults", ", ".join(sorted(set(refs))))
    schema_path = REFERENCE / "charts/oberth/values.schema.json"
    # Include the schema's public map/object children even where templates
    # serialize a whole object instead of naming individual child keys.
    def schema_values(node, prefix=""):
        if not isinstance(node, yaml.MappingNode):
            return
        entries = {key.value: value for key, value in node.value}
        if "properties" in entries:
            for key, value in entries["properties"].value:
                name = prefix + ("." if prefix else "") + key.value
                yield name, value
                yield from schema_values(value, name)
        if "additionalProperties" in entries and isinstance(entries["additionalProperties"], yaml.MappingNode):
            yield from schema_values(entries["additionalProperties"], prefix + ".*")
        if "items" in entries:
            yield from schema_values(entries["items"], prefix + "[]")
    for name, node in schema_values(yaml_nodes(schema_path)[0]):
        if name in values or name in ROWS["Helm template-only values"] or any(key.startswith(name + ".") or key.startswith(name + "[") for key in values):
            continue
        fields = {key.value: value for key, value in node.value}
        contract = {key: yaml.safe_load(yaml.serialize(value)) for key, value in fields.items() if key not in {"properties", "items", "additionalProperties"}}
        add("Helm schema-only values", name, "C9", "Accepted optional schema value; constraints " + json.dumps(contract, sort_keys=True, separators=(",", ":")) + "; whole-object template consumers apply", f"charts/oberth/values.schema.json:{node.start_mark.line+1}")
    # Alias/with/range variable references cannot safely be assigned by grep.
    # Record every template expression as well, so no indirect read is omitted.
    for template in templates:
        text = template.read_text()
        for i, m in enumerate(re.finditer(r"{{-?([\s\S]*?)-?}}", text)):
            expression = compact(m[1])
            if not expression or expression.startswith("/*"):
                continue
            line = text[:m.start()].count("\n") + 1
            if "." in expression or "$" in expression:
                add("Helm template expressions", f"{template.name}:expression-{i+1}", "C9", "Evaluate " + expression, f"{template.relative_to(REFERENCE)}:{line}")


def store(facts):
    values = {f["Name"]: f for f in facts if f["Kind"] == "value" and f["File"].startswith("internal/store/") and f["Values"]}
    functions = {f["Name"]: f for f in facts if f["Kind"] == "function" and f["File"].startswith("internal/store/")}
    calls = collections.defaultdict(list)
    for f in facts:
        if f["Kind"] == "call" and f["File"].startswith("internal/store/"):
            calls[f["Name"]].append(f)
    def resolve(expr, evidence):
        value = literal(expr)
        if value is not None:
            return value, evidence
        if expr in values:
            return resolve(values[expr]["Values"][0], values[expr])
        embed = {"historicalMigrationVersion20": "v20.sql", "historicalMigrationVersion23": "v23.sql", "historicalMigrationVersion25": "v25.sql"}
        if expr in embed:
            path = "internal/store/historical-schema/" + embed[expr]
            return (REFERENCE / path).read_text(), {"File": path, "Line": 1}
        raise AssertionError(("unresolved migration SQL", expr, loc(evidence)))
    def from_function(name):
        assert name in functions, name
        for f in calls[name]:
            callee = f["Values"][0]
            if callee.endswith(".ExecContext") and len(f["Args"] or []) >= 2:
                expr = f["Args"][1]
                try:
                    sql, evidence = resolve(expr, f)
                except AssertionError:
                    continue  # Data-loop statements have no static DDL.
                if re.search(r"\b(?:CREATE|DROP|ALTER)\s+(?:TABLE|INDEX)\b", sql, re.I):
                    yield sql, evidence
            elif callee in functions and callee.startswith(("migrate", "v11", "v12")) and callee != name:
                yield from from_function(callee)
    catalog = values["legacyMigrations"]
    batches = []
    pattern = r"\b(sql|apply|rawApply)\s*:\s*(`[^`]*`|[A-Za-z_]\w*)"
    for match in re.finditer(pattern, catalog["Values"][0]):
        # The source-line offset of a composite value includes the declaration.
        evidence = {**catalog, "Line": catalog["Line"] + catalog["Text"][:catalog["Text"].find(match[0])].count("\n")}
        if match[1] == "sql":
            batches.append(resolve(match[2], evidence))
        else:
            batches.extend(from_function(match[2]))
    assert len(re.findall(pattern, catalog["Values"][0])) == 55
    batches.extend(from_function("ensureMigrationIDs"))
    for path in sorted((REFERENCE / "internal/store/migrations").glob("*.sql")):
        batches.append((path.read_text(), {"File": str(path.relative_to(REFERENCE)), "Line": 1}))
    db = sqlite3.connect(":memory:")
    history = {}
    final_evidence = {}
    removals = {}
    def snapshot():
        tables = {}
        for name, sql in db.execute("SELECT name,sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' UNION ALL SELECT name,sql FROM sqlite_temp_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").fetchall():
            tables[name] = (sql, {r[1]: r for r in db.execute('PRAGMA table_info("' + name + '")').fetchall()})
        return tables
    # The migration ledger is initialized before the catalog.
    ledger = values["schemaMigrationsSQL"] if "schemaMigrationsSQL" in values else None
    if ledger:
        batches.insert(0, resolve(ledger["Values"][0], ledger))
    else:
        text = (REFERENCE / "internal/store/schema.go").read_text()
        match = re.search(r"`\s*(CREATE TABLE IF NOT EXISTS schema_migrations[\s\S]*?)`", text)
        assert match
        batches.insert(0, (match[1], {"File": "internal/store/schema.go", "Line": text[:match.start(1)].count("\n")+1}))
    for sql, evidence in batches:
        # ExecContext accepts a final statement without a semicolon.
        sql += "\n;"
        statement, start = "", 0
        for pos, character in enumerate(sql):
            statement += character
            if character != ";" or not sqlite3.complete_statement(statement):
                continue
            clean = re.sub(r"--[^\n]*", "", statement).strip()
            leading = re.match(r"(?:\s|--[^\n]*(?:\n|$))*", statement)[0]
            line = evidence["Line"] + sql[:start].count("\n") + leading.count("\n")
            ev = f'{evidence["File"]}:{line}'
            if re.match(r"(?:CREATE\s+(?:(?:UNIQUE|TEMP|TEMPORARY)\s+)?(?:TABLE|INDEX)|DROP\s+(?:TABLE|INDEX)|ALTER\s+TABLE)\b", clean, re.I):
                before = snapshot()
                try:
                    db.execute(clean)
                except sqlite3.Error as error:
                    raise RuntimeError(f"DDL replay failed at {ev}: {clean[:100]}: {error}") from error
                after = snapshot()
                for table, (definition, columns) in after.items():
                    if table not in before or before[table][0] != definition:
                        final_evidence[table] = ev
                    history.setdefault((table, ""), ("Table with constraints " + compact(definition), ev))
                    for col, info in columns.items():
                        key = (table, col)
                        contract = f"{info[2]}; not_null={info[3]}; default={info[4]}; primary_key_position={info[5]}"
                        if table not in before or col not in before[table][1] or before[table][1][col] != info:
                            history[key] = (contract, ev)
                for table, (_, columns) in before.items():
                    if table not in after:
                        removals[(table, "")] = ev
                    for col in columns:
                        if table not in after or col not in after[table][1]:
                            removals[(table, col)] = ev
            statement, start = "", pos + 1
        # The hand-written migration functions also use no-semicolon SQL.
        if re.search(r"\b(?:CREATE|ALTER|DROP)\s+TABLE\b", statement, re.I):
            # Feed a terminated statement through the same parser.
            # Such fragments are normalized before the main replay below.
            raise AssertionError(("unterminated DDL", loc(evidence), statement[:80]))
    current = snapshot()
    for (table, column), (behavior, evidence) in sorted(history.items()):
        exists = table in current and (not column or column in current[table][1])
        disposition = "KEEP" if exists else "RETIRED: removed by " + removals[(table, column)] + "; preserve no historical schema in the baseline (REWRITE.md §2, W6)"
        name = table + ("." + column if column else "")
        if exists and not column:
            behavior = "Current table; " + compact(current[table][0])
            evidence = final_evidence[table]
        add("Store columns" if column else "Store tables", name, "C7", behavior, evidence, inspection_for("internal/store/schema.go", table), disposition)
    return len(current)


def metrics_events(facts):
    metrics = {}
    for f in facts:
        if f["Kind"] != "string" or not f["File"].startswith("internal/"):
            continue
        value = f["Values"][0]
        if "# TYPE oberth_" in value or "# HELP oberth_" in value or re.fullmatch(r"oberth_[a-z_]+", value) and f["File"].endswith("status_metrics.go"):
            for name in re.findall(r"oberth_[a-z_]+", value):
                if name == "oberth_token_use_":
                    continue
                metrics.setdefault(name, (f, value))
    token = next(f for f in facts if f["Kind"] == "function" and f["Name"] == "WriteTokenUseMetrics")
    for suffix in re.findall(r'\{"([a-z_]+)",', token["Text"]):
        metrics["oberth_token_use_" + suffix + "_total"] = (token, "counter; token telemetry " + suffix + "; no token identifiers")
    for name, (f, behavior) in sorted(metrics.items()):
        gauge_call = next((call for call in facts if call["Kind"] == "call" and call["Values"] == ["gauge"] and call["Args"] and literal(call["Args"][0]) == name), None)
        if gauge_call:
            behavior = "gauge; " + literal(gauge_call["Args"][1])
        if "# HELP" in behavior:
            match = re.search(r"# HELP " + re.escape(name) + r" ([^\n]+)", behavior)
            behavior = match[1] if match else "Sample " + name + "; " + behavior
        elif "# TYPE" in behavior:
            match = re.search(r"# TYPE " + re.escape(name) + r" (\w+)", behavior)
            behavior = (match[1] if match else "sample") + "; emitted by " + f["File"]
        add("Metrics", name, "C7" if "/store/" in f["File"] else "C6" if "/sourcedelivery/" in f["File"] else "C3", behavior, loc(f), inspection_for(f["File"], name))
    events = {}
    for f in facts:
        if f["Kind"] == "value" and f["Values"] and (f["Name"].startswith("AgentEvent") and f["File"] == "internal/model/agent_events.go" or f["Name"] in {"RunAssistExec", "RunAssistPatch", "RunAssistContinue", "RunAssistAbandon"}):
            value = literal(f["Values"][0])
            if value:
                events[value] = (f, "Durable agent event; cursor-ordered, scoped, deduplicated" if f["Name"].startswith("AgentEvent") else "Durable assisted-run action record")
    # Audit kinds come only from typed AuditActionSpec fields. Other DTOs have
    # unrelated Action fields, which must never become invented audit kinds.
    owned = [f for f in facts if not f["File"].startswith(("internal/storetest/", "internal/testdb/", "hack/"))]
    functions = {(str(Path(f["File"]).parent), f["Name"]): f for f in owned if f["Kind"] == "function"}
    assignments, incoming = collections.defaultdict(list), collections.defaultdict(list)
    constants = {(str(Path(f["File"]).parent), f["Name"]): f for f in owned if f["Kind"] == "value" and f["Values"]}
    for f in owned:
        directory = str(Path(f["File"]).parent)
        if f["Kind"] == "assignment":
            assignments[(directory, f["Name"], f["Args"][0])].append(f["Values"][0])
        if f["Kind"] == "call":
            name = f["Values"][0].split(".")[-1]
            incoming[(directory, name)].append(f)
    def resolve(expr, directory, fn, seen=frozenset()):
        expr = expr.strip()
        key = (directory, fn, expr)
        if key in seen:
            return set()
        seen = seen | {key}
        value = literal(expr)
        if value is not None:
            return {value}
        parts = re.split(r"\s*\+\s*", expr)
        if len(parts) > 1:
            result = {""}
            for part in parts:
                choices = resolve(part, directory, fn, seen)
                if not choices:
                    return set()
                result = {a+b for a in result for b in choices}
                assert len(result) < 2000, expr
            return result
        if expr.startswith("string(") and expr.endswith(")"):
            return resolve(expr[7:-1], directory, fn, seen)
        if (directory, expr) in constants:
            return resolve(constants[(directory, expr)]["Values"][0], directory, fn, seen)
        if re.fullmatch(r"(?:model|argoworkflow)\.\w+", expr):
            package, name = expr.split(".")
            target = "internal/model" if package == "model" else "pkg/argoworkflow"
            return resolve(name, target, fn, seen)
        result = set()
        for assignment in assignments[(directory, fn, expr)]:
            result.update(resolve(assignment, directory, fn, seen))
        function = functions.get((directory, fn), {})
        # A conjunction of != comparisons followed by refusal defines a closed
        # accepted enum (the promotion tag/prune settlement contracts).
        for guard in re.findall(r"if ([^\n{]+)\{\s*return [^\n]+ErrInvalid", function.get("Text", "")):
            if "&&" in guard:
                for compared in re.findall(re.escape(expr) + r'\s*!=\s*("[^"\n]*"|model\.\w+)', guard):
                    result.update(resolve(compared, directory, fn, seen))
        params = function.get("Args") or []
        if expr in params:
            index = params.index(expr)
            for call in incoming[(directory, fn)]:
                if index < len(call["Args"] or []):
                    result.update(resolve(call["Args"][index], directory, call["Name"], seen))
        # Functions returning fixed event spellings (e.g. promotion plan state).
        match = re.fullmatch(r"(?:\w+\.)?(\w+)\([^)]*\)", expr)
        if match and (directory, match[1]) in functions:
            text = functions[(directory, match[1])]["Text"]
            result.update(re.findall(r'return\s+"([a-z][a-z0-9_.-]*)"', text))
        return result
    for f in owned:
        if f["Kind"] != "audit":
            continue
        expr, directory = f["Values"][0], str(Path(f["File"]).parent)
        concrete = resolve(expr, directory, f["Name"])
        if f["Name"] == "appendPublicationAudit":
            # The abandoned status always arrives with an actor; that branch
            # explicitly changes its audit action to publication.abandon.
            concrete.discard("publication.abandoned")
        for value in sorted(concrete):
            if re.fullmatch(r"[a-z][a-z0-9_.-]*", value) and not value.endswith("."):
                events.setdefault("audit:" + value, (f, "Append-only audited action " + value + "; emitted from " + expr))
        # Preserve the exact emitter expression as an executable inspection
        # obligation even if an open-ended request/field prevents finite values.
        if literal(expr) is None:
            name = "audit expression:" + f["File"] + ":" + str(f["Line"])
            events[name] = (f, "Append-only audit emitter " + expr + "; resolved static variants are separate rows; field-dependent values require a differential inspection")
    for case in owned:
        if case["Kind"] == "case" and case["Name"] == "performAdministration":
            for expression in case["Values"] or []:
                name = literal(expression)
                if name:
                    for suffix in ["intent", "succeeded", "failed"]:
                        value = name + "." + suffix
                        events.setdefault("audit:" + value, (case, "Actor-bound administrator action; persists intent before execution and a bounded terminal outcome"))
    # The administrative lifecycle action is a bounded wire enum, not an
    # arbitrary string. These are exact accepted lifecycle kinds.
    registry = next(f for f in facts if f["Kind"] == "call" and f["Values"] == ["tool"] and f["Args"] and literal(f["Args"][0]) == "admin_audit_record")
    for value in re.findall(r'"((?:backup|restore|upgrade)\.completed)"', registry["Text"]):
        events.setdefault("audit:" + value, (registry, "Administrator records a completed lifecycle operation; request enum bounds the action"))
    for name, (f, behavior) in sorted(events.items()):
        owner = "C7" if f["File"].startswith("internal/store/") else "C11" if f["File"].startswith("cmd/") else "C3" if f["File"].startswith("internal/app/") else "C2"
        add("Event kinds", name, owner, behavior, loc(f), inspection_for(f["File"], name.removeprefix("audit:")))


def installer(facts):
    # Every function in the owned implementation is inventoried, including
    # unexported planning, validation, apply, recovery and rollback helpers.
    for f in facts:
        if f["Kind"] != "function" or not f["File"].startswith(("internal/installer/", "internal/setuptui/")):
            continue
        source = (REFERENCE / f["File"]).read_text().splitlines()
        comments = []
        i = f["Line"] - 2
        while i >= 0 and source[i].startswith("//"):
            comments.insert(0, source[i].removeprefix("//").strip())
            i -= 1
        description = compact(" ".join(comments))
        if not description:
            description = "Implementation action " + f["Name"] + "; " + compact(f["Text"].split("{", 1)[0])
        # Do not inflate the plan with multi-paragraph historical commentary.
        description = re.split(r"\. (?=[A-Z])", description)[0][:400]
        add("Installer and upgrade actions", f["File"] + ":" + f["Name"] + ":" + str(f["Line"]), "C9", description, loc(f), inspection_for(f["File"], f["Name"]))


def pipelines():
    files = run("git", "ls-files", ".oberth").splitlines()
    for name in files:
        path = REFERENCE / name
        if path.suffix in {".yaml", ".yml"}:
            for document, root in enumerate(yaml_nodes(path)):
                for key, node in walk_yaml(root):
                    value = node.value if isinstance(node, yaml.ScalarNode) else ("{}" if isinstance(node, yaml.MappingNode) else "[]")
                    value = compact(value)
                    if len(value) > 220:
                        value = value[:180] + "; full scalar sha256=" + hashlib.sha256(node.value.encode()).hexdigest()
                    add("Own pipeline configuration", name + (f":document-{document+1}" if document else "") + ":" + key, "C13", "Declared " + key + " = " + value, f"{name}:{node.start_mark.line+1}")
        else:
            data = path.read_text()
            digest = hashlib.sha256(data.encode()).hexdigest()
            add("Own pipeline support", name, "C13", "Tracked pipeline support artifact; sha256=" + digest, name + ":1")
            pattern = r"(?m)^(?:def\s+([A-Za-z_]\w*)\(|(?:function\s+)?([A-Za-z_]\w*)\s*\(\)\s*\{)"
            for m in re.finditer(pattern, data):
                function = m[1] or m[2]
                line = data[:m.start()].count("\n") + 1
                add("Own pipeline support", name + ":" + function, "C13", "Pipeline helper " + function + "; body at cited source", name + ":" + str(line))


def folded(rewrite):
    text = rewrite.read_text()
    section = text.split("## 4. Issue fold-in\n", 1)[1].split("## 5.", 1)[0]
    owners = {
        "1606": "C13", "1610": "C9", "1578": "C10", "1604": "C4", "1629": "C4", "1630": "C5", "1633": "C14", "1635": "C5", "1647": "C5", "1602": "C5", "1632": "C6", "1634": "C7", "1636": "C13", "1627": "C13", "1637": "C4", "1638": "C4", "1639": "C1", "1641": "C2", "1643": "C9", "1645": "C11", "1646": "C6", "1648": "C1", "1650": "C14", "1651": "C5", "1653": "C2", "1654": "C4", "1655": "C1", "1616": "C6",
    }
    count = 0
    for line in section.splitlines():
        if not line.startswith("| ") or line.startswith("| Issue ") or line.startswith("|---"):
            continue
        cells = [s.strip() for s in line.strip("|").split("|")]
        issue, title, decision = cells
        number = re.search(r"#(\d+)", issue)
        owner = owners.get(number[1] if number else "", "C13" if "LOW" in issue else "C4")
        disposition = "KEEP"
        if issue == "#1616":
            spec = ROOT / "docs/plans/c6-source-delivery.md"
            line = next(i for i, text in enumerate(spec.read_text().splitlines(), 1) if text == "### Native schedules")
            disposition = f"KEEP: source-consuming native schedules refused for v0.24.0; docs/plans/c6-source-delivery.md:{line}"
        add("Folded issues", issue, owner, title + "; baseline may precede the fix", f"REWRITE.md §4 ({issue})", "needs test", disposition)
        count += 1
    assert count == 30, count
    # Hash only the table cells that affect this inventory. Physical line
    # numbers and prose elsewhere in the coordinator's file are not inputs.
    cells = [line.strip() for line in section.splitlines()
             if line.startswith("| ") and not line.startswith(("| Issue ", "|---"))]
    return hashlib.sha256(json.dumps(cells, ensure_ascii=False, separators=(",", ":")).encode()).hexdigest()


def client_and_push_surfaces():
    # Fixed public names are verified against their exact owned declarations.
    for command in ("install", "connect", "add-repo", "unseal", "runs", "watch"):
        file = source_map()["client_paths"][command]
        source = (REFERENCE / file).read_text().splitlines()
        token = "/oberth:" + command
        line = next(i for i, text in enumerate(source, 1)
                    if token in text or text == "name: " + command)
        add("Agent client commands", token, "C12", "Public agent command; delegates installation, connection, onboarding, recovery or read-only history to the public CLI/API", f"{file}:{line}")
    file = "internal/gitcache/push_options.go"
    source = (REFERENCE / file).read_text().splitlines()
    for option in ("oberth.assist", "oberth.priority"):
        line = next(i for i, text in enumerate(source, 1) if option in text)
        behavior = ("Explicit assistance intent; reference accepts 1/0 and true/false" if option.endswith("assist")
                    else "Refused retired scheduling option; no priority authority")
        add("Git push options", option, "C6", behavior, f"{file}:{line}")
    # Page behavior is inventoried separately from the shell route registration.
    source = (REFERENCE / "internal/api/static/app.js").read_text().splitlines()
    for name, token, behavior in [
        ("run list and repository filters", "async function loadRuns(", "Scoped ordered run list, repository/ref filtering and pagination"),
        ("run detail, steps and log parts", "function failedStepLabel(", "Run verdict, failed step, attempt-bound output and assistance controls"),
        ("repository and issue navigation", "function repoLabel(", "Canonical repository labels, scoped navigation and issue links"),
        ("status and authentication", "function ensureStatus(", "Authenticated status, readiness and credential diagnostics"),
        ("safe output and browser state", "function esc(", "Escaped output, same-origin requests and bounded browser state")]:
        line = next(i for i, text in enumerate(source, 1) if token in text)
        add("UI views", name, "C1", behavior, f"internal/api/static/app.js:{line}")


def spec_deltas():
    """Capture the actual enclosing Delta clause, not just its bold marker."""
    result = {}
    for path in sorted((ROOT / "docs/plans").glob("c[0-9]*-*.md")):
        source = path.read_text()
        for match in re.finditer(r"\*\*([^*]*\bdelta\b[^*]*)\*\*", source, re.I):
            if re.match(r"\s+(?:marks|means|identifies|describes)\b", source[match.end():]):
                continue
            line = source[:match.start()].count("\n") + 1
            key = f"{path.relative_to(ROOT)}:{line}"
            prior_blank = source.rfind("\n\n", 0, match.start())
            start = prior_blank + 2 if prior_blank >= 0 else 0
            boundaries = list(re.finditer(r"(?m)^(?:- |\d+\. |\| )", source[start:match.start()]))
            if boundaries:
                start += boundaries[-1].start()
            line_start = source.rfind("\n", 0, match.start()) + 1
            prior_marker = list(re.finditer(r"\*\*[^*]*\bdelta\b[^*]*\*\*", source[start:match.start()], re.I))
            if prior_marker or source[line_start:match.start()].lstrip().startswith("|"):
                start = line_start
            end = source.find("\n\n", match.end())
            if end < 0:
                end = len(source)
            boundary = re.search(r"\n(?:- |\d+\. |\| )|\*\*[^*]*\bdelta\b", source[match.end():end], re.I)
            if boundary:
                end = match.end() + boundary.start()
            value = result.setdefault(key, {"markers": [], "text": ""})
            value["markers"].append(compact(match[0]))
            value["text"] = compact(source[start:end])
    return result


def selected_rows(selector):
    category = selector["category"]
    regex = re.compile(selector["name_regex"])
    matches = [(category, name) for name in sorted(ROWS[category]) if regex.search(name)]
    assert matches, ("empty row predicate", selector)
    return matches


def approve_new(category, name, approval):
    counts = {"runs.step_count", "runs.steps_planned_exact"} | {"runs.steps_" + status for status in ("pending", "queued", "running", "passed", "failed", "skipped", "unknown")}
    allowed = {
        "owner decision 53": category == "CLI commands" and name == "oberth promote",
        "owner-approved D6": category == "Event kinds" and name == "run.updated",
        "owner-approved D7": (category == "MCP tools" and name == "run_check"
                              or category == "MCP arguments" and name.startswith("run_check.")
                              or category == "CLI commands" and name == "oberth check"
                              or category == "CLI flags" and name in {"--repo [runCheck]", "--local [runCheck]", "--json [runCheck]"}),
        "owner-approved D9": category == "Git push options" and name == "oberth.run",
        "owner 2026-11-15 20:25, #1637": category == "Store columns" and name in counts,
        "folded #1643/#1644": category == "Client metadata" and name == "installation.json",
        "owner decisions 115 and 120": (
            category == "CLI commands" and name == "oberth issue import"
            or category == "CLI flags" and name == "--dry-run [importIssues]"
            or category == "MCP tools" and name == "issue_import"
            or category == "MCP arguments" and name in {"issue_import.jsonl", "issue_import.dry_run"}),
        "owner decision 126": (
            category == "CLI flags" and name == "--upstream [importIssues]"
            or category == "MCP arguments" and name == "issue_import.upstream"),
        "owner decision 114": category == "Agent client commands" and name in {"/oberth-runs", "/oberth-watch"},
        "owner decision 115": category == "CLI flags" and name in {
            "--pipeline-namespace [installationFlags]", "--ssh-node-port [installationFlags]", "--https-node-port [installationFlags]"},
    }
    assert approval in allowed and allowed[approval], ("addition outside approved scope", category, name, approval)


def dispositions(path):
    raw = path.read_text()
    guard_names(raw)
    data = json.loads(raw)
    assert data["version"] == 2, "unsupported disposition version"
    expected = spec_deltas()
    spec_paths = {source.rsplit(":", 1)[0] for source in expected}
    assert set(data["spec_sha256"]) == spec_paths, "spec hashes do not cover all marked specs"
    for file in sorted(spec_paths):
        assert data["spec_sha256"][file] == hashlib.sha256((ROOT / file).read_bytes()).hexdigest(), "spec changed; review Delta dispositions: " + file
    seen, assigned, reasons = set(), {}, collections.defaultdict(lambda: collections.defaultdict(list))
    repeated = collections.defaultdict(set)
    predicate_log, delta_counts = [], {}
    for delta in data["deltas"]:
        source = delta["spec"]
        assert source in expected and source not in seen, "stale/duplicate Delta: " + source
        assert delta["markers"] == expected[source]["markers"], "changed Delta marker: " + source
        seen.add(source)
        scope = delta["scope"]
        assert scope in {"implementation", "behaviour"}, (source, scope)
        basis = compact(delta["basis"])
        assert basis and basis in expected[source]["text"], "basis must quote this Delta: " + source
        expanded = []
        if scope == "implementation":
            assert delta["rows"] == [] and not delta.get("predicates"), "implementation Delta changes disposition: " + source
            for annotation in delta.get("annotations", []):
                matches = selected_rows(annotation)
                for cat, name in matches:
                    row = ROWS[cat][name]
                    item = "Rebuilt under " + source + ": " + basis
                    row[5] = "; ".join(dict.fromkeys(filter(None, [row[5], item])))
                predicate_log.append((source, "annotation", annotation["category"], annotation["name_regex"], len(matches)))
        else:
            expanded.extend((entry, (entry["category"], entry["name"]), False) for entry in delta["rows"])
            for predicate in delta.get("predicates", []):
                matches = selected_rows(predicate)
                predicate_log.append((source, "behaviour", predicate["category"], predicate["name_regex"], len(matches)))
                expanded.extend((predicate, key, True) for key in matches)
            assert expanded, "behaviour Delta has no real rows: " + source
        delta_counts[source] = {"scope": scope, "rows": len({key for _, key, _ in expanded}), "basis": basis}
        for entry, key, from_predicate in expanded:
            category, name = key
            kind = entry["disposition"]
            assert kind in {"KEEP", "CHANGE", "RETIRED", "NEW"}, "invalid disposition"
            quote = compact(entry.get("basis", ""))
            # A basis must quote the enclosing clause; unrelated spec citations
            # and generic wave text cannot license a row's behaviour change.
            terms = [name, name.split(" [", 1)[0], name.split(".", 1)[0]]
            assert any(term in expected[source]["text"] for term in terms) or quote and quote in expected[source]["text"], "row key absent from Delta and no quoted basis: " + source + " " + name
            reason = entry["reason"].replace("{name}", name)
            if not from_predicate:
                repeated[reason].add(key)
            if kind == "NEW":
                approve_new(category, name, entry["approval"])
                label = "NEW (" + entry["approval"] + ")"
            else:
                label = kind
            old = assigned.setdefault(key, label)
            assert old == label, ("conflicting spec dispositions", key, old, label)
            if name not in ROWS[category]:
                assert kind == "NEW", ("unknown source row", key)
                add(category, name, entry["owner"], entry["behavior"], source, entry.get("inspection", "needs test"), label)
            row = ROWS[category][name]
            if "owner" in entry:
                row[1] = entry["owner"]
            reasons[key][reason].append(source)
            row[3] = label + ": " + "; ".join(text + "; " + ", ".join(dict.fromkeys(sources)) for text, sources in reasons[key].items())
    assert seen == set(expected), "unmapped Delta: " + ", ".join(sorted(set(expected) - seen))
    assert all(len(keys) <= 25 for keys in repeated.values()), "reason shared by >25 rows without a declared predicate"
    return hashlib.sha256(path.read_bytes()).hexdigest(), delta_counts, predicate_log


def sweeps(path, source_dir):
    raw = path.read_text()
    guard_names(raw)
    data = json.loads(raw)
    assert data["version"] == 1
    headers, required = {}, set()
    for source, filename in data["sources"].items():
        text = (source_dir / filename).read_text()
        for match in re.finditer(r"(?m)^([FSDAG]\d+(?:\.\d+)?) (DELETE candidate|KEEP(?: with reason)?|NEEDS-SPEC)[^\n]*", text):
            key = (source, match[1])
            headers[key] = (filename, text[:match.start()].count("\n") + 1)
            if match[2] == "DELETE candidate":
                required.add(key)
    required.add(("store", "F09"))  # Main ruling 3 resolves old cleanup custody.
    seen, log = set(), []
    for finding in data["findings"]:
        key = (finding["source"], finding["id"])
        assert key in headers and key not in seen, ("unknown/duplicate sweep finding", key)
        seen.add(key)
        kind = finding["disposition"]
        assert kind in {"KEEP", "RETIRED"}
        assert kind != "KEEP" or finding.get("justification"), "DELETE candidate remains KEEP without justification: " + str(key)
        source_file, line = headers[key]
        evidence = source_file + ":" + str(line)
        identity = finding["source"] + ":" + finding["id"]
        keys = [(entry["category"], entry["name"]) for entry in finding.get("rows", [])]
        for predicate in finding.get("predicates", []):
            matches = selected_rows(predicate)
            keys.extend(matches)
            log.append((identity, kind, predicate["category"], predicate["name_regex"], len(matches)))
        if not keys:
            name = identity
            add("Sweep deletions (no public surface)", name, finding["owner"], finding["subject"], evidence,
                "P5 absence guard: " + ", ".join(finding["evidence"]), kind)
            keys = [("Sweep deletions (no public surface)", name)]
        for category, name in keys:
            assert name in ROWS[category], ("unknown sweep row", category, name)
            row = ROWS[category][name]
            reason = identity + ": " + finding["reason"] + "; " + evidence
            if kind == "KEEP":
                reason += "; justification: " + finding["justification"]
            # Sweep retirement refines CHANGE/KEEP; never resurrect a retirement.
            assert not (kind == "KEEP" and row[3].startswith("RETIRED")), ("sweep contradicts spec retirement", identity, name)
            if row[3].startswith(kind) and row[3] != kind:
                row[3] += "; " + reason
            else:
                row[3] = kind + ": " + reason
    assert required <= seen, "unmapped DELETE candidates: " + ", ".join(":".join(key) for key in sorted(required-seen))
    return hashlib.sha256(path.read_bytes()).hexdigest(), len(seen), log


def requirement_contracts(data, specs=None):
    """Check the compact normative register, owning citations and inspections."""
    fields = {"version", "requirements", "retired_ids", "component_limits"}
    assert {"version", "requirements"} <= set(data) <= fields and data["version"] == 1, ("unsupported requirement schema", sorted(set(data) - fields))
    assert len(json.dumps(data, ensure_ascii=False).encode()) < 100000, "requirement register exceeds compact budget"
    if specs is None:
        specs = {str(p.relative_to(ROOT)): p.read_text() for p in sorted((ROOT / "docs/plans").glob("c[0-9]*-*.md"))}
    by_owner = {}
    for path in specs:
        match = re.fullmatch(r"docs/plans/c(\d+)-[^/]+\.md", path)
        assert match, ("invalid requirement spec", path)
        owner = "C" + match[1]
        assert owner not in by_owner, ("duplicate component spec", owner)
        by_owner[owner] = path
    requirements = {}
    fields = {"id", "component", "class", "requirement", "loss_if_absent", "inspection"}
    for entry in data["requirements"]:
        assert set(entry) == fields, ("requirement non-normative fields", sorted(set(entry) ^ fields))
        rid, owner = entry["id"], entry["component"]
        assert re.fullmatch(r"REQ-C(?:[1-9]|1[0-4])-\d{3}", rid) and rid.startswith("REQ-" + owner + "-"), ("invalid requirement identity", rid)
        assert rid not in requirements, ("duplicate requirement", rid)
        assert owner in by_owner, ("missing requirement owner", rid)
        assert entry["class"] in {"security", "correctness", "ux"}, ("invalid requirement class", rid)
        for key in ("requirement", "loss_if_absent"):
            assert entry[key].strip() and "\n" not in entry[key], ("empty or multiline requirement", rid)
        inspection = entry["inspection"]
        assert set(inspection) == {"kind", "name"}, ("inspection fields", rid, sorted(set(inspection) ^ {"kind", "name"}))
        assert inspection["kind"] in {"contract_test", "recorded_parity", "walk_step"} and inspection["name"].strip(), ("requirement inspection missing", rid)
        assert inspection["kind"] != "contract_test" or re.fullmatch(r"Test(?:[A-Z0-9_]\w*)?", inspection["name"]), ("inspection.name", rid, inspection["name"])
        assert entry["class"] != "ux" or inspection["kind"] == "walk_step", ("UX requirement needs a walk", rid)
        requirements[rid] = entry
    counts = collections.Counter(e["component"] for e in requirements.values())
    limits = data.get("component_limits", {})
    assert set(limits) <= {"C11"} and all(type(limit) is int and limit > 0 for limit in limits.values()), "unsupported component requirement limit"
    excess = {owner: count for owner, count in counts.items() if count > limits.get(owner, 10)}
    assert not excess, ("component requirement limit exceeded", excess)
    cited = collections.defaultdict(set)
    for path, source in specs.items():
        for rid in re.findall(r"\bREQ-C\d+-\d+\b", source):
            assert rid in requirements, ("unknown cited requirement", path, rid)
            cited[rid].add(path)
    for rid, entry in requirements.items():
        expected = by_owner[entry["component"]]
        assert cited[rid] == {expected}, ("requirement must cite exactly its owning spec", rid, sorted(cited[rid]))
        source = specs[expected]
        invariants = re.search(r"(?ms)^## Invariants[^\n]*\n(.*?)(?=^## |\Z)", source)
        inspections = re.search(r"(?ms)^## Inspections\n(.*?)(?=^## |\Z)", source)
        assert invariants, ("requirement invariants missing", rid)
        assert inspections and rid in inspections[1] and entry["inspection"]["name"] in inspections[1], ("requirement inspection absent from spec", rid)
    retired = data.get("retired_ids", {})
    assert not set(retired) & set(requirements), "retired requirement id reused"
    assert all(re.fullmatch(r"REQ-C(?:[1-9]|1[0-4])-\d{3}", rid) and reason.strip() for rid, reason in retired.items()), "invalid retired requirement"
    return {"requirements": len(requirements), "owning_specs": len(counts),
            "by_component_class": dict(sorted(collections.Counter(e["component"] + ":" + e["class"] for e in requirements.values()).items()))}


def render_requirements(data, digest):
    lines = ["# Requirements", "", "Generated from `requirements.json`; SHA256 " + digest + ".", "",
             "These are normative requirements. Inspections are planned; this register does not claim they have passed.", ""]
    for owner in sorted({e["component"] for e in data["requirements"]}, key=lambda o: int(o[1:])):
        lines.extend(["## " + owner, ""])
        for entry in data["requirements"]:
            if entry["component"] != owner:
                continue
            inspection = entry["inspection"]
            lines.extend(["### " + entry["id"], "",
                          "**" + entry["class"].capitalize() + ".** " + entry["requirement"] + " " + entry["loss_if_absent"] +
                          " Inspection to write: `" + inspection["name"] + "` (" + inspection["kind"] + ").", ""])
    if data.get("retired_ids"):
        lines.extend(["## Retired ids", "", "These ids are never reused.", "", "| Id | Where it went |", "| --- | --- |"])
        for rid, reason in sorted(data["retired_ids"].items(), key=lambda item: (int(item[0].split("-")[1][1:]), item[0])):
            lines.append("| " + rid + " | " + reason + " |")
        lines.append("")
    return "\n".join(lines)


def dependency_matches(path, pattern):
    if pattern.endswith("/**"):
        return path == pattern[:-3] or path.startswith(pattern[:-2])
    return fnmatch.fnmatchcase(path, pattern)


def dependency_patterns(data, pattern):
    if pattern == "@shared":
        return data["shared"]
    if pattern.startswith("@components"):
        excluded = pattern.split(":except:")[1] if ":except:" in pattern else None
        return [path for owner, paths in data["components"].items() if owner != excluded for path in paths]
    if pattern.startswith("@C"):
        return data["components"][pattern[1:]]
    return [pattern]


def intended_package_paths(data, reference_directories):
    paths = set(reference_directories) | set(data["packages"])
    for key in ("allow", "package_allow", "shared_dependencies", "test_allow"):
        if key != "allow":
            paths.update(data[key])
        for targets in data[key].values():
            paths.update(targets)
    return paths


def test_support_parent(data, path):
    parent, _, name = path.rpartition("/")
    parent = parent or "."
    production = {target for field in ("allow", "package_allow", "shared_dependencies") for targets in data[field].values() for target in targets}
    # Explicit production targets are ordinary packages; source checks prove suffix helpers.
    if name not in {"fake", "contracttest"} and path in production:
        return None
    if parent and (name in {"fake", "contracttest"} or name.endswith("test")):
        if parent in data["packages"] or any(dependency_matches(parent, p) for paths in data["components"].values() for p in paths) or test_support_parent(data, parent):
            return parent
    return None


def symbol_permitted(data, caller, target, symbol, test=False):
    if test:
        return True
    expand = functools.partial(dependency_patterns, data)
    for rule in data["rules"]:
        if rule["effect"] != "allow_if_listed" or not rule.get("symbols"):
            continue
        if not any(dependency_matches(caller, p) for item in rule["from"] for p in expand(item)):
            continue
        if not any(dependency_matches(target, p) for item in rule["to"] for p in expand(item)):
            continue
        owner = next((c for c, paths in data["components"].items() if any(dependency_matches(caller, p) for p in paths)), "")
        allowed = rule["symbols"].get("*", []) + rule["symbols"].get(owner, [])
        if symbol not in allowed and Path(target).name + "." + symbol not in allowed:
            return False
    return True


def intended_dependency_graph(data, packages):
    """Expand production permissions, never test or fixture exceptions."""
    matches = dependency_matches
    expand = functools.partial(dependency_patterns, data)
    nodes = {
        path for path in intended_package_paths(data, packages)
        if not test_support_parent(data, path)
        and not any(matches(path, tree) for tree in data.get("harness", {}).get("trees", []))
        and data["packages"].get(path, {}).get("kind") != "test_only"
        and not any(matches(path, pattern) for pattern in data["retired"])
        and not any(matches(path, pattern) for fixture in data["fixture_only"] for pattern in fixture["from"])
    }
    graph = {}
    for source in sorted(nodes):
        if source in data["shared"]:
            allowed = data["shared_dependencies"][source]
        else:
            owners = [owner for owner, patterns in data["components"].items() if any(matches(source, p) for p in patterns)]
            assert len(owners) == 1, ("unowned/multiply-owned graph package", source, owners)
            allowed = data["allow"][owners[0]]
        allowed = data["package_allow"].get(source, allowed)
        targets = []
        for target in sorted((set(allowed) & nodes) - {source}):
            denied = any(
                rule["effect"] == "deny"
                and any(matches(source, p) for item in rule["from"] for p in expand(item))
                and any(matches(target, p) for item in rule["to"] for p in expand(item))
                for rule in data["rules"]
            )
            if not denied:
                targets.append(target)
        graph[source] = targets
    return graph


def dependency_cycles(graph):
    """Return every cyclic strongly connected component in stable order."""
    indices, lowlinks = {}, {}
    stack, active, cycles = [], set(), []

    def visit(node):
        indices[node] = lowlinks[node] = len(indices)
        stack.append(node)
        active.add(node)
        for target in sorted(graph[node]):
            if target not in indices:
                visit(target)
                lowlinks[node] = min(lowlinks[node], lowlinks[target])
            elif target in active:
                lowlinks[node] = min(lowlinks[node], indices[target])
        if lowlinks[node] == indices[node]:
            component = []
            while True:
                target = stack.pop()
                active.remove(target)
                component.append(target)
                if target == node:
                    break
            if len(component) > 1 or node in graph[node]:
                cycles.append(sorted(component))

    for node in sorted(graph):
        if node not in indices:
            visit(node)
    return sorted(cycles)


def dependency_contracts(reference_directories=None):
    """Check intended JSON permissions and ownership without building packages."""
    data = json.loads((ROOT / "docs/plans/dependencies.json").read_text())
    runpy.run_path(str(Path(__file__).with_name("policy.py")))["dependencies"](data)
    for collection in ("allow", "package_allow", "shared_dependencies"):
        for targets in data[collection].values():
            assert not any(test_support_parent(data, target) for target in targets), "production permission to test support"
            assert not set(targets) & {p for p, r in data["packages"].items() if r["kind"] == "test_only"}, "production permission to test support"
    sealing = [r for r in data["rules"] if r["effect"] == "allow_if_listed" and r["to"] == ["internal/admissioncontract"]]
    assert len(sealing) == 1 and "Seal" not in sealing[0]["symbols"]["*"], "sealing constructor caller policy"
    assert {"PreparedAdmission", "PreparedView"} <= set(sealing[0]["symbols"]["*"])
    assert sealing[0]["from"] == ["@components:except:C4", "@shared", "pkg/argoworkflow/defaults/**", "pkg/argoworkflow/specsize/**"]
    table = data["packages"]
    assert all(record["owner"] in data["components"] for record in table.values()), "unknown package owner"
    assert all(record["kind"] in {"shared", "runtime", "test_only"} for record in table.values()), "unknown package kind"
    assert "shared_allow" not in data, "blanket shared allowlist is forbidden"
    for component in data["components"]:
        expected = sorted(name for name, record in table.items() if record["kind"] == "shared" and component in record["consumers"])
        assert expected == data["shared_per_component"][component], ("shared consumer mismatch", component)
        allowed = data["allow"][component]
        assert set(expected) <= set(allowed), ("missing allowed shared consumer", component)
        for target in allowed:
            if target in table:
                assert component in table[target]["consumers"], ("unlisted package consumer", component, target)
    for caller, targets in data["shared_dependencies"].items():
        for target in targets:
            assert target in data["shared"] and (table[caller]["kind"] != "shared" or table[target]["kind"] == "shared"), ("shared imports runtime", caller, target)
            assert table[caller]["owner"] in table[target]["consumers"], ("shared helper bypasses consumers", caller, target)
    expand = functools.partial(dependency_patterns, data)
    matches = dependency_matches
    for caller, targets in data["package_allow"].items():
        owners = [owner for owner, patterns in data["components"].items() if any(matches(caller, p) for p in patterns)]
        if caller in table and table[caller]["kind"] == "shared":
            assert set(targets) <= set(data["shared_dependencies"][caller]), ("package override broadens a shared helper", caller)
        else:
            assert len(owners) == 1, ("unowned package override", caller)
            assert set(targets) <= set(data["allow"][owners[0]]), ("package override bypasses consumer permissions", caller)
    conflicts = []
    for component, targets in data["allow"].items():
        for srcpattern in data["components"][component]:
            source = srcpattern.removesuffix("/**")
            effective = data["shared_dependencies"][source] if source in data["shared"] else data["package_allow"].get(source, targets)
            for target in effective:
                for rule in data["rules"]:
                    if rule["effect"] == "deny" and any(matches(source, p) for item in rule["from"] for p in expand(item)) and any(matches(target, p) for item in rule["to"] for p in expand(item)):
                        conflicts.append((component, target, rule["id"]))
    assert not conflicts, ("allowed/denied dependency conflicts", conflicts)
    directories = sorted(reference_directories or data["reference_packages"])
    unowned, retired, multiple = [], [], []
    intended = intended_package_paths(data, directories)
    for directory in sorted(intended):
        if test_support_parent(data, directory):
            continue
        if any(matches(directory, tree) for tree in data.get("harness", {}).get("trees", [])):
            continue
        if any(matches(directory, p) for p in data["retired"]):
            if directory in directories:
                retired.append(directory)
            continue
        owners = {owner for owner, patterns in data["components"].items() if any(matches(directory, pattern) for pattern in patterns)}
        if directory in table:
            owners.add(table[directory]["owner"])
        if not owners:
            unowned.append(directory)
        elif len(owners) != 1:
            multiple.append((directory, sorted(owners)))
    assert not unowned, ("unowned package directories", unowned)
    assert not multiple, ("packages have multiple owners", multiple)
    assert {name for name, record in table.items() if record["kind"] == "shared"} <= set(data["shared"]), "shared classification mismatch"
    assert all(name in table and table[name]["kind"] in {"shared", "runtime"} for name in data["shared"]), "unknown pinned contract package"
    assert set(data["shared_dependencies"]) == set(data["shared"]), "pinned contract dependency mismatch"
    graph = intended_dependency_graph(data, intended)
    cycles = dependency_cycles(graph)
    assert not cycles, ("intended dependency permission cycles", cycles)
    return {"reference_directories": len(directories), "intended_packages_audited": len(intended), "unowned": unowned, "multiple_owners": multiple, "retired": retired, "consumer_permissions": "JSON ownership and consumers checked", "allow_deny_conflicts": conflicts, "pure_shared_packages": len(data["shared"]), "production_graph_packages": len(graph), "production_graph_edges": sum(map(len, graph.values())), "cyclic_components": cycles}



def render(rewrite_digest, current_tables, disposition_digest, delta_counts, sweep_digest, sweep_count, predicates):
    lines = ["# Executable parity matrix", "",
             "Source evidence pin: 5e1200a2. Release oracle: the released v0.23.4 tag once published. No implementation or qualification claim.", "",
             "Regenerate: python3 hack/plans/parity.py --reference /path/to/old-checkout --rewrite /path/to/REWRITE.md. Portable self-check: python3 hack/plans/parity.py --check.",
             "Only extracted REWRITE.md §4 issue-table cells are hashed; unrelated prose/line numbers are not inputs. SHA256 " + rewrite_digest + ".",
             f"Spec input: docs/plans/dispositions.json (override --dispositions); SHA256 {disposition_digest}; {len(delta_counts)} marked Delta locations.",
             f"Sweep input: docs/plans/sweeps.json (override --sweeps, --sweep-dir); SHA256 {sweep_digest}; {sweep_count} findings.", "",
             "KEEP preserves observable behaviour even when implementation is rebuilt. Implementation Deltas only annotate rows. CHANGE names a specific observable difference and cites its actual Delta. RETIRED cites a spec or sweep absence decision. NEW names its approval state. No generic wave sentence licenses a behaviour change.",
             "The source registry has 92 tools; additions/retirements are explicit. All tracked source registrations, chart expressions, schema history, metrics/events, installer functions and own-pipeline declarations remain inventoried. VM product-bearing identifiers use neutral aliases; exact safe source locations remain evidence.",
             f"Schema replay finds {current_tables} current reference tables. No live database is read. Inspection names are candidates, not tests run by this docs pass; rewrite status remains missing until the behaviour passes its differential inspection.", "",
             "| Category | Rows |", "| --- | ---: |"]
    for category, rows in ROWS.items():
        lines.append(f"| {category} | {len(rows)} |")
    lines.insert(8, "Dependency input: docs/plans/dependencies.json; SHA256 " + hashlib.sha256((ROOT / "docs/plans/dependencies.json").read_bytes()).hexdigest() + ". JSON consumer permissions, recorded reference ownership and intended package acyclicity are checked on every run.")
    lines.extend([f"| TOTAL | {sum(map(len, ROWS.values()))} |", "", "| Disposition | Rows |", "| --- | ---: |"])
    counts = collections.Counter(disposition_label(row[3]) for rows in ROWS.values() for row in rows.values())
    lines.extend(f"| {name} | {count} |" for name, count in sorted(counts.items()))
    lines.extend(["", "## Declared row predicates", "", "| Delta / finding | Scope | Category | Name regex | Selected rows |", "| --- | --- | --- | --- | ---: |"])
    for predicate in predicates:
        lines.append("| " + " | ".join(map(cell, predicate)) + " |")
    lines.extend(["", "## Per-Delta coverage", "", "| Spec Delta | Scope | Behaviour rows | Implementation annotation / quoted basis |", "| --- | --- | ---: | --- |"])
    for source, value in delta_counts.items():
        lines.append(f"| {source} | {value['scope']} | {value['rows']} | {cell(value['basis'])} |")
    lines.append("")
    for category, rows in ROWS.items():
        lines.extend(["## " + category, "", "| Name | Owning component | Current behaviour (source evidence) | Parity disposition | Inspection | Implementation annotation |", "| --- | --- | --- | --- | --- | --- |"])
        for _, row in sorted(rows.items()):
            values = row[:5] + [row[5] if disposition_label(row[3]) != "RETIRED" else ""]
            lines.append("| " + " | ".join(cell(neutral_vm(value)) for value in values) + " |")
        lines.append("")
    text = "\n".join(lines)
    guard_names(text)
    return text


def regenerate_inventory(options):
    """Produce the inventory from the pinned code, retaining derived dispositions."""
    global REFERENCE, yaml, TESTS
    import yaml
    REFERENCE = options.reference.resolve()
    ROWS.clear()
    inspection_for.cache_clear()
    assert source_map()["client_paths"], "Reference regeneration requires an external source map."
    for folder in ["cmd", "internal", "pkg", "charts/oberth", ".oberth", "hack", *source_map()["source_folders"]]:
        assert not run("git", "diff", BASE, "--", folder, ":(exclude)hack/plans/**"), "reference sources changed: " + folder
    TESTS = test_index()
    facts, tools = source_facts()
    cli(facts)
    mcp(facts, tools)
    http(facts)
    helm()
    current = store(facts)
    metrics_events(facts)
    installer(facts)
    pipelines()
    digest = folded(options.rewrite)
    client_and_push_surfaces()
    disposition_digest, delta_counts, predicates = dispositions(options.dispositions)
    sweep_digest, sweep_count, sweep_predicates = sweeps(options.sweeps, options.sweep_dir or options.rewrite.parent / "trains/out")
    text = render(digest, current, disposition_digest, delta_counts, sweep_digest,
                  sweep_count, predicates + sweep_predicates)
    return text, current, disposition_digest, delta_counts, sweep_count


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--rewrite", type=Path)
    parser.add_argument("--reference", type=Path)
    parser.add_argument("--source-map", type=Path, help="External reference paths and aliases; also OBERTH_PARITY_SOURCE_MAP.")
    parser.add_argument("--check", action="store_true")
    parser.add_argument("--dispositions", type=Path, default=ROOT / "docs/plans/dispositions.json")
    parser.add_argument("--sweeps", type=Path, default=ROOT / "docs/plans/sweeps.json")
    parser.add_argument("--sweep-dir", type=Path)
    options = parser.parse_args()
    if options.source_map:
        os.environ["OBERTH_PARITY_SOURCE_MAP"] = str(options.source_map.resolve())
    if options.reference is None:
        assert options.check, "regeneration requires --reference <old checkout> and --rewrite <REWRITE.md>"
        from portable import check
        print(json.dumps(check(globals()), indent=2))
        return
    assert options.rewrite, "reference regeneration requires --rewrite"
    dependency_report = dependency_contracts()
    requirements_path = ROOT / "docs/plans/requirements.json"
    requirements_data = json.loads(requirements_path.read_text())
    requirement_report = requirement_contracts(requirements_data)
    requirements_text = render_requirements(requirements_data, hashlib.sha256(requirements_path.read_bytes()).hexdigest())
    text, current, disposition_digest, delta_counts, sweep_count = regenerate_inventory(options)
    target = ROOT / "docs/plans/PARITY.md"
    requirements_target = ROOT / "docs/plans/REQUIREMENTS.md"
    if options.check:
        guard_names(target.read_text())
        assert target.read_text() == text, "PARITY.md differs; regenerate"
        assert requirements_target.read_text() == requirements_text, "REQUIREMENTS.md differs; regenerate"
    else:
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(text)
        requirements_target.write_text(requirements_text)
    print(json.dumps({"categories": {k: len(v) for k, v in ROWS.items()}, "total": sum(map(len, ROWS.values())), "current_tables": current, "spec_deltas": len(delta_counts), "sweep_findings": sweep_count, "dependency_coverage": dependency_report, "requirement_coverage": requirement_report, "per_delta": delta_counts, "dispositions": dict(sorted(collections.Counter(disposition_label(row[3]) for rows in ROWS.values() for row in rows.values()).items()))}, indent=2))


if __name__ == "__main__":
    try:
        main()
    except (AssertionError, KeyError, ValueError, TypeError, OSError, AttributeError, IndexError) as error:
        raise SystemExit("POLICY: " + " ".join(str(error).split()))
