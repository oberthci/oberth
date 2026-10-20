# Issue locks and bulk closure

CI projection never takes or renews issue locks (#1170). These lifecycle tools
apply to explicitly acquired coordination locks, including retained legacy locks.

Read `issue_get`, `issue_get_many`, `issue_list`, or `/api/issues` to see an
issue's `lock`: owner, acquired_at and expires_at, or null. Reading does not
renew the five-minute lock. Conflicting mutations identify the lock owner and
expiry so an operator can decide whether to wait or intervene.

An administrator may call `issue_unlock {"id":123}` to break a lock early.
The audit entry `issue.lock.break` records the acting administrator and previous
owner. Scoped admins can unlock only visible issues. The former owner's next
explicit renewal fails; a later `issue_lock` call can acquire a new lock.

`issue_close_many {"ids":[123,124],"reason":"Superseded by the completed work"}`
closes up to 50 unique positive IDs per call. This tool is admin-only and
requires a nonempty reason of at most 4096 bytes. Each successful closure saves
the reason in the issue body and its own audit action. Results follow input
order, with `closed:true`, `error:"not_found"`, or `error:"lock_held"` plus lock
metadata. Hidden issues look missing. Existing active locks apply even to an
admin: unlock deliberately before retrying. Already-closed items succeed
without duplicating the reason or audit record. Unexpected storage failures
abort the call; earlier successful items remain closed and can safely be retried.

No schema migration, chart values or additional cluster permissions are needed.
