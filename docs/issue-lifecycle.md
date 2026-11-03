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
the reason in the issue body and its own audit action. CI issues use the same
guarded close path as `issue_close`, recording `issue.ci.close` with the branch
and resolution; manual issues record `issue.close`. Results follow input
order, with `closed:true`, `error:"not_found"`, or `error:"lock_held"` plus lock
metadata. Hidden issues look missing. Existing active locks apply even to an
admin: unlock deliberately before retrying. Already-closed items succeed
without duplicating the reason or audit record. Unexpected storage failures
abort the call; earlier successful items remain closed and can safely be retried.

The lock-break and bulk-close tools need no schema migration, chart values
or additional cluster permissions.

`issue_update.expected_body_sha256` checks the current body snapshot inside
the write transaction before any update or audit change. Body replacement and
`issue_append` change that hash; title, state and scope changes do not. It is not an atomic metadata revision
precondition.

Issue reads return `revision`, initially 1. `issue_update` accepts optional
`expected_revision`; the writer transaction checks it before metadata or audit
changes. A mismatch is a conflict. Omission keeps last-write-wins behavior.
Revision advances only when title, state, work status or scope actually changes.
Body and lock updates leave it unchanged; use the body hash separately for body
snapshots.
After a successful write, read the returned revision rather than predicting it.
Migration 41 adds the column and trigger. Upgrading from v0.17.82 advances
schema 39 through 48; rollback requires restoring the verified pre-upgrade
database and its matching identity/backup material. See the
[v0.17.83 upgrade notes](releases/v0.17.83.md#upgrade-from-v01782).

## Workspace work status and stale observations

Issues remain in Oberth. `work_status` is `todo` (the default for existing and
new issues), `active`, `waiting`, or `review`, independently of `state` (`open`
or `closed`). Set it with `issue_update`, optionally checking the current
metadata revision:

```json
{"id":123,"work_status":"active","expected_revision":7}
```

The CLI uses the same authenticated, lock-aware and audited MCP path:

```sh
oberth issues update --id 123 --work-status active --expected-revision 7
oberth issues --work-status active --json
oberth mcp call issue_update '{"id":123,"work_status":"review"}'
```

A real work-status change advances `revision`; repeating the same status does
not. A successful update refreshes `updated_at`, including an unchanged status
used as a checkpoint. Append substantive progress with `issue_append`.
CI repeat-red updates and green auto-closure keep their existing behavior and
retain the work status. Closing an issue does not change its work status.

`issue_get`, `issue_get_many`, `issue_list`, `/api/issues` and `/api/issues/:id`
return additive `work_status` and `stale` fields. Compact reads may select them
with `fields`. The dashboard displays both and can filter by work status.
`issue_list` and `/api/issues?work_status=active` apply the work-status filter
before pagination, totals, and state/kind facets. An omitted filter keeps the
existing all-status behavior.

An open issue is stale when the elapsed time since `updated_at` exceeds its
window. Closed issues are never stale. The server defaults are seven days,
and one hour for `active` issues. Configure them with `oberth serve
--issue-stale-after 168h --issue-active-stale-after 1h`. If only the general
server window is shortened below one hour, the inherited active window is
capped to match. An explicitly longer active server window is rejected.
Reads accept
`stale_after` and `active_stale_after` duration strings (HTTP query parameters
use the same names); the CLI list uses `--stale-after` and
`--active-stale-after`. Windows must be positive; the active window cannot
exceed the general window. A shorter general override also caps an inherited
active window. An explicit active override longer than the general window is
rejected. Empty or omitted values inherit the server policy. Reads compute
staleness without writing timestamps, advancing revisions, renewing locks,
or adding audit records.

Migration 50 adds the constrained work-status column and extends the metadata
revision trigger. It follows the unchanged policy-refresh migration 49. The
ledger stays contiguous; older binaries refuse the newer database. Rollback
requires the verified pre-upgrade backup and its matching identity material.
