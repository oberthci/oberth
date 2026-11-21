# Rewrite plans

These specifications describe Oberth's current build contract. Repository YAML
defines work, Argo executes it, and Kubernetes places Pods. Each spec owns one
component. Shared Go declarations define the public types and method signatures;
[dependencies.json](dependencies.json) defines ownership and allowed imports.

Status: approved by the owner on 2026-11-16; changes receive plans review.

## Index

| Spec | Component | Scope |
|---|---|---|
| [C1](c1-transport-and-tool-registry.md) | Transport and tool registry | `internal/api`: HTTP/MCP framing, authentication handoff, public DTOs, tool registration, bounded responses, embedded UI |
| [C2](c2-service-actions-and-history.md) | Service actions and history | `internal/service`: attributed actions, repository scope and grants, accepted-work lifetime, run/promotion/publication/issue history |
| [C3](c3-wiring-and-submission.md) | Wiring and submission | `internal/app`: one attributed run binding carried across admission, execution, source custody, state and observation |
| [C4](c4-admission-rules-and-plan.md) | Admission rules and plan | `pkg/argoworkflow` (with `defaults`, `specsize`): validates repository YAML; supplies declarations and the step plan for one prepared admission |
| [C5](c5-native-execution.md) | Native execution | `internal/argojob`, `internal/argowatch`: native account/token hooks, submit once, observe or adopt the owned Workflow |
| [C6](c6-source-delivery.md) | Source delivery | `internal/sourcedelivery`, `internal/sourceplugin`, `internal/gitcache`: exact admitted source as a read-only native input artifact; bare cache, outbox, publication mechanics |
| [C7](c7-durable-state.md) | Durable state | `internal/store`: canonical identities, accepted work, authority, obligations, evidence; atomic facts/audit/events; verified snapshots |
| [C8](c8-logs.md) | Logs | `internal/runlog`: bounded container output, exact span reads, filtering and evidence removal |
| [C9](c9-install-and-upgrade.md) | Install and upgrade | `internal/installer`, `internal/setuptui`, `charts/`: one installation engine for plain prompts with a review screen and unattended use; upgrade, rollback and backup |
| [C10](c10-schedules.md) | Schedules | `internal/schedule`, `internal/cronsync`: admitted schedule entries reconciled into native CronWorkflows; child parent-identity verification |
| [C11](c11-cli-and-server-composition.md) | CLI and server composition | `cmd/oberth`: executable boundary, health/readiness/metrics and composition root |
| [C12](c12-agent-client.md) | Agent client | `agent/`: agent session connection, onboarding, run history, push/wait/assist/promote loop |
| [C13](c13-own-pipelines.md) | Own pipelines | `.oberth/*.yaml`, `hack/`: reference pipelines and inspection harnesses, from admitted source to signed, verified release bytes |
| [C14](c14-docs.md) | Documentation | `docs/`, root `README.md`: current guides, generated references, checked examples, separation from history |
| [PARITY](PARITY.md) | Executable parity matrix | Source-derived inventory of CLI commands/flags, MCP tools/arguments, HTTP endpoints, Helm values, store records, metrics/events, installer actions, Agent client commands, Git push options, UI views, Client metadata and Sweep deletions. Inputs: `dispositions.json` and `sweeps.json`. Regenerate with `python3 hack/plans/parity.py --reference <old checkout> --rewrite <REWRITE.md>`; portable verification: `python3 hack/plans/parity.py --check` |

[REQUIREMENTS.md](REQUIREMENTS.md) is generated from `requirements.json`.
Each requirement has one sentence, an owner, a class, a user loss and one
inspection. Each spec links its requirements once beside their inspections.
Retired IDs are never reused. A component has at most ten requirements, except
C11, whose command line and server composition share a limit of thirteen; the
exception is recorded in `requirements.json`.

An inspection tests its requirement's sentence. External old-code cases are
reference material, consulted when a sentence's inspection or walk fails;
they are not a checklist of rules to bring back. Historical narratives and
old-code citations live in the coordination evidence file `spec-evidence.md`.
Inspections here are obligations to implement, not claims of passing results.

