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

Verified database/audit backups remain the history backup. The migration-only
issue exporter is removed. Body-hash conflict checks and append into the existing
body remain; the one-MiB request limit is not an aggregate stored-body limit.
