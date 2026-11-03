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
```

MCP agents use `run_resources` with the same arguments (`run`, `repo`, `ref`,
`last`, `burn`, `step`, `include_failed`, `format`). Authenticated HTTP clients
use `GET /api/run-resources` with those query parameters, or
`GET /api/runs/{id}/resources`. Default output is JSON; `format=yaml` includes a
`paste_ready` mapping per suggested step. Copy its `resources` and
`activeDeadlineSeconds` into that template in git, not the surrounding report.
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
an exact run. Missing or expired measurements stay unknown. A step's own
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
timestamp (clipped to finish). Duration uses pod container start/finish,
excluding Pod Pending time. Ephemeral storage and IO pressure are pod-wide,
including helpers; shared PVC storage is excluded. Main-container IO read/write
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
| Step deadline | maximum container duration + 50% + 60s startup allowance |

The median is displayed for comparison and never drives suggestions. Request
margin allows ordinary variation; limit margin allows bursts missed by 15-second
sampling. These margins are advice, not proof that the next run fits. A single
exact run can be compared but cannot alone establish a suggestion.

No suggestion is returned for insufficient history, OOM, a deadline kill, a measured peak at or
above a recorded limit (censoring), saturation, counter resets, changed requests,
restarts, ambiguous identity or truncated history. Unknown historical CPU,
memory or ephemeral limits also suppress advice. Deadline-killed durations are
censored, including workflow-wide deadline kills with no precise step diagnostic.
Inspect
coverage before committing a reduction. No automatic higher retry is performed.

## Bounds and retention

The tool reads through the database read pool, not the writer. Run selection uses
existing repository/ref indexes, then primary-key run lookups of telemetry and
step diagnostics. It reads at most 100 runs and returns at most 10,000 main
incarnations; truncation is explicit and suppresses suggestions. The storage
operation has a five-second deadline and the entire service call ten seconds,
including waiting for repository read locks. The query-plan test explains the
exact executed observation SQL and verifies run-ID primary-key lookups.
It takes no write transaction and makes no cluster calls. No SQL schema change
is needed: new optional provenance and limits live in the existing versioned
observation JSON, and old rows remain readable.

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

Stage A changes no admission, priorities or execution resources.
[Stage C admission](declared-admission.md) reserves declared peak CPU/memory,
retaining the size-class path for undeclared workflows. pod-only queuing remains a
later stage: the workflow controller workflow deadlines include Pending time, pod has
no FIFO aging/workflow gang guarantee, and a global Pending-pod cap and VM
deadlock policy need proof before handing it the queue.

The offline comparison table reads only the anonymised export. Examples use
`repo-01` and `step-abc12345`. That export omits container names and incarnation
IDs, and its numeric termination data is not guaranteed to identify the same
attempt. Ambiguous rows get no suggestion; no identity or pairing is inferred
from array order. Per-repository and per-step labels in that table are the
supplied anonymised labels only.
