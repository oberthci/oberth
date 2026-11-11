# Resource history and reviewed declarations (#1216, Stage A)

Git is the source of truth for what a step gets. Oberth captures usage and offers
read-only advice; it never changes requests, limits or retry resources. Existing
ordinary pod `resources:` and `activeDeadlineSeconds` remain the pipeline
syntax. Undeclared steps retain today's behavior.

The development loop is: change code, run CI on the branch, inspect
`oberth run resources --run <id>`, compare the captured values with the declared
values, then update the workflow in the same reviewed commit. Use
`--include-failed` to inspect a failed branch run. Resource values in a fragment
must be changed in its owning repository, then repinned in the consumer.

```
oberth run resources --repo repo-01 --last 30
oberth run resources --repo repo-01 --ref any --last 100 --include-failed --yaml
oberth run resources --run <id> --include-failed --yaml
oberth run resources --repo repo-01 --burn step-abc12345 --step step-abc12345 --yaml
oberth run resources --repo repo-01 --last 30 --anonymize > usage.json
```

`--anonymize` replaces repository, ref, burn and step names, commit SHAs and
run IDs with keyed hashes (`repo-…`, `step-…`) so usage data can be shared.
The CLI draws a fresh random salt for each export and never prints it: names
stay comparable within one export, two exports cannot be joined, and a list of
likely names cannot be checked against the tokens. Measurements, flags and
advice text are unchanged; the report names no container image.

