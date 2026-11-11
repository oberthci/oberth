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
declares in lowerCamel (`maxNotStarted`, `activeDeadlineSeconds`) is not a
Go-style duplicate: it is still sent next to its snake_case spelling. Dictionary keys,
such as step names, labels and resource names, are data and are not renamed.
Wire names are generated after visibility checks and redaction, from fields
already exposed in the response. Private storage fields remain private.

A primary run record returns both `id` and `run_id`. Full run details also expose top-level `id` and `run_id`.
Run log, artifact-list, artifact, cancel, rerun, preflight and HTTP live-log responses
keep `run_id` and add `id`. A promotion's `id` still identifies the promotion;
its related `run_id` never replaces it. Publication responses keep their distinct
`publication_id`, `promotion_id` and `run_id` meanings.

The tools `run_get`, `run_logs`, `run_retry`, `run_cancel`, `run_rerun`,
`run_hold_list`, `run_hold_exec`, `run_hold_patch`, `run_continue`, `run_hold_abandon`,
`run_purge`, `artifacts`, `artifact_get` and ID-based `wait`
accept preferred `run_id` alongside legacy `id`. `run_resources` accepts
`run_id` alongside legacy `run`. If both spellings are sent, they must be equal;
conflicting selectors are invalid input. Alias resolution uses the same scope
checks and canonical run-ID resolver. HTTP resource-history queries also accept
`run_id`; path and query aliases must not conflict.

## Collection envelopes

Default HTTP array responses are unchanged. To request the matching MCP object
shape, append `envelope=1` to these HTTP GET routes:

| HTTP route | Opt-in envelope / MCP collection field |
| --- | --- |
| `/api/runs` | `{"runs": [...]}` |
| `/api/repos` | `{"repositories": [...]}` |
| `/api/grants` | `{"grants": [...]}` |

Run paging still returns `X-Oberth-Next-Before`. The opt-in object also includes
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

## Resource-deadline safety exception

Resource history no longer derives a Pod deadline from main-container runtime.
Pod initialization, helper execution and finalization are not captured by that
runtime. Numeric `activeDeadlineSeconds` advice is withheld until complete Pod
active lifetime is measured; declarations and actual resource measurements remain
available.

Older CLIs decode a missing suggested deadline as integer zero and copy it into
YAML. To keep server-first upgrades safe, HTTP and MCP callers receive
`suggested: null` unless they explicitly send `resource_only=true`. For otherwise
accepted histories, `no_suggestion` and `sizing_acceptance.reasons` explain that
a compatible resource-only renderer is required. Those callers still receive
the measurements, run identities, declarations and timing semantics. The
compatibility refusal does not mean the retained samples were removed.

`resource_only=true` is a caller capability: it promises to handle suggestions
without `activeDeadlineSeconds`, preserving absence rather than inventing zero
in JSON or copy/paste YAML. Updated CLIs send it automatically. HTTP clients use
the query parameter; MCP `run_resources` clients use the boolean tool argument.
The same sample sufficiency, comparability and censoring checks still apply;
opting in cannot force a suggestion or restore unsupported deadline advice.

Upgrade the server first; existing CLIs can continue reading measurements safely.
Then upgrade the CLI to recover resource-only suggestions. Custom integrations
may opt in after fixing their decoder and renderer. Older servers do not
implement this safety behavior; an upgraded CLI alone cannot correct advice
computed by an older server. This is an explicit v0.17.x safety exception to the
previous suggestion semantics, not a removal of measurement fields or a change
to execution resources.

Resource-only capability does not waive measurement or safety requirements.
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
