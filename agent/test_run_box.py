"""Bind the run box's executable, profile default, CLI calls and JSON projection to public contracts."""
import json
from pathlib import Path
import re
import unittest

from test_guidance import check_command, command_table


ROOT = Path(__file__).resolve().parent.parent
AGENT = ROOT / "agent"


def struct_fields(file, name):
    text = (ROOT / file).read_text().split("type " + name + " struct {", 1)[1].split("\n}", 1)[0]
    return set(re.findall(r'json:"([^",]+)', text))


def check_calls(client, commands):
    calls = re.findall(r"json\(\$, options, \[([^\]]+)\]\)", client)
    if len(calls) != 2:
        raise ValueError("expected the list and single-run JSON calls")
    for call in calls:
        call = call.replace("String(count)", "'3'")
        call = re.sub(r"\bid\b", "'cad7d9ec'", call)
        args = re.findall(r"'([^']+)'", call)
        check_command(" ".join(args + ["--json"]), commands)
    flags = set(re.findall(r"'(--[a-z-]+)'", re.sub(r"const BINDING = \[[^\]]+\]", "", client)))
    if flags != {"--limit", "--json"}:
        raise ValueError("unchecked CLI flags")
    envelope = re.search(r"as \{ (\w+)\?:", client).group(1)
    if envelope not in struct_fields("internal/apicontract/results.go", "RunPage"):
        raise ValueError("unknown run-list JSON field")
    if client.count("$.process.run(") != 2 or "[await checkedExecutable($, options), ...args, '--json']" not in client:
        raise ValueError("all execution must use the configured absolute executable")
    check_defaults(client)


def check_defaults(client):
    """The unset executable and profile are the fetcher's install and the bridge's checkout binding."""
    if "$.process.run(BINDING, " not in client:
        raise ValueError("the only other process is the checkout binding read")
    binding = re.search(r"const BINDING = \[([^\]]+)\]", client).group(1)
    key = re.findall(r"'([^']+)'", binding)
    if key[:4] != ["git", "config", "--local", "--get"] or len(key) != 5:
        raise ValueError("the binding read must be one local git config key")
    for source in ("cmd/oberth/join_checkout.go", "cmd/oberth/mcp_profile.go"):
        if '"' + key[4] + '"' not in (ROOT / source).read_text():
            raise ValueError("binding key differs from the one join writes and the bridge reads")
    installed = re.search(r"const INSTALLED = '([^']+)'", client).group(1)
    fetcher = (AGENT / "skills/oberth/fetch-oberth").read_text()
    directory, name = installed.rsplit("/", 1)
    if "bin=$HOME" + directory + "\n" not in fetcher or 'at %s/' + name + ' (signature verified)' not in fetcher:
        raise ValueError("default executable differs from the fetcher's install")


def projection_fields(contract, name):
    body = contract.split("export type " + name + " = {", 1)[1].split("\n}", 1)[0]
    return body, set(re.findall(r"^  (\w+)\??:", body, re.M))


def check_projection(contract):
    body, fields = projection_fields(contract, "Run")
    public = struct_fields("internal/stepmodel/run.go", "RunSummary")
    public |= struct_fields("internal/stepmodel/step.go", "LogicalStepCounts")
    if fields - public:
        raise ValueError("unknown RunSummary JSON field: " + str(fields - public))
    _, failure = projection_fields(contract, "Failure")
    if failure != {"failed_step", "failed_step_name"} or failure - struct_fields("internal/stepmodel/step.go", "FailureRecord"):
        raise ValueError("unknown failed plan step JSON field")
    status = re.search(r"status: (.+)", body).group(1)
    enums = (ROOT / "internal/stepmodel/enums.go").read_text()
    if set(re.findall(r"'([^']+)'", status)) != set(re.findall(r'Run\w+\s+RunStatus\s+=\s+"([^"]+)"', enums)):
        raise ValueError("run status contract drift")
    if "runs" not in struct_fields("internal/apicontract/results.go", "RunPage"):
        raise ValueError("missing run list envelope")
    results = (ROOT / "internal/apicontract/results.go").read_text()
    record = results.split("type RunRecord struct {", 1)[1].split("\n}", 1)[0]
    page = results.split("type RunPage struct {", 1)[1].split("\n}", 1)[0]
    if not re.search(r"^\tstepmodel\.RunSummary$", record, re.M) or not re.search(r"^\tRuns\s+\[\]RunRecord\s", page, re.M):
        raise ValueError("run record no longer carries the run projection fields")
    catalogue = (ROOT / "internal/apicontract/tools_runs.go").read_text()
    for tool, result in (("run_get", "RunRecord"), ("run_list", "RunPage")):
        if not re.search(re.escape(result) + r'[^\n]+?tool:"' + tool + r'"', catalogue):
            raise ValueError("CLI command result contract drift")


