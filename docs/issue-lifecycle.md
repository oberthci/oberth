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
Migration 42 adds the column and trigger. Upgrading from v0.17.82 advances
schema 39 or 40 through 55; rollback requires restoring the verified pre-upgrade
database and its matching identity/backup material. See the
[v0.17.84 upgrade notes](releases/v0.17.84.md#upgrade-from-v01782-or-v01783).

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

Migration 51 adds the constrained work-status column and extends the metadata
revision trigger. It follows the unchanged policy-refresh migration 50. The
ledger stays contiguous; older binaries refuse the newer database. Rollback
requires the verified pre-upgrade backup and its matching identity material.

## Commit trailers on green default branches

Automatic trailer processing is off by default. Opt in for one repository by
committing `.oberth/issues.yaml`:

```yaml
close_from_trailers: true
```

The setting is read from the commit that was published. An absent file or
`false` disables processing. Unknown settings, multiple YAML documents,
symlinks and oversized files are refused. There is no installation-wide
permission change.

After a successful default-branch push, or a successful promotion published to
that branch, Oberth reads every commit newly reachable since the previous green
published head. Failed, interrupted, cancelled, scheduled, tag and other branch
runs do not trigger this feature. Fast-forward promotions reuse the successful
source run as their evidence. Git identifies the final trailer block; prose
such as “this fixes #123” does not count. For example:

```text
Repair the cache lookup

Fixes: #123, #124
Refs: #125
Closes: #126
```

`Fixes` and `Closes` append evidence then close the issue; `Refs` only appends.
Keys are case-insensitive. Several comma- or whitespace-separated positive
issue IDs, repeated trailers and folded values are supported. An invalid value
is ignored as a whole. Close takes precedence when an issue has both kinds of
trailer on the same commit. Each referenced commit supplies one evidence line;
commits encountered after the issue closes add nothing:

```text
upstream/team/repository abcdef123456 run <run-id>: green on main
```

Only issues visible to a caller scoped to the repository's upstream can change:
repository-backed issues in that upstream and manual issues filed under it.
Workspace-global issues and issues from other upstreams are excluded. Closed
issues receive no evidence. An active lock, including one owned by `oberth`,
prevents changes and records `issue.trailer.locked` in the audit instead. Locked
work is skipped permanently for that publication; unlocking does not replay it.
The issue model has no structured set of repositories, so the first eligible
close trailer closes a cross-repository issue; repository names in prose are
not interpreted as policy.

Evidence and closure use the existing issue audit chain, with actor `oberth`,
the run ID, publication ID, full commit SHA and repository. Evidence, closure,
and the processing receipt commit together. Replaying a delivery or restarting
the server does not duplicate evidence. Pending work is recovered at startup
and on scheduler recovery ticks. Missing Git caches, unavailable objects and
other infrastructure errors retain the pending record. Recovery batches
interleave repositories, so one repository's failing rows cannot consume every
slot ahead of another repository.

Invalid policy (including unknown fields, multiple documents or a non-regular
file), malformed Git trailer framing (including embedded NULs) and byte-limit
overflows permanently skip that publication. An `issue.trailer.skipped` audit
entry records the reason, repository, full head SHA, run ID, branch and
publication ID with actor `oberth`. The skip audit and completed receipt commit
together; the issue remains unchanged and later ticks do not retry that input.
Fixing policy at a later commit does not replay a skipped range. Rejected YAML
content is not copied into the audit. This does not change the published run's
green verdict. Historical green deliveries seed
the boundary during migration 55 without replaying their trailers. With no
previous green delivery, the first green run scans all reachable history.
Disabled green deliveries still advance the boundary, so enabling the setting
does not replay older green commits. History output is bounded at 4 MiB and
policy files at 16 KiB; exceeding either skips the whole publication without
applying partial evidence.
