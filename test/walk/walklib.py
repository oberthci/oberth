"""Drive the published-release acceptance walk with a single assertion table."""
import argparse
from dataclasses import dataclass
import json
import os
from pathlib import Path
import re
import sys
import time
import uuid
from walkevidence import write_json
from walkassert import Blocked, Failure, HERE, outside_git, require
from walkconfig import Environment
from walkio import Session
from walkhost import Runtime, Load
from walkux import UX, required_patterns
from walkreference import approved, record_sample
from walkstate import remember
import walkcleanup as cleanup
import walkevidence as evidence
import walksetup as setup
import walkruns as checks
from walkmessages import collect


@dataclass(frozen=True)
class Step:
    name: str
    assertion: str
    action: object
    needs: tuple = ()
    milestone: str = ""
    phase: str = ""


def ux_gate(s):
    report = s.ux.report(kind=s.kind)
    if getattr(s.args, "counts_only", False):
        report["regressions"] = report["failures"]
    expected = {spec.name for spec in STEPS if spec.name != "ux-gate"}
    allowed = approved({"target": "published signed release " + s.args.version, "cli_sha256": s.data.cli_sha256,
                        "steps": s.results, "ux": report}, expected)
    require(not report["failures"], "UX regression: " + "; ".join(report["failures"]))
    if s.args.record_baseline:
        require(s.kind == "reference", "Only the verified reference may calibrate")
    elif not report["baseline_recorded"] or not all(m["complete"] for m in report["milestones"].values()):
        raise Blocked("UX baseline/milestones are incomplete")
    if s.runtime.load.busy and not getattr(s.args, "counts_only", False):
        raise Blocked("Host 5-minute load exceeded 12; result is inconclusive",
                      {"host_busy": True, "time_overruns": report["time_overruns"]})
    if s.args.record_baseline:
        sample = {"mode": s.args.mode, "release": s.args.version, "evidence": s.args.out.name,
                  "reference_gaps": sorted(allowed), "reason": s.args.baseline_reason, **report["milestones"]}
        s.pending_baseline = record_sample(s.ux.baseline, sample)
        return {"reference_sample_recorded": True, "reference_gaps": sorted(allowed)}
    require(not report["regressions"], "UX regression: " + "; ".join(report["regressions"]))
    return {"measured_baseline": True, "time_tolerance_percent": 25, "reference_gaps": sorted(allowed)}


STEPS = (
    Step('preflight', 'Provided slot, tools and host capacity are usable', setup.preflight),
    Step('target', 'Signed CLI verifies before execution', setup.target, ('preflight',)),
    Step('cluster', 'Stock fresh slot or unchanged warm platform', setup.cluster, ('target',), 'cluster_ready'),
    Step('fixture', 'Disposable upstream contains committed authored source', setup.fixture, ('cluster',), phase='first_green'),
    Step('install', 'Default signed installation and verification complete', setup.install, ('fixture',), phase='first_green'),
    Step('login', 'Saved administrator profile authenticates through the CLI', setup.login, ('install',), phase='first_green'),
    Step('connect', 'Repository connects and checkout binds', setup.connect, ('login',), phase='first_green'),
    Step('onboard', 'Distinct invited client profile binds', setup.onboard, ('connect',), phase='first_green'),
    Step('push', 'Ordinary push prints the exact run link', checks.push, ('onboard',), phase='first_green'),
    Step('live-logs', 'CLI follow and agent output expose bytes with an independently running Pod', checks.live_logs, ('push',), phase='first_green'),
    Step('build', 'Exact clean source builds with VCS stamping', checks.build, ('push',), 'first_green', 'first_green'),
    Step('retained-logs', 'Both interfaces retain the exact source/build proof', checks.retained_logs, ('build',), phase='first_release'),
    Step('ui', 'Leaf entrypoint label uses the authored template', checks.ui, ('push',), phase='first_release'),
    Step('grant', 'The exact release step receives its approved signing grant', checks.grant, ('connect',), phase='first_release'),
    Step('promote', 'Forge main contains tested source and is tested', checks.promote, ('build', 'grant'), phase='first_release'),
    Step('release', 'Exact tag publishes a verified image and durable event', checks.release, ('promote', 'grant'), 'first_release', 'first_release'),
    Step('release-ui', 'Release leaf label uses the authored template', lambda s: checks.ui(s, s.data.release_run, s.data.tag_object, True), ('release',), phase='checks'),
    Step('failing-build', 'A failed source step supplies the exact working log command in CLI, wait and agent views', lambda s: checks.negative(s, 'failing'), ('onboard',), phase='checks'),
    Step("server-restart", "A source step waits through the server outage and finishes without native retries", lambda s: checks.negative(s, "restart"), ("onboard",), phase="checks"),
    Step('readonly-source', 'Default root cannot write or chmod delivered source', lambda s: checks.negative(s, 'readonly'), ('onboard',), phase='checks'),
    Step('admission-control', 'The same refusal queries find an accepted run and Workflow', checks.admission_control, ('onboard',), phase='checks'),
    Step('invalid-yaml', 'Unknown native fields are refused at push with both offender names', lambda s: checks.negative(s, 'invalid'), ('admission-control',), phase='checks'),
    Step('ungranted-secret', 'Exact grant refusal starts no run or Workflow', lambda s: checks.negative(s, 'ungranted'), ('admission-control',), phase='checks'),
    Step('cleanup', 'Owned upstream and registry resources are removed by exact names', cleanup.cleanup),
    Step('ux-gate', 'User effort, product time and remedies meet the target budget', ux_gate, ('cluster',)),
)


