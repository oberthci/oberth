# Pre-step recovery and the run list

A branch run can receive **one** automatic retry at the same SHA for an
allow-listed infrastructure failure with positive proof that no Workflow POST
occurred. Eligible source-checkout deadlines and native preparation failures
after a definitive Workflow NotFound can qualify. Permanent configuration,
policy, malformed pipeline and missing-SHA failures do not. Release/tag,
promotion, plan and apply execution is excluded.

Only fresh dispatch initializes an explicit empty progress journal, before
execution; it never truncates existing evidence. A missing journal in any later
check is unknown and refuses replay. Every retained progress attempt, engine
step/attempt result and terminal observation still participates in the guard.
Run StartedAt means admission, not step execution. Started/completed/non-queued
steps and unreadable/torn history refuse retry. Source preparation must also
carry a trusted infrastructure classification; an unclassified error is not a
retry authorization.

After a Workflow create attempt, only the native engine's positive pre-POST
proof can opt in. Submission-record failure, observed/completed-while-down
Workflows and uncertain-create settlement never request automatic retry, even
with an empty journal. Post-create workflow deadlines preserve their original
job phase and do not auto-retry. Partial executor observations retain step-start
evidence without collecting artifacts from a live workspace; artifact collection
requires an actually terminal Workflow.

Definite rejection and foreign-name refusal never route through Delete. An
uncertain POST uses v0.17.72's shared create-settlement worker. A failed delete
after successful creation/submission-record failure also transfers to that
worker when the engine supports settlement. Its run and admission stay retained
while Kubernetes is unavailable; unrelated runs can progress and the worker's
error never exits the scheduler. Only confirmed cleanup settles it, without an
automatic execution retry. Existing ownership checks remain mandatory.

The writer transaction records failure, creates the retry, links both attempts
and appends `run.retry.auto` atomically. It rechecks that the source is newest for
its branch/schedule entry, has no active sibling, failed step, persisted step
execution, pending deletion or publication, and belongs to an active repository.
A newer push wins instead of being superseded backwards by retry. The queued
copy resets credential admission and re-earns grants through the ordinary path.
It preserves source, schedule entry, actor and queue priority. No arbitrary build
step is replayed from stderr or a client-side timeout.

The one-retry marker survives restart requeue, KVM preflight restart recovery
and manual retry/rerun. Human
reasons carry its fast read-side projection; immutable `run.auto-retry-lineage`
receipts preserve it if cancellation/supersession replaces the human reason.
An explicitly requested manual replay is still allowed by its existing policy,
but does not reset the automatic budget. A separate new push is independent.
Retry creation is atomic with failure, so failed audit/enqueue writes leave no
partial retry or amber claim. Transient first-attempt infrastructure failures
that were retried do not create a CI-red issue; an exhausted attempt retains the
normal issue policy.

The original attempt keeps its durable failed Status and original Error. New
optional `auto_retry_of` and `auto_retried_by` JSON fields expose the links.
Its presentation status is `infra-retried`, with an amber `infra error, retried`
label and a link to the retry. An exhausted retry can still be red. Superseded
attempts keep their durable interrupted Status but have a distinct
`display_status: superseded`, separate from intentional cancellation. Existing
Raw status fields and ordinary run-list behavior remain available. Existing
`display_status` values change for affected rows: `cancelled` becomes
`superseded`, and an automatically retried `failed` row becomes `infra-retried`.
Clients should accept those display enums. `auto_retry_of` can also describe
spent-budget lineage on a manual replay; audit actions identify whether a
particular execution was automatic.

## Dashboard history

With the `all` status chip selected, the dashboard lists the latest run for each
`(repository, ref kind, ref)`. It also fetches the existing unpaged
`/api/runs?view=active` view, scoped by repository when selected, on **every**
page. Any active run missing from that page's heads appears first, without
duplicating a head. Thus a running nightly hidden by a newer push, work in
another schedule lane, a pending publication, and active heads outside the
current page remain visible. Status and release filters still apply to the
combined rows. The active endpoint retains its existing server bound of 500;
the UI adds no further active-run limit.

Selecting any status other than `all` requests the **flat retained list**, then
filters its loaded page. In particular, `failed` can find a finished nightly
behind a newer successful push, while `superseded` and `infra-retried` can find
non-head attempts. Page through older results when needed. Changing the status
chip resets pagination; it does not mistake a head-view cursor for a flat-list
cursor. The quiet footer under the table names the current view and page.

The optional HTTP parameter `latest_per_ref=1` asks `/api/runs` for the grouped
view; it accepts the ordinary `all` view only. Each head has `earlier_attempts`,
the count of all retained older attempts for that exact group. The server groups
a narrow covering index into exact heads, then applies the page cursor to those
heads before fetching selected wide rows. An older page cannot resurrect an
earlier attempt as the current head. Counts and heads honor the actor's
repository scope. Existing requests without the parameter keep their flat list
and paging.

