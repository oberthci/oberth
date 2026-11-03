# Admission waits with XL weight 8 and budget 8

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

Measured backfill can use spare capacity before and after `maxHeadWait`,
normally **20 minutes**. The scheduler projects the earliest head start by
releasing enough running reservations at their measured p95 finish times.
A candidate must fit now and finish strictly before that reserved start, or
leave capacity for the head alongside it at that time. All other surviving
grants count, including earlier backfills. Lower effective or base classes
can use only the ends-before test, protecting every higher-class waiter ahead. An unmeasured candidate can use only the coexistence test;
unknown or already overrun running work is never assumed to release capacity.
Draining is capped at a further **five minutes** after `maxHeadWait` (25 minutes
of head wait with defaults). Once that cap expires, later work that fits current
capacity may backfill even without a usable p95 prediction. This bounds idle
capacity behind unknown or overrun holders; it can delay the head. The head is
still considered first at every release, and all actual grants still count.
Priority ordering, groups, VM capacity, pressure, disk and run limits remain in
force. Zero `maxHeadWait` keeps strict FIFO and disables this escape.

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

**Higher-priority waiters sort ahead of a normal-priority XL.** They can take
freed weight and extend the drain. An
admin bump can prevent lower-priority overtaking; normal/high work ages to
`highest` after 60 minutes using its original FIFO position. Existing holders
and earlier highest-priority work still have to finish. See
[priority and aging](release-priority.md).

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
The [#1216 measurements](step-resource-usage.md#external-sampler-coverage-1216)
add containers, helpers, Pod overlap, storage and bound VM-launcher totals, but
sampled peaks remain lower bounds. Representative load, platform/independent
helper coverage and VM/storage attribution remain incomplete. Stage 2 is still
blocked: low main-container p95s do not justify lower resources or higher intake.
This report uses code and local tests; no live queue was inspected or changed.
