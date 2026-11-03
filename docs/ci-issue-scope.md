# CI issue scope and staged tracking retirement (#1318)

Oberth issues support the CI repair loop: a red repository/branch projection
opens or updates an issue, an agent acquires a five-minute lock while fixing
it, and the eligible green result closes it. An agent can also discover a
repository defect before a red run exists. That belongs to the same repair
loop; general project plans, personal assignments and work journals do not.

The rejected workspace design is absent. Do not add work status, assignees,
due dates, waiting relationships, hierarchies, discussion comments or attention
sweeps. Pipeline filesystem workspace isolation is a different subsystem.

This audit describes the remaining legacy surface in the integrated v0.17.83 candidate. The candidate includes
atomic body-hash checks and metadata revisions from #1312; those compatibility
and concurrency fixes remain in place. **This change prepares export and migration; it does not
retire live issue writes or complete #1318.** Existing history must move before
features that depend on it become read-only. No history is deleted here.

## Feature decisions

| Existing feature | CI purpose and decision | Evidence / remaining change |
| --- | --- | --- |
| CI issue per repository/branch, repeat-red updates and eligible-green closure | Keep. These are the repair queue's source of truth. | `internal/service/ci_issue_reconciler.go`, `internal/store/issue_projection.go`; preserve the accepted-work sequence and origin guards. |
| Five-minute locks and admin lock break | Keep. Coordinate a repair without making reads acquire ownership. | `internal/service/issue_lifecycle.go`, `internal/store/issue_lifecycle.go`; retain scope, expiry and audit checks. |
| Repository-less manual issues, global or attached only to an upstream | Retire as a writable general tracker after migration. Upstream scoping alone does not associate a task with a repository. | `issueCreate` in `internal/service/api.go` and `CreateManualIssue` in `internal/store/issues.go` currently accept no repository. A later implementation must require a visible repository for new manual repair issues; old repository-less rows stay readable. Do not infer a repository from title text. |
| `issue_append` and journal-sized bodies | Keep bounded repair evidence for CI/repository repair issues; retire repository-less journals as writable history after migration. Link to exact run/step evidence instead of copying whole logs or continuing project journals. | `internal/store/issues.go` appends to the existing body. It is not a discussion thread. A later write-policy change needs explicit request/retained-body bounds and large legacy-record coverage; export must preserve those legacy bodies unabridged. |
| `fields`, `body_limit`, explicit truncation markers | Keep for reading repair evidence within a bounded response. These are transport controls, not work tracking. | `internal/service/mcp_compact.go` truncates only when requested and preserves UTF-8 boundaries. Export requests neither a field projection nor a body limit. |
| `issue_get_many` | Keep. An agent can retrieve a bounded set of CI failures without one network call per issue. | `internal/service/issues_bulk.go` applies visibility per issue and a 256 KiB response budget. `response_limit` is not a missing record; export falls back to full `issue_get`. |
| State/kind facets and body substring search | Keep bounded, scoped queue discovery: find repeated compiler/test signatures and distinguish unresolved CI failures from manual repair reports. | `ListIssues` in `internal/store/issues.go` keeps scope/repository filters in counts and pages. It currently scans matching bodies for substring search; this audit does not claim an indexed search or a scale fix. Repository-less history must eventually be an explicit read-only archive view. |
| `issue_close_many` | Keep as an administrator repair-queue cleanup operation when one verified change resolves several related records. | `CloseIssueWithReason` routes CI items through `closeCIIssue`, retains lock checks, requires a reason and records per-issue audit history. No implicit bulk transition is added to the normal green projection. |
| `issue_reopen` | Keep for an incorrectly closed repository repair report; retain the prohibition on manually reopening CI-generated state. | `internal/store/issues_reopen.go` already refuses CI issues. Later retirement must refuse changes to archived repository-less records as well. |
| Moving a manual issue between upstreams, including back to global | Retire this independent tracker operation after migration. A repair issue's visibility must derive from its repository, whose lifecycle is managed separately. | `issueUpdate` currently accepts `upstream`; no repository association is required. Removing this argument before migration could strand existing tracking. |
| `issue_delete` | Retire deletion of exported legacy history. Accidental new repository repair reports can be closed with evidence; deleting history is not a migration mechanism. | `DeleteManualIssue` currently removes a row. The staged read-only policy must cover this path and bulk/lifecycle entry points, not just `issue_update`. |

These are product scope decisions supported by existing behavior, not claims
that a particular installation uses every tool. The `expected_body_sha256`
transaction defect and broader DTO/error changes remain with #1312. This audit
does not duplicate that worker's implementation.

## Export before retirement

Configure the normal Oberth CLI endpoint, CA and credentials for the scope to
be migrated. Use a new private output path:

```sh
python3 hack/export-issues.py --oberth /path/to/oberth --output /private/path/manual-issues.json
```

The script calls only `issue_list`, `issue_get_many` and `issue_get`. It exports
both open and closed **manual** issues visible to that authenticated uplink.
CI records remain in Oberth; this file is not a database/audit backup. A scoped
uplink cannot export hidden upstreams. Before an installation-wide migration,
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
tracking write before cutover. An archive whose body actually ends in a
truncation marker is deliberately refused as ambiguous; preserve such content
through the verified database backup instead of removing the marker.

## Remaining cutover sequence

1. Select the external tracker and map every old issue ID to an imported record.
   Preserve the original state, timestamps, upstream/repository context and full
   body. Compare imported body hashes and counts, including closed history.
2. Confirm every scope and all writers are accounted for. Keep the immutable
   archive and verified history backup. The export itself is not import proof.
3. In a later reviewed change, make repository-less history read-only across
   create/update/append/close/reopen/delete, locks and bulk routes. Reject new
   unbound manual issues; bind new repair reports to an authorized repository.
   Preserve CI projection and its green-close behavior. Verify old database
   upgrades without dropping rows, tables or body history.
4. Remove the obsolete tracker write surface only after dependent clients have
   migrated. Retain access to archived history for the promised retention period.

The migration destination, import receipts and remaining scope coverage are
operator inputs. This repository change supplies the export mechanism and the
feature audit; it does not choose a destination or publish issues to one.