def check_paths(source):
    if re.search(r"token|ca.file|/home/|/Users/|~[/\\]|[A-Za-z]:\\Users\\|\.config/|process\.env|mcp.*call|run_list|run_get", source, re.I):
        raise ValueError("agent module must not name credentials, home paths or private routes")


class RunBoxContract(unittest.TestCase):
    def test_commands_flags_and_output_contracts(self):
        client = (AGENT / "hooks/client.ts").read_text()
        contract = (AGENT / "types/index.d.ts").read_text()
        check_calls(client, command_table())
        check_projection(contract)

    def test_renamed_commands_flags_and_fields_are_refused(self):
        client = (AGENT / "hooks/client.ts").read_text()
        for old, new in (("'list'", "'missing'"), ("'get'", "'missing'"), ("'--limit'", "'--missing'"), ("'--json'", "'--missing'")):
            with self.subTest(old=old), self.assertRaises(ValueError):
                check_calls(client.replace(old, new), command_table())
        with self.assertRaises(ValueError):
            check_calls(client.replace("runs", "missing"), command_table())
        for old, new in (("'oberth.uplinkProfile'", "'oberth.profileName'"), ("'--local', ", ""),
                         ("'/.local/bin/oberth'", "'/bin/oberth'"), ("$.process.run(BINDING, ", "$.process.run(['git'], ")):
            with self.subTest(old=old), self.assertRaises(ValueError):
                check_calls(client.replace(old, new), command_table())
        contract = (AGENT / "types/index.d.ts").read_text()
        with self.assertRaises(ValueError):
            check_projection(contract.replace("failure?:", "missing_field?:"))
        for field in ("step_count", "steps_planned_exact", "finished_at", "status", "failed_step", "failed_step_name"):
            with self.subTest(field=field), self.assertRaises((ValueError, AttributeError)):
                check_projection(contract.replace(field + ":", "missing_field:"))

    def test_no_credentials_home_paths_or_old_routes(self):
        sources = [p for p in (AGENT / "hooks").glob("*.ts*") if ".test." not in p.name]
        sources += [AGENT / "types/index.d.ts", AGENT / ".claude-plugin/plugin.json"]
        for path in sources:
            check_paths(path.read_text())
        for forbidden in ("--token-file", "--ca-file", "/home/person", "~/config", "mcp call run_get"):
            with self.subTest(forbidden=forbidden), self.assertRaises(ValueError):
                check_paths(forbidden)

    def test_shared_skill_and_install_line(self):
        marketplace = json.loads((ROOT / ".claude-plugin/marketplace.json").read_text())
        self.assertEqual(marketplace["name"], "oberthci")
        self.assertEqual(marketplace["plugins"][0]["source"], "./agent")
        for host in (".claude-plugin", ".codex-plugin"):
            manifest = json.loads((AGENT / host / "plugin.json").read_text())
            self.assertEqual(manifest["name"], "oberth")
        manifest = json.loads((AGENT / ".codex-plugin/plugin.json").read_text())
        self.assertEqual(manifest["skills"], "./skills/")
        self.assertEqual(manifest["hooks"], [])
        self.assertTrue((AGENT / manifest["skills"] / "oberth/SKILL.md").is_file())


if __name__ == "__main__":
    unittest.main()
