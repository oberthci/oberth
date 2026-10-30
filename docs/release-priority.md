# Release priority and fairness

The scheduler computes queue priority from server-recorded run metadata.
Tag/release runs are `highest` when their stored ref kind is `tag` and they
carry the release flag or a `tag`/`release` trigger. They do not need a size
grant. Promotions are also `highest`. Administrator-configured RC branch
patterns are `high`; other work is `normal`.

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

Size grants now affect resource size only. Grant/revoke does not lower or
raise release priority, including authorized tag reruns. New size-grant audit
events retain repository/max-size/previous-max-size fields and omit the old
`tag_priority` and `previous_tag_priority` flags. Historical audit records are
unchanged. There is no database migration or new priority class.
