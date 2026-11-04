# Run list (#1478)

The dashboard uses four uniform columns: status, run, when and message. SHA and
actor are available on hover and in run details. Running elapsed times tick each
second, with local start time beneath; earlier starts include the date. Queued
rows leave when empty. Cancelled and superseded runs show only their timestamp.

The server supplies one concise message for dashboard, CLI and MCP. Ordinary
running rows are empty; queue messages name required CPU and memory, with at
most one host-pressure or visible-blocker qualifier. Failures show a step or
Oberth-classed cause. Full errors, priority provenance, resource coverage advice
and detailed scheduling observations remain in run details. Statuses and run
ordering are unchanged. No migration is needed for this presentation change.

Passed rows also show `closes #N` / `refs #N` from the pinned commit's Git
trailer block, optionally followed by a schedule name. Prose is never matched.
The timestamped migration `20261104160000-run-issue-trailers` adds immutable
per-run display metadata; old runs remain unknown. Displaying a trailer never
closes an issue: closure still requires its existing repository policy and
green default-branch publication.
