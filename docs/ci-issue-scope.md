# CI issues, manual tracking and read-only export

Oberth keeps its issues. The owner decision of 2026-11-03 supersedes the earlier
external-tracker retirement plan. Global/manual issue writes and repository CI
projection remain available; no external import or global read-only policy is
part of the laptop branch. The AI-workspace model, extra work statuses and stale
handling are owned by a separate input. This document does not claim those new
features are implemented here.

CI projection opens/updates repository failures and closes eligible green
results. Manual issues, scoped visibility, five-minute coordination locks,
lock-aware mutation, metadata revisions and transactional `expected_body_sha256`
checks remain. Existing `issue_append` behavior appends to the body; discussion
or workspace changes must follow the separately owned implementation. Pipeline
filesystem workspace isolation is a different subsystem.

## Read-only history export

Configure the normal Oberth CLI endpoint, CA and credentials for the scope to
be exported. Use a new private output path:

```sh
python3 hack/export-issues.py --oberth /path/to/oberth --output /private/path/manual-issues.json
```

The script calls only `issue_list`, `issue_get_many` and `issue_get`. It exports
both open and closed **manual** issues visible to that authenticated uplink.
CI records remain in Oberth; this file is not a database/audit backup. A scoped
uplink cannot export hidden upstreams. Before an installation-wide export,
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
tracking write needed in the archive. An archive whose body actually ends in a
truncation marker is deliberately refused as ambiguous; preserve such content
through the verified database backup instead of removing the marker.

This export is optional archival tooling. It does not select a tracker, mutate
an issue, restrict future writes or substitute for the verified database/audit
backup. Private archive contents must not be committed to git.