The portable check uses Python 3 alone:
`python3 hack/plans/parity.py --check`. Explicit inventory regeneration uses
`--reference <old checkout> --rewrite <REWRITE.md>` with PyYAML and Go.
`dispositions.json` records reviewed changes to the parity inventory;
`sweeps.json` records deletion decisions. **Delta** marks their current clauses.
The comparison baseline in `dispositions.json` is the `main` commit immediately
before the fold. When a fold lands, advance the baseline to the resulting
`main` commit and its inventory.
The parity tool's pinned-source regression runs with
`OBERTH_PARITY_REFERENCE=<old checkout> OBERTH_PARITY_REWRITE=<REWRITE.md>
python3 -m unittest discover -s hack/plans -p test_regeneration.py`.

## Product priorities

It works and is pleasant first: install, connect, push, read logs, promote and
release with clear defaults and a useful next action. Keep the code thin;
use the standard library and native Kubernetes and Argo mechanisms first.
YAML owns resources, placement, retries, synchronization and optional deadlines.
The only run cap is Argo's global parallelism setting. Oberth has no scheduler,
capacity queue, dependency engine, resource estimator or automatic run retries.

The always-on boundary is small:

1. Authenticate operators and agent uplinks, including each uplink's repositories.
2. Move main through promotion of the exact tested commit or operator action;
   releases use the default branch's release pipeline and history.
3. Deliver secrets and release credentials only through an operator's exact
   repository, workflow-file and step grant. Branch CI gets no release credential.
4. Deliver exactly the admitted source commit, read-only.
5. Keep server state private. Admission refuses host paths, host namespaces,
   privileged containers and the server's accounts or volumes.
6. Bound requests and validate caller names, refs and paths before using them
   in Git commands, files or URLs.

Admission is the one layer for authored YAML: later stages trust the sealed
prepared admission. Token delivery and source loading check caller identity and
recorded ownership; they do not re-admit the work or replay authored-shape checks.
C3 submission and C11 offline inspection call C4 `Prepare`; sealing stays restricted to C4.
Offline inspection separates authored refusals from installation needs and shares push commands.

