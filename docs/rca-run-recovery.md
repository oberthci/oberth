# Pre-step recovery and the run list

A branch run that fails from infrastructure before any pipeline step starts can
receive **one** automatic retry at the same SHA. This addresses transient
workflow creation, source seeding, executor identity delivery, and pre-step
workflow deadline failures. Release/tag, promotion, plan and apply execution is
excluded. Normal step failures are never automatically replayed.

The scheduler checks every retained progress attempt, the engine's step results
and earlier Workflow attempts, and terminal observations. It does not use the
run's StartedAt as a step-start indicator: that timestamp means resource
admission. Missing progress files contain no emitted records; malformed, torn,
or otherwise unreadable progress refuses automatic retry. Any known step start,
completion or non-queued result refuses it. Explicit hard engine validation
errors also refuse it. Later executor-identity failures preserve already
observed Workflow execution evidence instead of discarding it.

A failed workflow-create attempt must pass ownership-checked deletion before
recording a retryable failure. Observation errors first attempt terminal result
recovery; a real terminal result takes precedence. If no terminal result can be
read, the existing ownership-checked deletion must succeed. Failed cleanup keeps
the original run owned and its admission retained; no replacement is queued.
This uses the existing engine cancellation contract (accepted ownership-checked
deletion), not a new Kubernetes termination protocol.

The writer transaction records failure, creates the retry, links both attempts
and appends `run.retry.auto` atomically. It rechecks that the source is newest for
its branch/schedule entry, has no active sibling, failed step, persisted step
execution, pending deletion or publication, and belongs to an active repository.
A newer push wins instead of being superseded backwards by retry. The queued
copy resets credential admission and re-earns grants through the ordinary path.
It preserves source, schedule entry, actor and queue priority. No arbitrary build
step is replayed from stderr or a client-side timeout.

The one-retry marker survives restart requeue and manual retry/rerun. Human
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
JSON fields and ordinary run-list behavior remain available.

## Dashboard history

The dashboard defaults to the latest run for each `(repository, ref kind, ref)`.
The optional HTTP parameter `latest_per_ref=1` asks `/api/runs` for this view;
it accepts the ordinary `all` view only. Each head has `earlier_attempts`, the
count of all retained older attempts for that exact group. The server ranks
heads before applying the cursor, so an older page cannot resurrect an earlier
attempt as the current head. Counts and heads honor the actor's repository
scope. Existing requests without the parameter keep their flat list and paging.

Click `n earlier attempts` to load history for that ref, with further history
paged in batches of 100. Superseded attempts are hidden by default; `show
superseded` includes them in expanded history. Turn off `latest per ref` for the
flat historical list. These preferences persist in localStorage. History data
is cleared on token changes and repository selection, and late responses obey
the existing route/auth generation checks. Filters apply to current heads in
collapsed mode, rather than counting an old failure as a current branch failure.
The earlier-attempt count includes hidden superseded records, so it can exceed
the number of expanded visible rows.

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