Open an individual run and select **Run history for this branch** or **Run
history for this tag**. History includes superseded runs, excludes the current
run, and matches its exact repository, ref name and ref kind; an older run's
history can include newer runs too. Older results load in batches of 100 with
the server's cursor. Open history refreshes page one on detail poll or refresh,
merging fresh records by ID while retaining already loaded older rows. Unchanged
history keeps its DOM, keyboard focus and scroll position; updates also restore
focus and the table's horizontal scroll. Older cached rows outside page one are
refetched when history is reopened. History waits for pending repository context
before choosing a qualified selector for shared names. Token changes and
repository selection clear history; collapse, replacement requests and auth
generation checks discard late responses.

List and history rows show only repository and ref in the run column, without
priority, release, schedule-entry or concurrency-group badges. Existing message
text takes precedence; an empty message falls back to `release` for a release
run, otherwise `schedule: <entry>` for a named scheduled run. Priority and group
appear in a queued run's message only through its existing wait reason. The
`release` filter remains, and run-detail badges are unchanged.

The old `latest per ref` and `show superseded` switches are removed; their saved
localStorage values are ignored. Status, release and repository choices still
persist. The repository sidebar shows retained-run counts with accessible labels
and tooltips; these count runs, not identities.

## Readiness under database stalls

The readiness watcher now gives its separate read-only SQLite connection a
**30-second** probe budget and a busy timeout at least as long. HTTP `/readyz`
reads the in-memory snapshot; it performs no DB IO. A DB operation that succeeds
within that budget publishes a fresh successful sample even if it was slow.

A deadline or typed SQLite BUSY/LOCKED result retains the last green sample
**without renewing its age**. A single load-related failure therefore does not
immediately remove the API Service endpoint. A last-good sample older than 90
seconds is unready, including while a sampler is stuck; repeated contention
cannot keep the pod Ready forever. No successful initial sample means unready.
Hard DB errors, absent upstream configuration, credential failures, cancellation,
and startup/shutdown recovery gates retain their fail-closed behavior.

Reader retention excludes the writer readiness signal: once the writer's own
90-second confirmed-contention policy reports failure, the next readiness
sample publishes it immediately instead of granting another 90-second window.
Sampler scheduling can still add its ordinary in-flight probe latency.
Liveness and writer progress budgets are unchanged. Readiness is routing
availability, not write authorization: database writes still run through real
transactions and current integrity/mutation gates. Admission, access, resource
and publication checks still execute at the operation. Retaining a routing
sample cannot authorize a stale write or bypass an admission guard. A DB stall
can make API calls slow or return their own bounded errors while keeping the
Service endpoint available.

Retained deadline and busy samples have separate counters
`oberth_readiness_retained_deadlines_total` and
`oberth_readiness_retained_busy_total`; the first retained transient event after
a successful sample is logged without exposing raw database errors. Snapshot
age metrics and readiness transition logs remain available.

## Schema and query cost

Schema **35** adds `runs_latest_ref_idx` on repository, ref kind, ref and descending
queue sequence. Exact head/count aggregation scans only that covering index;
wide historical errors are not window-sorted or read. Only selected heads fetch
wide rows. Queue positions retain the global priority order of the flat view.
Work still grows with retained history; this is an indexed aggregate, not a
constant-time maintained-head table. The benchmark includes 50,000 runs, 2,000
refs and 2KiB errors, plus an assertion on the actual query plan.

Upgrade applies the new index through the ordered migration ledger. v0.17.72
supports schema 34 and refuses schema 35. Rollback is **database restore only**:
restore a verified pre-upgrade schema-34 backup before starting the pinned old
binary/chart, with writers stopped and installation/witness continuity preserved.
Do not drop the index or edit migration-ledger rows to downgrade. A schema-35
backup is not a rollback backup for v0.17.72.

**Take and verify a fresh backup immediately before upgrading.** Record its
actual schema, audit head and current external witness state; retain a protected
off-volume copy and required restore keys. A restore loses all database changes
recorded after that backup. External witnesses ahead of the backup can refuse a
normal restore, so backup integrity alone does not establish operational rollback
permission. OpenBao secrets, grants, chart values, identities, logs and VM resource
ownership need their own preserved recovery state. Qualify the restore path on a
copy with the old binary before deployment; otherwise choose forward repair.
See [schema-35 restore qualification](schema35-rollback-qualification.md).

The schema-35 emergency restore ordering is defined in the
[v0.17.73 runbook](v073-upgrade-runbook.md). `oberth restore` with the new binary
does not downgrade it; the old chart must first be staged without starting a
Pod. The old chart rejects replicaCount=0, so use the rehearsed single-field
maintenance copy described there. Check the Rekor/pending-intent deadline before
replacing any database.