[C9 invariant 7](c9-install-and-upgrade.md#invariants) defines baseline Pod defaults,
the link-local egress filter, review and opt-out.
The local hash-chained audit log is always on; verify it on reads, backup and restore, never for every mutation.
Save audit records atomically with their state changes.

Use plain names, one representation of each fact, small functions and injected
dependencies. Refusals name all independently knowable problems and their fixes.
No aliases, compatibility paths, deployment-specific code or owner names enter
current code or examples.

## Current contracts

- Native source-consuming schedules are refused for v0.24.0; manual schedule
  triggers use ordinary run admission. Native non-source schedules remain active.
- The first release has no restore or uninstall command. Backup is safekeeping;
  removal follows the install guide's native procedure.
- The first release is made by hand under C13's first-release limits.
- Run history starts fresh. Cutover is an operator procedure; issues arrive once
  through [C2's manual import](c2-service-actions-and-history.md#manual-issue-import).
  [C9's command inventory and backup contract](c9-install-and-upgrade.md#owned-data-and-public-contract)
  define maintenance surfaces and database safekeeping.
- Operator and installer keys run admitted pushes; agent keys park unless the
  push uses `oberth.run=1`. Push booleans accept only `=1` and `=0`.
- `run_check` and `oberth check` are read-only preparation using C4's validator.
- Schedules have no minimum interval or frequency refusal; `maxEntries` bounds
  managed objects. The conformance release is v0.24.0.

## Shared contracts

The exported declarations in the fresh repository are the shared contract.
Ownership, consumers, pure values, runtime adapters and test support are listed
once in [dependencies.json](dependencies.json). C12 and C14 consume public
commands and data without server imports. Pure shared values do no external
I/O; callers read bounded inputs and write returned bytes. Runtime readers and
clients retain their declared I/O and exact permissions. The `shared` list
identifies pinned contract packages, including runtime contract adapters.
The pinned list changes only in commits containing plan JSON data alone; the gate
prints each pinned contract added or removed under GATE FILES CHANGED.
Requirements are due when a component owns a present package that is not a
pinned contract package. Walk inspections use the walk step table once present;
a Go Covers marker for a walk requirement is refused. A package named `fake` or
`contracttest`, or ending in `test` with no `main` package and imported only from
`_test.go` files, directly under an owned package P belongs to P's owner. It may
import P and what P may import, including its test permissions. It may be imported
by P's tests, tests allowed to import P, and test-support packages within their
parent's permissions; production imports are refused. Symbol restrictions apply
only to non-test files, so tests may call `admissioncontract.Seal` to build
prepared-admission fixtures.

### Shared values and errors

Use the bounded standard JSON decoder: refuse unknown fields and trailing data,
then validate the typed value once. Explicit null means absent, as does omission;
there is no presence tracking. Native authored YAML keeps its own case-sensitive
schema decoding. C6's `reponame` validates bare and canonical repository names;
`gitoid` validates object IDs, and C6 owns ref and path validation.

C2 owns public error classification and Permanent, the predicate requiring every
public cause to be caller-fixable; Git absence/author errors unwrap to public errors.
Invariant failures dominate; otherwise the
error nearest the caller determines the code. Same-code refusals retain all
remedies, and private causes stay private. A zero actor has no authority;
a named actor's empty scope retains its installation-wide meaning. Issue,
patch and filter values live in `internal/model`. Configuration values contain
no clients; C9 owns the bounded runtime reader, returning copied snapshots.

### Prepared admission and plan

C3 calls C4 preparation, which invokes C5 transformation hooks before final
native schema validation, planning and sealing. Only C4 calls
`admissioncontract.Seal`. Prepared fields remain private; constructors and
accessors make deep copies, including maps, native objects and raw snapshots.
PrepareHooks includes BindSource: C4 discovers descriptors, calls the C3-provided
binder and records coordinates before sealing. The pipeline account is the
executor account; identity evidence records it once. Argo mounts a Pod-bound
projected token. Create receives the sealed spec unchanged. C10 copies it into a schedule parent;
a child acquires a run identity only when accepted.

C6 owns `sourcecontract.CanonicalJSON`: sorted keys, UTF-8, no insignificant
whitespace, preserved array order and parsed raw snapshots. The spec digest
covers the final spec; the admission digest covers the whole view except itself.
Views contain no secret bytes; audit records contain digests rather than dumps.

One containerSet invocation is one logical step with separate container logs.
Literal loops expand within 2048 entries and 1 MiB of plan JSON. Overflow becomes
a dynamic placeholder rather than a refused run. Native observations expand
that placeholder without rewriting the immutable plan. Counts cover the full
display projection regardless of paging. Unreached static steps stay pending;
a terminal dynamic remainder resolves to unknown.

<a id="public-run-and-step-dtos"></a>

### Public run and step values

C4 owns step values with C5 and C2 review. C5 supplies native evidence and typed
failures; C2 projects those facts without inferring a different cause. C1 renders
the same declarations, C7 stores them atomically, and the agent client displays them.
Publication retry advice is separate and never starts an automatic run retry.

Run JSON's `failure` record contains `failure_class`, `failed_step` (the plan path),
`failed_step_name` (the plan step's name), `node_id` and `retry_hint`.
For example, an exit hook keeps `"failed_step":"onExit[0].cleanup"` beside
`"failed_step_name":"cleanup"`; a failure without a plan step has an empty name.

Use `omitzero` for steps, attempts and logs: requested empty pages are non-nil
empty arrays; absent pages are omitted. Lists omit step pages and use stored
counts. Unknown evidence stays unknown. Run success and publication are distinct.
`object_sha` is the accepted object; `commit_sha` is the tested checkout commit.
Steps stay in plan order. Each step carries `credential_intent` from its plan entry;
views show one `secret` tag without exposing a secret name or value.

Status reasons are complete phrases of at most 40 characters, most important
word first. Refusal/error reasons are one sentence of at most 80 characters;
remedies are one sentence of at most 120. Native messages stay whole in a
separate `detail` field beside the reason. Views never truncate or add ellipses.
Each component lists every generated reason in one place and tests the entire
catalog, using `internal/stepmodel/steptest` for status reasons and publicerr
construction tests for errors. C2 owns run/step projection, C5 native mapping,
C1 UI/tool output and C12 agent presentation. Full detail survives JSON and storage.

### Source and Git ports

Store sealed admission and the run, then call C3's one-method Keep port before
Create. C6 retains roots as Git refs; C5 records Workflow name and UID once.
Load selects that run and matches the authenticated Pod to its recorded Workflow.
Terminal runs return 403; a run without a recorded UID returns retryable 503.
Tags retain their own objects, and a missing recorded tag refuses Load. After
restore, re-pin live runs before sweeping unowned refs.

Git requests name exact repository, ref, object, predecessor and action.
`AuthorRefusalError` has Path, Code, Message and Hint as declared in gitport.
Missing objects, invalid caller inputs and unavailable I/O remain distinct;
a failed read never proves absence. Cancellation covers locks and child I/O.

For port P, P/fake and P/contracttest belong to its owner and may import P and
its shared values. Only tests consume that support. One contract suite runs
against both the fake and real implementation. C3's Keep port follows this rule.
`internal/stepmodel/steptest` is C4-owned test support available to every
component's tests, excluded from the production dependency graph.

### State and acceptance

C7 owns the canonical run, acceptance batch/result, branch deletion, query and
event values in `internal/stateport`. C2 and C3 import them without copies or
conversions. Decide authority before the seal; Accept records one complete batch
without rechecking authority. C3 prepares, creates and recovers. Shared contract
tests run against C7's real store and stateport's in-memory fake.

### Events

C2 owns the eight event kinds, including observation-only `run.updated`, in
[Events and observation](c2-service-actions-and-history.md#events-and-observation).
The component recording each fact supplies its events in the same C7 call.
C7 allocates cursor IDs and persists deduplication; C1 delivers the model values
through `events_wait` and notifications pushed to interactive sessions.
Counts and exactness appear together or neither. Public pages have at most 200
entries, internal scans 500. Summaries follow the public message rule; larger
native text goes in detail, never a clipped summary.

## Dependency rules

[dependencies.json](dependencies.json) is the closed local-import policy.
Unlisted local edges refuse; standard library and pinned native dependencies
remain allowed. `p/**` matches p and descendants; p alone matches only p.
Production reachability starts at every command in `production_commands`, including
the CLI, source plugin, release publisher and stopped-server backup helper.
The production permission graph is acyclic after overrides and denies.
Leaf package permissions narrow component permissions. Test/fixture permissions
never grant a production import.

C11 alone composes the server. C2 declares consumed interfaces; C3 adapts them.
C3 and C10 use callbacks for schedule integration without importing each other.
Only C3 calls Prepare; C5/C10/C11 use listed schema or plan readers. Store imports
shared values, not component implementations. Shared packages never import their
consumers. Product code never imports harness trees or retired package paths.
The exact symbol and package permissions are maintained in JSON, without a
second package table here. `hack/plans/parity.py --check` validates ownership,
consumer permissions, sealing access and graph cycles.

## Walk

The walk runs install → connect → push → live/retained logs → UI → promote →
release, with real Git and source delivery. `failing-build` adds a failing run;
`ungranted-secret` → `grant` → `push` proves the printed remedy. `ux-gate`
checks message lengths and the recorded commands, flags and time budgets.
A complete baseline is required; 25% growth fails the gate. Component catalog
checks cover every generated reason, including uncommon refusal paths.

Harness captures are published only after scanning the complete bounded capture for raw and encoded credentials; preserve the original failure separately.

## Parity

Inventory regeneration uses the read-only reference checkout. Portable checks
need only the committed plan bundle. A passing old-code test is evidence to
consider; only the new component inspection and walk establish the new behavior.
History and source citations stay in external evidence. A marked change has a
reviewed disposition and quoted current clause.

Harness captures are published only after scanning the complete bounded capture for raw and encoded credentials; preserve the original failure separately.

## Retired terms

Only `docs/plans/`, `docs/releases/` and `docs/archive/` are exempt from retired
vocabulary and leftovers checks. Refusals use current terms instead of quoting
retired selectors. There is no named-exception schema or other exemption.
`retired.json` lists exact word-boundary selectors and replacements; ordinary
English words with current meanings are allowed.

| Id | Retired concept | Current replacement |
|---|---|---|
| R01 | Automatic workspace and tool mounts and environment (`oberth.ci/workspace-env`, `oberth.ci/workspace-mounts`), server-added caches and tool carriers | Authored YAML; explicit R01 exception: C5 automatically adds `safe.directory=/work/src` for main-source consumers , through the C4 preparation hook. Scaffolds, examples and the migration guide show the portable setting explicitly |
| R02 | Automatic run and release environment facts (`OBERTH_REPO/REF/SHA/TRIGGER/TAG`, injected `OBERTH_RELEASE_TAG/SHA`, `OBERTH_ARTIFACTS`) | Admitted native parameters and authored environment. `OBERTH_RELEASE_WIF` remains as an explicit selector |
| R03 | Secret-delivery wrappers from the env-template era, credential chains, step observers | Explicit projected tokens and the current secret client |
| R04 | Standalone execution mode | None. Independent operation of a dependency server is unaffected |
| R05 | Remote execution targets: routing, placement, credentials, journal and custody tables | One configured native execution namespace |
| R06 | The Oberth-owned Go module proxy | Authored module access |
| R07 | Server source PVC and claim seeding, seed Pods, source storage class, source annotations and the top-level source shorthand | Native source input artifact (C6) |
| R08 | Durable artifact and output collection for new runs, collector Pods | no artifact tools, routes, CLI, retention/budget flags or artifact purge option; `run_purge` removes logs only |
| R09 | Repository and tag fragments and their resolver | Native shared templates. Source bindings record admitted inputs |
| R10 | Controller-profile attestation, the nonroot annotation, old executor scratch defaults | Native controller settings plus identity verification |
| R11 | Technology-specific VM and device execution, launcher and node bridge, guest harness injection, `.oberth/tests.yaml` | Operator-owned native capability grants |
| R12 | Resource, weighted, size and exclusive-class admission; CPU pools, fair share, backfill, pressure gates, lookahead, credits, legacy resource grants | None |
| R13 | Queue caps, receive deferrals, disk-floor and admin holds, startup stalls and allowances, placement expiry, automatic infrastructure retry, default deadline policy | The global run cap, plus authored retries and deadlines |
| R14 | The resource sampler and step-usage measurement history | None |
| R15 | Pre-launch compatibility promises, aliases, casing and envelope adapters, old priority spellings, legacy fallback and repair branches | The current public DTO with an exact rejection |
| R16 | The old Git backend and its shadow mode, flat-cache adoption, installer-owned legacy controls, config and identity migrations, retired Transit configuration | The current Git cache (C6) and the current installer (C9) |
| R17 | The static pipeline DSL (`.oberth/ci.yaml`) and the runner planning that went with it | Admitted native YAML is the plan |
| R18 | Imperative seeding of plans and step rows | The plan computed from admitted YAML (C4) |
| R19 | `burn` as a user-facing phase or grouping  | `step` = plan path; `node_id` = native attempt; no grouping |
| R20 | Unqualified legacy, compatibility or retired narratives that keep removed architecture alive | The current contract, or release history |
| R21 | External audit witness configuration, acknowledgments and records | Local hash-chained audit records; no external witness |

Harness trees `test/**` and `hack/**` may import inside their own tree; product
imports require explicit test or fixture permissions. Product code never imports
them. The finite policy-data exceptions `PRODUCT_REPLACEMENTS` and `VM_TERMS`
in `hack/plans/parity.py` exist only to normalize source evidence and reject
forbidden names; they grant no permission for those names in product content.
