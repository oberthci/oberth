# Merge guards

Run make guards from the checkout. Make check validates the portable plans,
runs race tests and tooling fixtures, lint, then the guards. The Python entry
point validates the closed policy schema before invoking the Go AST backend.
Unknown keys, effects, roles and component references fail with their key.

The gate checks a detached branch snapshot using main's tooling and complete
plans bundle. Changes to either are listed; a second detached worktree then
runs the branch's own check and parity. An external names policy with no blank patterns is required. Pin changes are
printed package by package under GATE FILES CHANGED and require a separate commit
containing only plan JSON data.
Existing parity cannot be deleted. The container has a 25-minute deadline.

Dependencies are closed, numbered denies win, every Go package has exactly
one owner, and every product package has a package Spec comment naming an
existing ATX heading outside fences. Harnesses and disposable fixtures have
their declared scopes. Conventional test helpers inherit their parent's test
permissions. Exact helper names are fake and contracttest; a suffix of test
also qualifies when only test files import the package. Main packages never
qualify. Ordinary production imports of test support are refused. Symbol rules
apply to non-test files.

A component's inspections become due when an owned package outside the
existing shared contract list is present. Pinned runtime contracts and
their test kits are excepted alongside pure shared values. Each contract test has one directly attached Covers marker naming
one current id, the planned inspection name and the owning component.
Unknown and retired ids, multiple claims, mismatched names and Go claims for
walk inspections fail. Walk inspections become due when test/walk/walklib.py
is present; the checker reads its literal STEPS table without executing it.
A step in that table is defined. Only a passed step in a checked-in candidate
result for the exact checked commit is proven. A missing definition fails;
a defined step without current proof does not. With no walk, its inspections
wait. The summary counts test coverage, definitions and proofs separately;
proven inspections are also counted as defined.

Candidate runs use one JSON file each under test/walk/results/, named with
their UTC run start, for example 20261116T170000Z.json. The lexically newest
checked-in filename wins, independent of checkout times. Only files present
in HEAD are read; staged files and working edits cannot supply proof. The
minimal format is:

```json
{
  "commit": "0123456789abcdef0123456789abcdef01234567",
  "steps": [
    {"name": "push", "verdict": "pass", "evidence_sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
  ]
}
```

The commit identifies the candidate product tested. evidence_sha256 is the
lowercase SHA-256 of the evidence summary's exact bytes, retained with the
run's evidence outside the checkout. Verdicts are pass, fail, blocked or
skipped; only pass proves an inspection. Current results require unique,
existing steps and valid verdicts and hashes. A different commit is reported
as stale, contributes no proof, and does not fail when the table has changed
or the walk is absent. This exact comparison also makes a
result stale after an evidence-only commit; there is no implicit equivalence
between commits. Published-release evidence does not qualify as a candidate
result. Recording evidence and verifying its live assertions belong to the
walk; this guard validates the record and its binding, not the assertions.

Retired vocabulary uses exact word-boundary selectors and per-check directory
exemptions in retired.json. There is no named-exception mechanism.
Markers use the linter's plain keyword list. Empty test skips are refused.

Names use an outside-repository file and search tracked text and commit
messages. A plugin docs allowance starts at the first unfenced level-2
"Agent plugins" heading and ends at the next level-1 or level-2 heading;
duplicate or wrong-level headings are findings. JSON and YAML plugin manifests
receive the same structured authorship check, including owners and maintainers.
History checks configured identity and attribution lines. Reachability
runs `go tool` with its own declaration and version in `hack/tools/deadcode.mod`,
keeping the analyzer and linter dependency graphs separate.
Tests cover the module, while production checks
the live mains' dependency closure and owned packages outside it. Source
classification supplies roots and product packages through the AST report;
production imports cannot reach test support.
Name matches print only their masked location and rule. Other reasons and remedies come from messages.json; a
single catalogue test checks every pair against 80/120 characters with long
detail inputs. Source text and command output stay in the separate detail.

The Python portion keeps Git searches, walk-table parsing and subprocess
handling shorter than Go process glue; Go owns source AST checks. There is
no temporary parser binary or JSON offset handoff.
Successful subprocess diagnostics stay separate from structured output;
failed commands include their diagnostics in the finding.

Warm the analyzer once with `go tool -modfile=hack/tools/deadcode.mod deadcode -help`.
With its modules cached, run the guard suite offline:

```sh
GOPROXY=off python3 -m unittest discover -s hack/guards -p 'test_*.py'
```

The reachability suite checks tool startup once before running its fixtures;
a missing declaration or build dependency produces one setup error naming the tool.

Test reachability always covers the module; production is skipped until the
marked `product_command` in `docs/plans/dependencies.json` is a real main that
builds, with invalid shapes reported as findings.
Production roots are the existing listed `production_commands`, with no
inferred roots, and every owned product package outside their dependency
closure is a finding except test support and harnesses.
Every other main must be a declared harness; unlisted mains are reported once
per package.

A deny rule applies to every file with one exception: a test file may import
what `test_allow` grants its package explicitly. Production files never get
that exception.
