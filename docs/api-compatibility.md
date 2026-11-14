# HTTP and MCP compatibility

Oberth is pre-launch: there is no backward-compatibility promise. Field
names, tool arguments and envelopes change or disappear without aliases or
deprecation windows, and each removal is listed as a breaking change in the
release notes. This page describes the current wire shape.

## Record fields

Typed public records in HTTP JSON bodies and MCP structured content use
snake_case field names: a run returns `id`, `repo_id`, `queue_sequence`,
`status` and `sha`, and a full run response returns `run`, `steps` and
`repository`. Integers keep their full JSON integer precision. JavaScript
clients should use an integer-preserving decoder for IDs above 2^53.

Go-style names (`ID`, `RepoID`, `Steps`) are never sent, and no request
header or query parameter brings them back. The bundled CLI decodes the
snake_case names only. Go programs that decode responses into untagged
records (`model.Run`) can use `internal/recordjson.Unmarshal`, which maps the
snake_case names back to the Go field names.

Names already written in snake_case retain their spelling. A name a record
declares in lowerCamel (`activeDeadlineSeconds`) is sent under its snake_case
spelling (`active_deadline_seconds`) only. Dictionary keys,
such as step names, labels and resource names, are data and are not renamed.
Wire names are generated after visibility checks and redaction, from fields
already exposed in the response. Private storage fields remain private.

A run record carries its identity as `id`; responses about a run (logs,
artifacts, cancel, rerun, live logs) carry it as `run_id`. A promotion's `id`
identifies the promotion and its `run_id` the related run. Publication
responses keep their distinct `publication_id`, `promotion_id` and `run_id`.

Every tool that selects one run takes it as `id`: `run_get`, `run_logs`,
`run_cancel`, `run_rerun`, `run_hold_list`, `run_hold_exec`,
`run_hold_patch`, `run_continue`, `run_hold_abandon`, `run_purge`,
`artifacts`, `artifact_get`, ID-based `wait`, `run_wait`,
`run_priority`. No other spelling is accepted.

## Collection envelopes

HTTP collections are objects, the same shape MCP returns:

| HTTP route | Collection field |
| --- | --- |
| `/api/runs` | `{"runs": [...]}` |
| `/api/repos` | `{"repositories": [...]}` |
| `/api/grants` | `{"grants": [...]}` |

Run paging also returns `X-Oberth-Next-Before`. The object includes
`next_before` for an unscoped sequence cursor or `next_cursor` for a scoped run
ID, matching `run_list`. Follow the returned cursor; do not derive it from the
last record. No global sequence is added for scoped callers. Empty collections
are arrays (`[]`), not null. Existing object responses retain their envelope:
`/api/issues` returns `issues` and `next_before`, matching the names in MCP
`issue_list`.

Default MCP text and structured JSON carry the same snake_case names. Tools with a
specialized text format, such as logs or YAML reports, keep that existing text
format; their structured records receive the additive fields. JSON-RPC framing
is unchanged.

## Conditional requests

Every `200` JSON view carries a weak `ETag`, the digest of its uncompressed
body. A `GET` that sends it back in `If-None-Match` gets `304 Not Modified`
with no body while the view is unchanged; other headers, such as
`X-Oberth-Next-Before`, are still sent. `GET /api/repos?view=summary` returns
the repository records without their latest runs. The dashboard uses both
while polling.

## Removal policy

Removed names, arguments and envelopes are not kept as aliases. A release that
removes one lists it under breaking changes in its release notes; clients
update with the server. Clients must tolerate additional object fields.

## Issue-list privacy exception

`GET /api/issues` and `oberth issues --json` now return summaries. They omit
`Body`, `ScopeUpstreamID` and `CIWorkSequence`; body text remains available
from authenticated `GET /api/issues/{id}` and `issue_get`. These removed fields
are an explicit v0.17.x privacy exception to the additive compatibility policy.
The list includes no discussion fields or discussion API.

## Resource advice

Resource history does not derive a Pod deadline from main-container runtime.
Pod initialization, helper execution and finalization are not captured by that
runtime. Numeric `activeDeadlineSeconds` advice is withheld until complete Pod
active lifetime is measured; a suggestion omits the deadline (absent, never
zero) in JSON and in copy/paste YAML. Declarations and actual resource
measurements remain available.

Coverage counts, `advice_basis`, `safety_run_ids` and `safety_scope` are additive
fields. A passed-only metric view still consults bounded retained failure
evidence, and incomplete or missing executed-attempt coverage can return
`suggested: null`. Existing numeric measurements remain available. Suggestions
that are emitted retain known resource-bound floors and explicitly remain
provisional; no cold-run safety guarantee or numeric Pod deadline is introduced.

The safety population also includes bounded active Workflow evidence. Ordinary
unfinished telemetry is excluded from completed quantiles, while known failures
and missing attempt coverage still refuse advice. Independent attempt inventory
is stored through an additive migration; it adds no caller argument or run status.
An omitted limit in an available current declaration cannot be replaced by a
finite historical limit.

Compact run steps are paged. `run_get` returns at most 100 Argo nodes per
summary, with `step_count` and, when more nodes remain, `next_step_offset`.
Pass that value as `step_offset` on the next read; `step_limit` may reduce the
page size to 1–100 (0 uses the default of 100). Pages follow the current stored
node order, so offsets can shift while Argo adds nodes to a running run.
`summary:false` returns the full node history. Node messages have terminal
escape sequences and control characters removed and are limited to 512 Unicode
characters. Passed nodes omit messages in compact summaries. Frozen grant and
replay evidence stays internal and is omitted from run responses.

Native process exit codes are omitted when Argo has no valid exit value. A known
zero remains a reported zero. Historical stored exit codes remain readable.
