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

A waiting higher class prevents lower-class backfill. Within a class the
existing bounded backfill still applies: the default 20-minute head-wait
window permits fitting work before draining for a blocked head; zero selects
strict FIFO. Repository concurrency groups, VM-capacity parking, disk-floor
holds and run-count/size limits still apply.

At 60 minutes, normal and high work join `highest` using their original FIFO
position. New releases can no longer leapfrog an older aged branch. This
bounds priority-based bypass, not elapsed time to execution: work already
holding resources and earlier highest-priority work must finish or be cleaned
up. A release can still wait when all capacity is in use, when older aged
work leads the highest class, or on its repository concurrency group.

An administrator can use MCP `run_priority` with a full run ID, priority `high`
or `highest`, and a reason to expedite an unadmitted run. The operation is
audited, cannot lower priority, and does not preempt running work.
`queue_priority_config` sets RC globs; `queue_priority_get` reports them and
the aging threshold. These are unscoped-admin capabilities.

A repository with no size-grant record defaults to automatic tag priority up
to size L. After exact checkout resolves size, an oversized tag (for example,
XL without an XL grant) uses normal order and the existing bounded backfill
window. Its admission/scheduling priority reason explains the withheld boost.
Tag execution still keeps its reviewed release.yaml size without requiring a
grant. Priority before checkout is provisional; durable queue positions do not
incorporate the gate's resolved size. Polling/admin refresh preserves the size
cap. If the size-limit lookup fails, automatic urgency is withheld at every
size, with a public reason and server warning (`reason=size_limit_unavailable`).
The release continues in normal order; a later successful refresh can restore
its eligible boost. This advisory lookup never fails release execution.
An XL grant permits automatic urgency at XL; an explicit admin bump and
60-minute aging are independent of this automatic size restriction.

Admin `size_revoke` / `oberth size revoke <repo>` disables automatic tag
priority as well as returning the size maximum to L. It works even if a grant
was never needed. Existing revoked records also opt out. `size_grant` with L
or XL re-enables the boost within that maximum. Local MCP grant/revoke wakes
and refreshes waiting work; in-pod CLI/external changes are polled within 15
seconds, with refresh deferred during DB contention. Neither revocation nor
an ordinary priority change preempts admitted work or disables promotions,
explicit bumps or starvation aging. The opt-out is durable in the existing
size-grant record; no migration or new priority class is needed. Audit events
include `automatic_tag_priority_enabled` and its previous value alongside size
fields. Historical audit records are unchanged.

A tag burst can still delay a promotion: they share `highest` FIFO, and another
tag can become the repository's oldest waiter as its predecessor is admitted.
The cap limits concurrent automatic boosts, not total burst duration. Release
priority also skips only the youngest 60 minutes of branch backlog; older
branches retain their aged FIFO place. After deployment, inspect `run_get`
priority reasons and the admission view on the next tag before attributing the
reported load incident entirely to priority classification.

An oversized tag that loses its boost still occupies its repository's oldest
waiting-tag slot until admission or termination. The next tag does not inherit
a boost while that older tag waits; it can age or receive an explicit admin
bump. See the README's pre-deploy grant checks for existing revoked records.
