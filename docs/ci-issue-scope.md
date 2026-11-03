# Issue scope and workspace coordination

Oberth issues support both the CI repair loop and agent workspace coordination.
Manual issues and their history remain in Oberth. No external tracker, import,
export, or read-only cutover is required to keep using them. This scope replaces
the earlier staged tracking-retirement proposal.

CI eligibility is unchanged: default-branch and scheduled branch runs open or
update one issue per repository and branch, and an eligible green result closes
it. CI projection never acquires or renews investigation locks. Agents can
explicitly acquire a five-minute lock; scope, lock expiry, audit and mutation
checks remain in force.

Manual issues are workspace-global or filed under an upstream. A single-upstream
uplink files there by default; scoped callers see only permitted upstreams.
Upstream moves, append, close, reopen, accidental-manual-issue deletion, admin
lock break and bulk closure retain their existing authorization and behavior.
Manual issues are not inferred to belong to a repository based on their titles.

Work status is an independent `todo`, `active`, `waiting` or `review` field.
Agents can set it through MCP or the CLI. Open/closed remains the issue
lifecycle; CI repeat-red updates and green auto-closure retain work status.
An open issue with no recent update is reported as stale, with a shorter default
window for active work. Staleness is computed, never written as a mutation.
See [issue lifecycle](issue-lifecycle.md) for windows, overrides and examples.
Assignees, due dates, parent hierarchies, discussion threads and attention
sweeps are outside this change. Pipeline filesystem workspace isolation remains
a separate subsystem.

Reads retain bounded transport and scoped queue discovery: `fields`,
`body_limit`, explicit truncation markers, the 256 KiB bulk response budget,
state/kind facets, body substring search and descending-ID pagination.
Work-status filters apply before pages and all existing counts. These controls
do not add an indexed body search or change the CI accepted-work guards.

## Optional portable manual-issue archive

The read-only export tool remains available for an operator-requested portable
archive. Export does not move issues, change their ownership or retire writes.
It is not a replacement for the verified database/audit backup.

Configure the normal Oberth CLI endpoint, CA and credentials for the scope to
be exported. Use a new private output path:

```sh
python3 hack/export-issues.py --oberth /path/to/oberth --output /private/path/manual-issues.json
```

The script calls only `issue_list`, `issue_get_many` and `issue_get`. It exports
both open and closed **manual** issues visible to that authenticated uplink.
CI records remain in Oberth; this file is not a database/audit backup. A scoped
uplink cannot export hidden upstreams. For an installation-wide export,
account for all scopes explicitly using the appropriate authorized credentials.
An empty visible result does not prove the installation has no tracking history.

The JSON archive contains a format version, acquisition times, visibility
qualification, count, complete public issue records and a SHA-256 for each
UTF-8 body. Unknown response fields are retained. Private titles, bodies and
lock metadata belong only in the archive, not command diagnostics or git.
The file is created with mode 0600, published only after checks pass, and never
overwrites an existing path, including a symlink.

Pagination must move backwards with exact counts and no duplicate IDs. The
export refuses missing records, changing visibility/counts/revisions, truncated
bodies, malformed responses and exceeded limits. It re-reads metadata and full
content to detect same-second edits hidden by rounded API timestamps; transient
lock changes alone do not invalidate the content check. It preserves oversized
records through `issue_get` instead of silently accepting a partial bulk reply.
Limits are 10,000 issues, 128 MiB of encoded issue records and 120 seconds per
CLI call; reaching a limit fails the whole export. There is no prefix-success
mode and no automatic mutation to quiet the server.

Separate API reads cannot provide a database snapshot. Arrange a quiet window,
retain the ordinary verified history backup, and re-export after the last
write when a stable archive is required. An archive whose body actually ends in a
truncation marker is deliberately refused as ambiguous; preserve such content
through the verified database backup instead of removing the marker.
