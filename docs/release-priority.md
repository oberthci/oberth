# Release priority and fairness

The scheduler computes queue priority from server-recorded run metadata.
The oldest waiting tag/release run per repository gets automatic `highest`
priority when its stored ref kind is `tag` and it carries the release flag or
`tag`/`release` trigger. Later tags of that repository stay `normal`. Waiting
includes claimed runs until resource admission (`started_at`); admission or
termination releases the repository's automatic boost for its next tag.
Promotions are also `highest`. Administrator-configured RC branch patterns
are `high`; other work is `normal`. Explicit admin bumps and aging can still
raise any waiting run independently of the automatic tag cap.

Branch names, a branch's release flag, trigger spelling, pipeline annotation
or requested Argo priority cannot claim automatic release priority. The branch
transport records branch refs; the separate, ancestry-checked tag admission
path records tag refs. Pipelines cannot set queue priority. Only an unscoped
administrator can configure RC patterns or explicitly bump a waiting run.

Both durable claiming and weighted admission sort by effective priority,
then original admission sequence and queue sequence. The dispatcher can look
past ordinary waiting claims to expose higher priority work, with bounded
extra claims. This does not create extra weight, run permits or capacity.
Already admitted work is not preempted.

Backfill candidates are considered in priority/FIFO order while the head's
wait remains below `max_head_wait_seconds` (20 minutes by default). A measured
candidate must finish strictly before the earlier of the predicted head start
and the remaining head-wait window. If running durations are unknown, measured
short work may use existing capacity when its finish remains inside that window.
Work in the same effective and base priority classes may also coexist if the
head still fits with all surviving grants at its reserved start. Lower classes
cannot use coexistence and occupy room needed by higher-class waiters.
Unmeasured candidates can use only proven coexistence. At the head-wait limit,
new backfill stops and existing grants drain for up to five additional minutes.
After that cap, available-capacity backfill and claims resume without a p95
promise. All reservations still count and the head is considered first; this
can extend its wait. Zero selects strict FIFO and disables the escape.
Repository concurrency groups, VM-capacity parking, pressure, disk-floor holds
and run-count/size limits still apply. P95 predictions are estimates, not enforced
runtime deadlines; running work is never preempted.

Aging moves ordinary work up one class only. At 60 minutes a `normal` run without
an explicit administrator override becomes
effective `high`, with the reason `aged: normal run waited at least 60
minutes`; `high` work does not age further and nothing ages into `highest`,
so a backed-up queue cannot turn every run `highest` and empty the manual
priority line: promotions, the oldest waiting tag release and explicit
administrator `highest` bumps always precede aged work. Within a class the
order stays FIFO by admission sequence, and the aged run keeps its original
position, so every `high` run accepted after it waits behind it. Against
`high` arrivals its wait is therefore bounded by 60 minutes plus the `high`
work accepted before it. Aging never lifts a run above `highest`, so sustained
`highest` arrivals (promotions from any identity holding a promote grant, tag
releases, explicit admin bumps) can extend the wait of aged and `high` work
without limit; before this change aged work joined `highest` FIFO instead.
Existing resource holders must finish or be cleaned up, and repository
concurrency groups still apply.

An unscoped administrator can use MCP `run_priority` with a full run ID or unique
8+ character hex prefix and a required reason (1–512 bytes). `high`/`highest`
retain their raise-only rules. `normal` and `low` pin a run awaiting admission
at the selected class, including a tag or promotion that normally gets automatic
urgency. An explicit `normal`/`low` override never ages: it stays at that class
until another administrator change. Ordinary normal work still ages into high.
Queue order is highest, high, normal, low, with FIFO within each effective class.
The existing priority reason line shows the operator's reason; the atomic
`run.priority` audit record includes actor, previous priority, selected priority
and reason. Overrides survive restart and automatic refire. Non-admin rules are
unchanged. Already granted/running work cannot be preempted.

