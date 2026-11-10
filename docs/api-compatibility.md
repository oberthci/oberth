# HTTP and MCP compatibility

Except for the issue-list privacy and resource-deadline safety changes below,
the v0.17.x API retains existing field names, tool arguments, value meanings
and default response envelopes. New clients should use the additive names below.
Older clients may continue using their existing names and array responses.

## Record fields

Typed public records in HTTP JSON bodies and MCP structured content use
snake_case field names: a run returns `id`, `repo_id`, `queue_sequence`,
`status` and `sha`, and a full run response returns `run`, `steps` and
`repository`. Integers keep their full JSON integer precision. JavaScript
clients should use an integer-preserving decoder for IDs above 2^53.

Releases before v0.20.3 also sent every record's Go-style name (`ID`, `RepoID`,
`QueueSequence`, `Run`, `Steps`, `Repository`) with the same value, on every
nested record. That doubled each record and, for a full `run_get` whose steps
carry resource samples, multiplied the reply about fourfold. Those duplicates
are no longer sent by default; see [Go-style field names](#go-style-field-names).

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
`issue_list` (and `Issues` and `NextBefore` too with the Go-style opt-in).

Default MCP text and structured JSON carry the same snake_case names. Tools with a
specialized text format, such as logs or YAML reports, keep that existing text
format; their structured records receive the additive fields. JSON-RPC framing
is unchanged.

## Go-style field names

A client that reads the Go-style names asks for them per request:

- HTTP and MCP: send the header `X-Oberth-Go-Field-Names: 1`.
- HTTP only: or add `go_field_names=1` to the query string.
- CLI: `oberth mcp call --go-field-names`, or `OBERTH_GO_FIELD_NAMES=1` for
  `oberth mcp call` and the REST commands (`oberth runs --json` and friends).

The response then carries both spellings with the same value, the shape
releases before v0.20.3 sent by default, and echoes
`X-Oberth-Go-Field-Names: 1`. Without the opt-in only the snake_case names are
sent. The bundled CLI and dashboard read the snake_case names and also accept
the two-spelling shape, so either side may be upgraded first. Go programs that
decode responses into untagged records (`model.Run`) can use
`internal/recordjson.Unmarshal`, which maps the snake_case names back to the Go
field names.

The v0.20.3 default response shape is an announced exception to the additive
policy below: the values are unchanged; only the duplicate spelling moved
behind the opt-in.

## Compatibility and deprecation policy

Apart from the issue-list privacy and resource-deadline safety exceptions, no
existing name, argument or default envelope is removed or repurposed in v0.17.x.
Prefer the unified names for new integrations; legacy names remain
supported aliases. Clients must tolerate additional object fields.

A future removal requires an explicit deprecation announcement in release notes
and this document, identifying the replacement and first eligible removal
version. The deprecation window must span at least 90 days and one stable minor
release after that announcement. Removal is permitted only in a later major
version after both conditions are met. No removal announcement or deadline is
established by this change. Envelope defaults also require that later-major
migration process; opting into `envelope=1` does not change other callers.

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

The #1294 stage 3 retirement explicitly removes five legacy execution tools; see
[the breaking change notice](releases/unreleased-v0.17.95.md#execution-retirement-1294).