class Walk:
    def __init__(self, args, config, runtime):
        self.args, self.clock, self.started = args, runtime.clock, runtime.clock()
        self.out = args.out.resolve()
        destinations = (self.out, config.path("private"), config.path("state"))
        require(all(outside_git(p) for p in destinations), "Evidence and private state must be outside worktrees")
        require(
            self.out.parent.is_dir() and not self.out.exists(),
            "Choose a new evidence directory with an existing parent",
        )
        self.out.mkdir(mode=0o700)
        self.s = Session(config, args, UX(HERE / "golden-path.json", args.mode, runtime.clock), runtime)
        self.flush()

    def step(self, spec):
        s, started = self.s, self.clock()
        s.current, s.ux.phase = spec.name, spec.phase
        begin = len(s.commands)
        status, reason, facts = "pass", "Assertion satisfied", {}
        outcomes = {r["name"]: r["status"] == "pass" for r in s.results}
        try:
            if not all(outcomes.get(name, False) for name in spec.needs):
                status, reason = "skipped", "Required preceding assertion did not pass"
            else:
                facts = spec.action(s) or {}
                require(isinstance(facts, dict), "Walk step must return its public facts as an object")
        except Blocked as error:
            status, reason, facts = "blocked", str(error), error.facts
        except Exception as error:
            status, reason = "fail", str(error)
            facts = error.facts if isinstance(error, Failure) else {}
        remember(s)
        self.message_expectation(spec.name, status, begin)
        row = {"name": spec.name, "status": status, "reason": reason, "assertion": spec.assertion,
               "duration_seconds": round(self.clock() - started, 3), "evidence_path": spec.name + ".json",
               "facts": facts, "commands": s.public_commands()[begin:]}
        if spec.milestone and status == "pass":
            s.ux.mark(spec.milestone)
        s.results.append(row)
        self.flush()
        print(f"{status.upper()} step={spec.name} duration={row['duration_seconds']:.3f}s", flush=True)

    def message_expectation(self, name, status, begin):
        expectation = self.s.expectations.get(name)
        if expectation is None or status == "skipped":
            return
        commands = remedy_commands(self.s.commands[begin:], expectation["when"] == "always")
        if expectation["when"] != "always" and not commands:
            return
        text = []
        diagnostics = []
        for command in commands:
            output, diagnostic = self.product_output(command)
            text.append(output)
            diagnostics.append(diagnostic)
        replacements = {"base": self.s.config.base, "repo": self.s.canonical, "run_id": self.s.data.run_id}
        if "refusal" in expectation:
            scope = expectation["refusal"]
            expectation = {
                **expectation,
                "refusal": {**scope, "contains": [t.replace("{repo}", self.s.canonical) for t in scope["contains"]]},
            }
        required = required_patterns(expectation)
        expected = {**expectation, "required": required, "diagnostic": "\n".join(diagnostics)}
        for key, value in replacements.items():
            expected["required"] = [p.replace("{" + key + "}", re.escape(value)) for p in expected["required"]]
        self.s.ux.expect(name, "\n".join(text), expected)

    def product_output(self, command):
        stem = self.s.private / command["capture"]
        raw = stem.with_suffix(".stdout").read_text() + stem.with_suffix(".stderr").read_text()
        if command["operation"] == "git-push":
            product = "\n".join(line for line in raw.splitlines() if line.startswith("remote: "))
            return product, product
        return raw, command["message"]

    def verdict(self):
        rows = self.s.results
        statuses = {r["status"] for r in rows}
        if "fail" in statuses:
            return "FAIL"
        if "skipped" in statuses and "blocked" not in statuses:
            return "FAIL"
        if len(rows) != len(STEPS):
            return "BLOCKED" if "blocked" in statuses else "INCOMPLETE"
        if self.s.runtime.load.busy and not self.args.counts_only and all(only_host_block(r) for r in rows):
            return "INCONCLUSIVE"
        return "BLOCKED" if "blocked" in statuses else "PASS"

    def flush(self):
        rows, verdict = self.s.results, self.verdict()
        result = self.s.clean(
            {
                "schema_version": 2,
                "target": "published signed release " + self.args.version,
                "target_kind": self.s.kind,
                "cli_sha256": self.s.data.cli_sha256,
                "host_busy": self.s.runtime.load.busy,
                "mode": self.args.mode,
                "slot": self.s.config.slot,
                "verdict": verdict,
                "host_load": {
                    "one_minute_start": self.s.runtime.load.samples[0][0],
                    "five_minute_max": max(sample[1] for sample in self.s.runtime.load.samples),
                },
                "duration_seconds": round(self.clock() - self.started, 3),
                "steps": rows,
                "ux": self.s.ux.report(self.s.secrets, (self.s.private, self.s.config.root), self.s.kind),
                "owned_resources": {
                    "repository": self.s.data.name if self.s.data.repository_created else None,
                    "key_id": self.s.data.key_id,
                    "images": [n for n in (self.s.data.tools_repository, self.s.data.image_repository) if n],
                },
            }
        )
        write_json(self.out / "result.json", result)
        for row in result["steps"]:
            write_json(self.out / row["evidence_path"], row)
        lines = [f"{verdict}: {result['target']} ({self.args.mode}); {result['duration_seconds']}s"]
        for row in result["steps"]:
            lines.append(f"{row['status']} {row['name']} {row['duration_seconds']}s: {row['reason']}")
            lines.extend(f"  {c['command']} (exit {c['exit_code']}): {c['message']}" for c in row["commands"])
        lines.append("UX: " + json.dumps(result["ux"], ensure_ascii=False))
        (self.out / "summary.txt").write_text("\n".join(lines) + "\n")
        return result

    def execute(self):
        try:
            for spec in STEPS:
                self.step(spec)
        except KeyboardInterrupt:
            self.lifecycle_failure("blocked", "Interrupted; rerun after inspection")
        except Exception as error:
            self.lifecycle_failure("fail", str(error))
        finally:
            try:
                if not any(row["name"] == "cleanup" for row in self.s.results):
                    self.step(next(spec for spec in STEPS if spec.name == "cleanup"))
            except Exception as error:
                self.lifecycle_failure("fail", str(error))
        result = self.publish()
        self.persist_reference(result)
        return {"PASS": 0, "BLOCKED": 2, "INCONCLUSIVE": 2}.get(result["verdict"], 1)

    def persist_reference(self, result):
        if (
            self.s.pending_baseline is not None
            and not result.get("host_busy", False)
            and any(r["name"] == "ux-gate" and r["status"] == "pass" for r in result.get("steps", []))
        ):
            write_json(HERE / "golden-path.json", self.s.pending_baseline)

    def publish(self):
        try:
            result = self.flush()
            write_json(self.out / "messages.json", collect(self.s))
        except Exception as error:
            result = {"schema_version": 3, "verdict": "FAIL", "evidence_error": self.s.clean(str(error))}
            write_json(self.out / "result.json", result)
            (self.out / "summary.txt").write_text("FAIL: " + result["evidence_error"] + "\n")
        try:
            evidence.scan(self.out, self.s.secrets)
        except Failure as error:
            for path in evidence.unsafe_evidence(self.out, self.s.secrets):
                path.unlink()
            result = {"schema_version": 2, "verdict": "FAIL", "evidence_error": self.s.clean(str(error))}
            write_json(self.out / "result.json", result)
            (self.out / "summary.txt").write_text("FAIL: " + result["evidence_error"] + "\n")
            evidence.scan(self.out, self.s.secrets)
        finally:
            cleanup.close_local(self.s)
        return result

    def lifecycle_failure(self, status, reason):
        self.s.results.append({"name": "lifecycle", "status": status, "reason": reason,
                               "assertion": "Complete evidence publication", "duration_seconds": 0,
                               "evidence_path": "lifecycle.json", "facts": {}, "commands": []})