For example:

```sh
oberth mcp call run_priority '{"run_id":"01234567","priority":"low","reason":"Let ordinary work proceed"}'
```
`queue_priority_config` sets RC globs; `queue_priority_get` reports them and
the aging threshold. These are unscoped-admin capabilities.

A repository with no size-grant record defaults to automatic tag priority up
to size L. After exact checkout resolves size, an oversized tag (for example,
XL without an XL grant) falls back to an admin override, then an accepted
submission priority, then normal order. All heads use the same measured backfill
rule. Its admission/scheduling priority reason explains the fallback.
Tag execution still keeps its reviewed release.yaml size without requiring a
grant. Priority before checkout is provisional; durable queue positions do not
incorporate the gate's resolved size. Polling/admin refresh preserves the size
cap. If the size-limit lookup fails, automatic urgency is withheld at every
size, with a public reason and server warning (`reason=size_limit_unavailable`).
The release keeps any explicit priority or continues in normal order; a later
successful refresh can restore its eligible boost. This advisory lookup never fails release execution.
An XL grant permits automatic urgency at XL; an explicit admin bump, accepted
submission priority and 60-minute aging are independent of this automatic size restriction.

Admin `size_revoke` / `oberth size revoke <repo>` disables automatic tag
priority as well as returning the size maximum to L. It works even if a grant
was never needed. Existing revoked records also opt out. `size_grant` with L
or XL re-enables the boost within that maximum. Local MCP grant/revoke wakes
and refreshes waiting work; in-pod CLI/external changes are polled within 15
seconds, with refresh deferred during DB contention. Neither revocation nor
an ordinary priority change preempts admitted work or disables promotions,
explicit bumps or aging. The opt-out is durable in the existing
size-grant record; no migration or new priority class is needed. Audit events
include `automatic_tag_priority_enabled` and its previous value alongside size
fields. Historical audit records are unchanged.

A tag burst can still delay a promotion: they share `highest` FIFO, and another
tag can become the repository's oldest waiter as its predecessor is admitted.
The cap limits concurrent automatic boosts, not total burst duration. Release
priority is `highest` and precedes aged branch work, which never leaves `high`;
within `high`, aged and RC branches share one FIFO. After deployment, inspect `run_get`
priority reasons and the admission view on the next tag before attributing the
reported load incident entirely to priority classification.

An oversized tag that loses its boost still occupies its repository's oldest
waiting-tag slot until admission or termination. The next tag does not inherit
a boost while that older tag waits; it can age or receive an explicit admin
bump. See the README's pre-deploy grant checks for existing revoked records.

Submission priority may only raise the automatic class: natural `highest`
(promotion or eligible oldest tag), admin override, accepted `high`/`highest`
request, RC pattern, then normal. Refused, invalid and `normal` requests leave
that classification and its reason intact; refusals appear in `priority_refusal`.

Accepted priority, admin bumps and automatic RC/tag/promotion runs are protected
from ordinary branch supersession. Every push matching an RC pattern queues
independently, so repeated RC pushes all run. Accepting priority cancels nothing
unless the push explicitly includes `-o oberth.supersede=true`. That option is
honoured even on a priority push, inside the same branch and schedule entry;
protected work may be replaced only by its requester or an unscoped admin.
Promotion attempts and pending publications keep their separate guards.

`schedule_trigger force` bypasses the busy/paused check. A forced normal run
replaces only unprotected runs in its branch and entry; protected runs remain,
and a priority schedule submission queues independently. Force does not grant
protected supersession. Cancellation still requires the existing admin/force
permissions for tag, release, promotion and concurrency-group work; being the
priority requester adds no admin authority.

Restart requeue, trusted preflight recovery and eligible one-shot infrastructure
retry preserve protected work's priority and admission sequence alongside newer
normal siblings. Recovery can replace only its own queue place and preserves
all existing retry, publication, cleanup and execution-evidence checks.
