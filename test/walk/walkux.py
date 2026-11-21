"""Measure headless user operations and enforce measured milestone budgets."""
import hashlib
import json
import re
from walkcommands import COMMANDS, flags
from walkevidence import redact

COUNTS = ("commands", "flags", "manual_actions", "retry_steps", "wait_steps")


def required_patterns(expectation):
    if "required" in expectation:
        return expectation["required"][:]
    refusal = expectation["refusal"]
    return [re.escape(t) for t in [refusal["code"], *refusal["contains"], refusal["remedy"]]]


def baseline_changes(old, new):
    for mode, profile in new["profiles"]["reference"].items():
        previous = old["profiles"]["reference"].get(mode)
        if previous is None:
            continue
        if profile is None:
            raise ValueError("Measured baseline cannot be removed")
        if compare(previous, profile, 1.0) and not profile.get("reason", "").strip():
            raise ValueError("Baseline increases require a recorded reason")
    target_changes(old.get("candidate_target"), new.get("candidate_target"))


def target_changes(before, after):
    if before is not None:
        if after is None:
            raise ValueError("Candidate target cannot be removed")
        for name in ("first_green", "first_release"):
            for count, limit in before[name].items():
                if after[name].get(count, float("inf")) > limit:
                    if not after.get("reason", "").strip() or after["reason"] == before["reason"]:
                        raise ValueError("Target increases require a new owner reason")


def budget_errors(baseline, observed, time_factor=1.25):
    failures, slow = [], []
    if baseline is None:
        return failures, slow
    for name in ("first_green", "first_release"):
        milestone, budget = observed[name], baseline.get(name, {})
        for count, value in milestone["counts"].items():
            limit = budget.get("counts", {}).get(count)
            if limit is None:
                failures.append(f"{name}.{count}: baseline is missing")
            elif value > limit:
                failures.append(f"{name}.{count}: {value} > {limit}")
        if budget.get("seconds") is None:
            failures.append(name + ".seconds: baseline is missing")
            continue
        limit = budget["seconds"] * time_factor
        elapsed = milestone["seconds"] if milestone["complete"] else milestone.get("elapsed_lower_bound_seconds")
        if elapsed is not None and elapsed > limit:
            slow.append(f"{name}.seconds: {elapsed} > {limit:.3f}")
    return failures, slow


def compare(baseline, observed, time_factor=1.25):
    failures, slow = budget_errors(baseline, observed, time_factor)
    return failures + slow


class UX:
    def __init__(self, baseline, mode, clock):
        self.baseline, self.mode, self.clock = json.loads(baseline.read_text()), mode, clock
        self.origin, self.completed, self.phase = None, {}, "checks"
        self.commands, self.events, self.missing = [], [], []

    def command(self, record):
        step, argv, code = record["step"], record["argv"], record["exit_code"]
        role = COMMANDS[record["operation"]].role
        identity = hashlib.sha256(json.dumps(argv).encode()).hexdigest()
        prior = [c for c in self.commands if c["identity"] == identity and c["step"] == step]
        if role == "product" and prior and prior[-1]["exit_code"] != 0:
            self.event(step, "retry", "The same declared command required another attempt")
        self.commands.append({**record, "phase": self.phase, "identity": identity, "role": role,
                              "attempt": len(prior) + 1, "flags": flags(argv, record["operation"]),
                              "harness_flags": COMMANDS[record["operation"]].harness_flags})

    def event(self, step, kind, reason):
        self.events.append({"step": step, "phase": self.phase, "kind": kind, "reason": reason})

    def expect(self, step, text, expectation):
        required = all(re.search(pattern, text, re.I | re.S) for pattern in required_patterns(expectation))
        if not required:
            self.missing.append(
                {"step": step, "message": expectation.get("diagnostic", text), "needed": expectation["description"]}
            )
        return required

    def mark(self, name):
        if name == "cluster_ready":
            self.origin = self.clock()
        elif self.origin is not None and name not in self.completed:
            self.completed[name] = self.snapshot(name, True)

    def snapshot(self, name, complete=False):
        phases = {"first_green"} if name == "first_green" else {"first_green", "first_release"}
        commands = [c for c in self.commands if c["phase"] in phases and c["role"] == "product"]
        events = [e for e in self.events if e["phase"] in phases]
        elapsed = round(sum(c["duration_seconds"] for c in commands), 3)
        return {
            "complete": complete,
            "counts": milestone_counts(commands, events),
            "seconds": elapsed if complete else None,
            "elapsed_lower_bound_seconds": None if complete else elapsed,
        }

    def report(self, secrets=(), paths=(), kind="reference"):
        observed = {n: self.completed.get(n) or self.snapshot(n) for n in ("first_green", "first_release")}
        baseline = self.budget(kind)
        failures, slow = budget_errors(baseline, observed)
        regressions = failures + slow
        status = "pass" if baseline and len(self.completed) == 2 else "incomplete"
        if regressions or self.missing:
            status = "fail"
        commands = [{**c, "command": redact(" ".join(c["argv"]), secrets, paths)} for c in self.commands]
        for command in commands:
            del command["argv"]
        missing = [{**m, "message": redact(m["message"], secrets, paths)} for m in self.missing]
        return {"status": status, "mode": self.mode, "headless": True, "milestones": observed,
                "commands": commands, "observations": self.events, "missing_remedies": missing,
                "regressions": regressions, "failures": failures, "time_overruns": slow,
                "time_tolerance_percent": 25, "baseline_recorded": baseline is not None}

    def budget(self, kind):
        profile = self.baseline["profiles"]["reference"][self.mode]
        if kind == "reference" or profile is None:
            return profile
        value = json.loads(json.dumps(profile))
        target = self.baseline["candidate_target"]
        for name in ("first_green", "first_release"):
            value[name]["counts"].update(target[name])
        return value


def milestone_counts(commands, events):
    return {"commands": len(commands), "flags": sum(len(c["flags"]) for c in commands),
            "manual_actions": sum(e["kind"] == "manual" for e in events),
            "retry_steps": len({e["step"] for e in events if e["kind"] == "retry"}),
            "wait_steps": len({e["step"] for e in events if e["kind"] == "wait"})}