def remedy_commands(commands, always):
    commands = [c for c in commands if product_command(c)]
    if always:
        return commands
    failed = [c for c in commands if c["exit_code"] != 0 and not c.get("stopped_after_observation")]
    failed.extend(c for c in commands if c["exit_code"] == 0 and c.get("product_failure"))
    return failed


def product_command(command):
    return command.get("operation") and (command["argv"][0] == "oberth" or command["operation"] == "git-push")


def only_host_block(row):
    return row["status"] == "pass" or (row["name"] == "ux-gate" and row["status"] == "blocked"
                                       and row.get("facts", {}).get("host_busy", False))


def parser():
    result = argparse.ArgumentParser(description=__doc__)
    result.add_argument("--env", type=Path, help="External environment JSON, using env.example's schema")
    result.add_argument("--slot", choices=("walk-1", "walk-2"), default="walk-1")
    result.add_argument("--mode", choices=("fresh", "warm"), default="fresh")
    result.add_argument("--version", default="v0.23.4")
    result.add_argument("--candidate", type=Path, help="Unsigned bytes from this admitted run")
    result.add_argument("--counts-only", action="store_true", help="Record times without enforcing a time gate")
    result.add_argument("--out", type=Path)
    result.add_argument("--run-timeout", type=int, default=600)
    result.add_argument(
        "--record-baseline", action="store_true", help="Record a reference with only exact approved gaps"
    )
    result.add_argument(
        "--baseline-reason", default="", help="Recorded explanation required for a measured baseline increase"
    )
    result.add_argument("--plan", action="store_true")
    return result


def main(argv=None):
    os.umask(0o077)
    args = parser().parse_args(argv)
    if args.plan:
        print(
            json.dumps(
                {
                    "target": "published signed release " + args.version,
                    "slot": args.slot,
                    "mode": args.mode,
                    "steps": [{"name": x.name, "assertion": x.assertion, "needs": x.needs} for x in STEPS],
                },
                indent=2,
            )
        )
        return 0
    if args.env is None or args.out is None:
        parser().error("--env and --out are required unless --plan is used")
    try:
        require(1 <= args.run_timeout <= 600, "Product wait timeout must be between 1 and 600 seconds")
        with Load(os.getloadavg) as load:
            walk = Walk(
                args, Environment.load(args.env, args.slot), Runtime(time.monotonic, lambda: uuid.uuid4().hex, load)
            )
            try:
                return walk.execute()
            finally:
                cleanup.close_local(walk.s)
    except (Failure, OSError, ValueError) as error:
        print("walk: " + str(error), file=sys.stderr)
        return 2 if isinstance(error, Blocked) else 1