MCP agents use `run_resources` with the same arguments (`id`, `repo`, `ref`,
`last`, `burn`, `step`, `include_failed`, `format`). Authenticated HTTP clients
use `GET /api/run-resources` with those query parameters (the run as `run`), or
`GET /api/runs/{id}/resources`. Default output is JSON; `format=yaml` includes a
`paste_ready` mapping per suggested step. Copy its `resources` into that template
in git, not the surrounding report. Deadline advice is withheld; keep the
declared `activeDeadlineSeconds` until Pod lifetime has been measured; a
suggestion omits the deadline rather than reporting zero. See
[resource advice](api-compatibility.md#resource-advice).
Bearer token scope is checked through the same repo/run lookups as `run_get`
before raw usage or git declarations are read. Responses contain resource
numbers, identifiers and fixed explanations; never logs, commands, environment,
images, termination messages or image-pull details.

## Population and suggestions

Default selection is the last 30 **passed default-branch runs**, including
promotion CI runs linked to a promotion targeting that branch. The promotion
record, not a branch-name prefix, proves the target.
`ref=any` includes tags/releases and other branches; a specific ref selects that
exact ref without adding promotion refs. The response's `include_promotions`
identifies default selection. `last` is bounded to 1..100. `run` accepts the same full ID or unique 8+
hexadecimal prefix as `run_get`, is exclusive with `ref`, and can infer `repo`.
Failed/incomplete runs are excluded unless `include_failed=true`, including for
an exact run. This controls the displayed metric population, not the safety
population. Independently, the server checks up to **100 latest matching
terminal runs and 100 matching active runs**, before the passed-only filter, with the same repository,
exact-run/ref/promotion selection. `last` limits displayed runs; it cannot hide
a known recent failure, including an eviction while its Workflow is still running.
`safety_run_ids` and `safety_scope` identify that bounded check. Active-run overflow
or displayed runs outside this window refuse advice rather than
claiming an unbounded failure audit. Missing or expired measurements stay unknown. A step's own
memory peak can supplement a retained, exactly matched container attempt for
advice; other self-reported counters are not substituted.

Each row identifies the consumer repo, normalized burn and **template name**.
Retry suffixes and pod hashes are removed, and `frag-<hash>-<template>` normalizes
across repins. New samples capture the template and fragment owner from the
server-bound Workflow and its fragment lock. Legacy samples recover template
identity from the pinned workflow-controller pod naming scheme. A fragment owner that cannot be
proved from retained provenance remains unknown and suppresses suggestions.
The declaration snapshot comes from the newest selected run's exact tested SHA
and its resolved fragment pins, never the working tree or an arbitrary newer
branch. If git data is unavailable, history still works and declarations are
explicitly unavailable. The tool does not claim to describe an untested current
branch head.

Quantiles are across container attempts, not individual polls. Repeated samples
of one incarnation are deduplicated. Each metric reports its own count, and rows
show sample count, attempts, distinct run IDs and the captured time window.
Memory prefers the step's own cgroup `memory_peak`, bound through its termination
diagnostic to the same run, workflow, pod, main container ID and restart count.
It is self-reported advice, never an admission input. Otherwise memory requires
**at least four polls per attempt**. A higher external peak with four polls is
retained even when a step peak exists. Attempts lacking either source suppress
the entire suggestion, including when other attempts provide five good runs.
The `memory_step_peak_attempts`, `memory_sampler_attempts` and
`memory_insufficient_attempts` counts expose this coverage.

Memory and peak CPU are main-container peaks. Average CPU is cumulative externally
captured CPU time divided by the interval from container start to its CPU sample
timestamp (clipped to finish). `duration_ns` uses main-container start/finish,
excluding initialization, helper wait and Pod finalization. Ephemeral storage and
IO pressure are pod-wide, including helpers; shared PVC storage is excluded. Main-container IO read/write
bytes and pod PSI stall nanoseconds are reported separately. Unknown is never
zero. Sampled peaks remain lower bounds, including killed attempts.

Every JSON/YAML step reports `sizing_acceptance`: `status` (`sufficient`,
`insufficient`, `non_comparable` or `censored`), `sufficient`, `required_runs`,
`comparable_complete_runs`, `cohorts`, `censored_attempts` and explicit `reasons`.
The CLI and MCP use the same report. Suggestions require **five distinct
terminal runs with all required measurements together**: memory, average/peak
CPU, ephemeral storage, duration and known historical CPU/memory/storage limits.
Retries in one run count once. Separate partial histories for different metrics
cannot be combined into five complete runs.

Coverage is metric-specific. `memory_coverage`, `cpu_coverage` and
`storage_coverage` on captured usage count distinct source timestamps, with first
and last source times. The legacy aggregate `samples` count can grow when an
unrelated metric refreshes and cannot establish these counts. Existing rows
without coverage retain their numbers but do not acquire invented coverage.
External memory advice needs four memory samples through main finish, or the
existing exact-attempt step cgroup peak. CPU advice needs four CPU-counter source
times and a cumulative counter reaching main finish. Storage advice needs four
local-storage source times through the last container exit of a terminal Pod.
These are minimum coverage checks for provisional advice, not a guarantee that
polling captured a peak. Unknown terminal tails refuse advice. CPU averages keep
their captured-counter interval; they are never recomputed over a Pod deadline.

Nonzero/failed commands retain their measured maxima but do not prove completed
workload coverage. A bound Pod disposition is persisted independently of runtime
container IDs, so an evicted Pod can veto advice without any usage row or valid
main termination. Before listing Pods, the collector also retains a bounded
Workflow/node attempt inventory, including superseded retries. Pod UID is added
only after ownership validation; an absent or foreign Pod supplies no invented
identity, timestamp, failure cause or measurement. A successful retry cannot
discharge another node's missing coverage. Incomplete/offloaded node inventories,
legacy missing inventories and truncated reads remain unknown and refuse advice.
Ordinary uncensored active attempts stay outside completed measurements without
being treated as failed workloads. This exemption requires both the run and the
retained Workflow observation to be active: Workflow completion can precede node
settling, log collection and run finalization. A terminal Workflow with unfinished
exact attempt coverage refuses advice during that interval. An executed terminal step without retained usage or Pod
disposition is an explicit coverage gap. Skipped, unexecuted steps do not invent
measurements or failures. Ordinary still-running telemetry stays outside terminal
quantiles; known censor evidence is evaluated before that exclusion.

Comparable means the same normalized step and owning repository with unchanged
observed request/limit vectors and workflow size. Mixed conditions refuse advice;
unknown historical limits are never replaced by today's declarations. An explicit
zero limit means known unlimited; an absent limit means unknown. This comparison
cannot prove equivalent inputs or hardware, so operator review still matters.
Any limit-censored observation, OOM or deadline kill refuses the entire suggestion,
including reductions. External sampled peaks retain their lower-bound warning.

Named constants in `internal/measured/report.go`
implement these rules for both the tool and the offline table:

| Field | Suggestion |
| --- | --- |
| CPU request | p95 average cores + 25%, rounded up to 10m, minimum 10m |
| CPU limit | omitted |
| Memory request | p95 peak + 25%, rounded to MiB, minimum 64Mi |
| Memory limit | maximum peak + 50%, rounded to MiB, minimum 64Mi |
| Ephemeral request | maximum pod usage + 25%, rounded to MiB, minimum 64Mi |
| Ephemeral limit | maximum pod usage + 50%, rounded to MiB, minimum 64Mi |
| Step deadline | withheld: Pod active lifetime through finalization is not measured |

The table computes estimates before applying retained baseline floors. Because
cache warmth and equivalent workload inputs across revisions are not proved,
`advice_basis` labels every emitted suggestion **provisional**. Requests retain
the greatest captured historical/current request, and memory/storage limits
retain the greatest captured historical/current limit. A declared current
request can also raise its corresponding suggested limit. Missing baselines,
known unlimited memory/storage, and a Pod-local aggregate limit that differs
from the main-container limit refuse new restrictive advice. An unlimited
resource does not become a guessed finite cap. Retaining a declaration does not
certify that it is sufficient or measured. When a current declaration is available,
an omitted memory or storage limit is unbounded or unknown; historical finite
values cannot supply a new finite copyable limit. Omitted requests do not acquire
that limit meaning. Recheck after cache, mount, workload
or CPU-cap changes; a cache mount does not prove cold coverage or cache hits.

Each step's `deadline_no_suggestion` explains this timing gap, even with no
history or otherwise sufficient resource measurements. Neither `suggested` nor
`paste_ready` contains a generated `activeDeadlineSeconds`; the captured git
declaration remains visible under `declared`. Resource sizing acceptance applies
to resource advice and does not establish deadline sufficiency. Without the
resource-only capability, acceptance also records the compatibility refusal;
complete comparable run counts and numeric measurements remain unchanged.

The retained `PodStartedAt` to `PodFinishedAt` span includes initialization and
helper execution, but `PodFinishedAt` is the last container exit observed on a
terminal Pod. It is a lower bound, not the time finalization completed. A Pod can
fail its deadline after all containers exited successfully. Neither this span
nor main runtime plus a fixed allowance establishes the complete active lifetime.
Deadline-killed samples are censored and cannot justify a reduction. Missing
timing stays unknown; no zero or fallback deadline is generated. Measuring final
Pod disposition is required before restoring numeric deadline advice.

The median is displayed for comparison and never drives suggestions. Request
margin allows ordinary variation; limit margin allows bursts missed by 15-second
sampling. These margins are advice, not proof that the next run fits. A single
exact run can be compared but cannot alone establish a suggestion.

No suggestion is returned for insufficient history, OOM, a deadline kill, a measured peak at or
above a recorded limit (censoring), saturation, counter resets, changed requests,
restarts, ambiguous identity or truncated history. Unknown historical CPU,
memory or ephemeral limits also suppress advice. Deadline-killed durations are
censored, including workflow-wide deadline kills with no precise step diagnostic.
Run/step deadline uncertainty conservatively refuses advice through
`deadline_unknown`, independently of `deadline_killed`. A successful completed
container stays a normal sample when another step or retry hits a deadline.
Only an exactly bound container diagnostic, timed-out step result or Pod
disposition establishes a deadline kill; the final display row cannot label
every retry. Unknown deadline attribution cannot produce a finite suggestion. Independent dispositions
preserve exact Pod identity. Inspect coverage before committing a change. No
automatic higher retry is performed.

## Bounds and retention

The tool reads through the database read pool, not the writer. Run selection uses
existing repository/ref indexes, then primary-key run lookups of telemetry and
step diagnostics. Display selects at most 100 runs; safety selects at most 100
terminal plus 100 active runs. Each reads at most 10,000 main incarnations.
Safety also bounds Pod dispositions, executed step inventory and attempt rows at
10,000 rows each; missing or truncated safety coverage refuses
suggestions. No cluster query occurs on this read path. The storage
operation has a five-second deadline and the entire service call ten seconds,
including waiting for repository read locks. The query-plan test explains the
exact executed observation SQL and verifies run-ID primary-key lookups.
It takes no write transaction. Optional coverage lives in versioned observation
JSON, and old rows remain readable. An additive timestamped database migration
stores independent Pod dispositions, bounded to 2,048 Pods per run and deleted
with run retention. A further additive timestamped migration stores the attempt
inventory marker and at most 4,096 retained nodes per run, with at most 2,048 nodes
in one collector snapshot. The marker records visibility and terminal Workflow
observation, not metric sufficiency. Failure and bound Pod identity survive late
or duplicate snapshots; dropped retry nodes are retained. These tables also
cascade with run deletion. They do not store raw Pod messages, commands or environment.
Every retained main observation and Pod disposition is reconciled with its full
Workflow, namespace, node, Pod and invocation binding. A contradiction refuses
advice for the measured invocation too, even if the conflicting inventory names
another burn or template. Current writers reject definite binding conflicts in
either order; the reader also fences inconsistent records retained by older code.
Use the normal pre-upgrade backup/restore process for database rollback; this
does not require older CLI clients to understand the new fields.

Retention currently targets 90 days and **100,000 container incarnations globally**,
with bounded asynchronous pruning. It has no per-repo or rare-release quota.
Helpers consume that global budget too, and busy repos may evict rare release
history before 90 days. Requesting 30 runs does not guarantee 30 measured runs;
the reported population and time window are the actual retained coverage.

The offline [comparison table](measured-requests-table.md) is generated by
`go run ./hack/run-resources-table --input ./step-usage-anon.jsonl.gz` using
the same `measured.Build` and suggestion code. Its declaration proxy is the newest
captured request/diagnostic values, explicitly marked; this export does not
contain every repository's git source or fragment lock.

Resource reporting is read-only. [Admission](native-synchronization.md)
uses the concurrent-workflow ceiling, enabled hard PSI valve and configured disk
floor. Kubernetes places step Pods by their requests; Oberth has no serving size
weight or declared CPU/memory budget. Argo deadlines still include Pending time,
and Kubernetes supplies neither workflow gang scheduling nor FIFO aging. These
limits remain visible in the product contract and installation qualification.

The offline comparison table reads only the anonymised export. Examples use
`repo-01` and `step-abc12345`. That export omits container names and incarnation
IDs, and its numeric termination data is not guaranteed to identify the same
attempt. Ambiguous rows get no suggestion; no identity or pairing is inferred
from array order. Per-repository and per-step labels in that table are the
supplied anonymised labels only.

A Pod deadline after main has already failed with an ordinary exit code, or
with its own OOM reason, does not relabel that failure as a deadline kill.
Bound deadline evidence also needs a compatible main-container termination;
exit137/143 or a signal by itself is not deadline evidence. Uncertain attribution
still refuses advice, and OOM censoring remains independent.
