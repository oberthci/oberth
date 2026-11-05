# Historical weighted-admission wait analysis

This document describes the retired weighted gate and its compatibility tests.
It does not describe current serving admission or recommend current capacity
settings. See [workflow admission](../declared-admission.md),
[host pressure](../io-pressure-admission.md) and
[execution priority](../release-priority.md) for current behavior.


With `jobs=4` and `M=2`, the weight budget is 8. An XL with weight 8 needs
**every existing grant to release** before it can start, then occupies the
whole budget for its entire run. Keeping these weights and this budget makes
XL exclusive: small runs cannot run beside it.

The relevant measure is **time until the last blocking holder releases**,
including any newly admitted holders. Saving weight-minutes on a holder that
already finishes earlier does not advance the XL. Optimize the drain's critical
path: slow tests, repeated downloads, startup gaps or retries in the last holder.
The scheduler already releases weight before slow finalization/publication once
the Workflow is terminal or its deletion has been accepted.

Bounded backfill can use spare capacity before `maxHeadWait`, normally
**20 minutes**. At the limit, new backfill stops and existing grants drain for the head.
After five additional minutes (25 minutes with defaults), available-capacity
backfill and claims resume without a p95 promise. The head remains first and
every live grant still counts, so this can extend its wait. Pressure, groups,
VM capacity, numeric, weight and run ceilings still apply. The durable first head-wait origin survives
restarts and priority displacement. Zero `maxHeadWait` keeps strict FIFO.

The scheduler projects the earliest head start by releasing enough running
reservations at their measured p95 finish times. A measured candidate must fit
now and finish strictly before the earlier of that reservation and the remaining
head-wait window. If running durations are unknown, a measured short candidate
may use existing capacity when it finishes within the window; unknown holders
are never projected free. Same effective and base priority classes may also
coexist if the head still fits alongside all surviving grants at its reserved
start. Lower classes cannot use coexistence, protecting higher-class waiters.
Unmeasured candidates use only proven coexistence, never a guessed duration.

Duration samples use the newest 30 passed runs of each repository's requested
workflow class: branch, release, promotion or schedule. Retained step
observations from `run_resources` supply whole workflow spans, extended by
known run startup/teardown. When a run has no retained observations, its durable
`StartedAt` and `FinishedAt` supply the sample if both form a positive duration.
At least five distinct complete run samples are required; fewer samples count
as unmeasured. Incomplete retained observations remain unusable, and truncated
history provides no estimate. These are p95 forecasts, not execution deadlines: an
overrun can still delay the head. An expired prediction stops new backfill
until the head starts or the drain limit expires; its predicted deadline cannot
move later while it remains head.
A head displaced by a different waiter gets a fresh forecast when it returns,
while retaining its original durable wait origin. The scheduler does not
preempt running work.

**Higher-priority waiters sort ahead of a standard-priority XL.** They can take
freed weight and extend the drain. An
admin bump can prevent lower-priority overtaking; standard work ages to `promotion`
after 60 minutes using its original FIFO position, never into `high`.
Existing holders and earlier highest-priority work still have to finish. See
[priority and aging](../release-priority.md).

Workflows with no scheduled workload Pod fail after **ten minutes** from
creation. Executor identity readiness has the same bound for each workload
Pod, measured from Pod creation, including later steps. Durable timestamps keep
these deadlines across restarts; actual Pod placement or retained Argo Pod-node
placement proves scheduling progress. The executor-identity holder is deliberately
SchedulingGated for its entire lifetime: it anchors credentials, runs no code,
and never counts as workload progress. Its gate must not be lifted.

Pod observation retries and each identity-delivery pass are bounded to two
minutes; individual Workflow reads are bounded to 20 seconds. The existing
shorter TokenRequest/exec retry limits still apply. Expiry uses existing failed /
infrastructure reporting with the reason in the run's message. Cleanup must
succeed (or a terminal result be observed) before releasing the reservation. If
the Kubernetes API refuses cleanup, the existing recovery path retains ownership
and budget; the drain cap never invents freed capacity.

For an incident, inspect the `system_status` blocker, `Draining`, holders and
priority order. Choose between waiting for the drain, cancelling the waiting XL
if it can be deferred so small runs can proceed, or cancelling disposable long
holders through normal cleanup. A bump helps when priority overtaking extends
the drain. Release only proven orphaned reservations; saturation alone proves
no orphan. These are operator choices, not actions performed by this report.

Allowing small runs beside XL requires a different capacity or weight policy:
for example, `jobs=5` gives budget 10 and leaves 2 beside XL=8. That changes
resource exposure and needs host/platform/VM/IO qualification; it is not a
recommendation to raise intake blindly.

Use `run_get` timing and read-only `repo_usage` to investigate the last holders.
The [#1216 measurements](../step-resource-usage.md#external-sampler-coverage-1216)
add containers, helpers, Pod overlap, storage and bound VM-launcher totals, but
sampled peaks remain lower bounds. Representative load, platform/independent
helper coverage and VM/storage attribution remain incomplete. Stage 2 is still
blocked: low main-container p95s do not justify lower resources or higher intake.
This report uses code and local tests; no live queue was inspected or changed.
