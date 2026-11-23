# C1: Transport and tool registry

## Purpose

C1 owns HTTP/MCP framing, authentication handoff, public request and response types,
tool registration, bounded response delivery and the embedded web UI. It presents
authorized actions and history from C2. It mounts C6's separate source reader. It does
not admit workflow YAML, compute plans, execute workflows, schedule runs, allocate
capacity, persist events, infer failure causes or own grants, publication and issue
policy. C11 composes listeners and their lifetime.

## Owned data and public contract

The [Shared contracts](README.md#shared-contracts) are normative for shared admission,
plan, step/attempt/count, failure/retry, actor, source, Git, event, identity and
runtime-configuration types. Local descriptions below specify component behavior and use
those definitions; they do not define rival DTOs.

### Authentication and transport state

Authentication produces `internal/actor.Actor` as defined in [Shared
contracts](README.md#shared-contracts). `TokenCredentialID` remains internal.
Authentication returns an actor or a typed invalid-credential/temporary-failure result.
Only verified client certificates are accepted. Dual credentials must agree on identity,
fingerprint, admin and scope; an unknown certificate cannot fall back to a bearer.

C1 owns only ephemeral session IDs, subscription filters, resource subscriptions,
delivery cursors, stream reservations and browser
preferences. A session binds delivery to a credential; it grants no authority and is
unnecessary for tool calls. Durable events, run records and authorization remain behind
C2 interfaces.

### One tool declaration and one decode

**Delta:** each tool has one typed declaration: name, description, argument type/schema,
result type/schema, visibility and scope classification, mutation and audit
requirements, blocking bound, and a typed handler binding. The registry derives listing,
lookup, timeout metadata and invocation from that declaration. Missing or duplicate
declarations and unclassified tools fail construction. C2 supplies typed handlers
through interfaces; C1 never imports C2's package. There is no reflection-based
dispatcher or raw-JSON forwarding between handlers.

One shared bounded decoder uses the standard library plainly for Oberth-owned objects:
bound the input, refuse unknown fields and trailing data, then validate typed arguments
once. Explicit null means absent, just like omission; there is no presence tracking or
separate exact-key parser. Omitted tool arguments use the zero-value argument object.
Protocol extension dictionaries keep their declared semantics, and native workflow
documents keep C4's schema boundary. Refusals list knowable offenders and remedies
without echoing credentials. C1 uses `internal/reponame` for canonical repository paths
and bare repository names.

**Delta:** replace `decodeOne`/`strictUnmarshal`, the separate admin object walker and
C2's permissive decoder. Decode before deriving scope-sensitive behavior, audit targets
or defaults. A validated admin call contains validated typed arguments and its public
audit target; C2 then checks live authority and records intent/outcome.

The tool registry exposes these operation families:

| Family | Surface |
| --- | --- |
| Run observation and evidence | `status`, `wait`, `logs`, `run_list`, `run_get`, `run_logs`, `system_status` |
| Run actions and assistance | `run_check`, `run_cancel`, `run_rerun`, `run_purge`, `run_hold_list`, `run_hold_exec`, `run_hold_abandon` |
| Publication and promotion | `sync`, `promote`, `promotion_list`, `promote_status`, `promote_retry`, `publish_retry`, `publication_abandon` |
| Issues | `issue_create`, `issue_get`, `issue_get_many`, `issue_list`, `issue_update`, `issue_append`, `issue_close`, `issue_reopen`, `issue_delete`, `issue_lock`, `issue_import` |
| Identity and repository administration | `uplink_add`, `uplink_scope`, `uplink_invite`, `uplink_cert`, `client_renew`, `uplink_list`, `uplink_revoke`, `uplink_rotate`, `upstream_add`, `upstream_list`, `upstream_remove`, `upstream_provide_key`, `repo_add`, `repo_verify`, `repo_list`, `repo_remove`, `repo_archive`, `repo_unarchive` |
| Grants and credential diagnostics | `secret_put`, `access_list`, `access_allow`, `access_revoke`, `access_dismiss`, `workflow_capability_get`, `workflow_capability_grant`, `workflow_capability_revoke` |
| History, repair and schedules | `audit_list`, `receive_outbox`, `schedule_list`, `schedule_pause`, `schedule_resume`, `schedule_trigger` |
| Admin observations and receipts | `admin_access_list`, `admin_public_identity`, `admin_readiness`, `admin_version`, `admin_idle`, `admin_audit_record` |
| Agent loop | `run_wait`, `events_wait`, `sync_status` |

This is an operation inventory, not permission to preserve obsolete argument aliases. C2
owns each operation's business contract. C11's CLI consumes the same schemas and
timeout/mutation metadata.

### Read-only run check

**Delta:** declare `run_check` as a scoped read with args `{repo,ref?,files?}`. `repo`
is canonical; absent `ref` selects the default branch head. `files` is an optional array
of `{path,content}` UTF-8 overlays confined to regular authoring files: 1–32 files, 1
MiB total, no symlinks or parent traversal. C11 reads `--local` into these bounded
overlays. Result is `{ok,repo,ref,object_sha,commit_sha,selected_file,spec_digest,plan,
findings,checks}`; unavailable SHA/digest/plan facts are null, never invented.
`findings` entries are `{path,code,reason,remedy,command?}`; `checks` entries are `{name,state}`
with `state=offline|snapshot_checked|live_required|unknown`. `ok` means all required
checks passed; unknown/live-required is not a pass. C2 authorizes and calls C3 inspect
mode; no acceptance, run, Workflow, token, declaration write or audit mutation occurs.
C4 is the sole validator.

### Routes and wire shapes

| Boundary | Contract |
| --- | --- |
| Public | `GET /healthz`, `/livez`, `/readyz`; `/`, `/runs`, `/runs/{run}`, `/repos`, `/issues`, `/issues/{id}`, `/status` and `/assets/{name}` serve redirects or a data-free static shell. |
| Authenticated MCP | `POST /mcp` requires `Content-Type: application/json`: JSON-RPC 2.0 `initialize`, `ping`, `tools/list`, `tools/call`, initialized notification and run resource methods. `GET /mcp` streams session events; `DELETE /mcp` closes a session. |
| Authenticated views | `GET /api/runs`, `/api/runs/counts`, `/api/runs/{run}` and its `/logs`, `/containers`, `/containerlog`, `/containerlog/stream`; `/api/repos`, `/api/issues`, `/api/issues/{id}`, `/api/grants`, `/api/audit`, `/api/schedules`, `/api/status`, `/api/uplinks`. `/metrics` requires metrics authority or an unscoped admin. |
| Actions | `POST /api/promotions` calls the same typed promotion operation. `POST /api/admin/{action}` uses the fixed admin action inventory, JSON bodies only, no query arguments, and unscoped admin authority. Database backup is a stream, not an MCP tool. |
| Enrollment | `POST /mcp/enroll` offers only `uplink_redeem`. Invitation inputs are body-only. Its typed result includes the server's SSH endpoint, delivered inside the verified TLS session for the joined profile. It creates no ordinary MCP session and cannot call other tools. |
| Source | A dedicated listener exposes only C6's source Load route. Pod credentials confer no user, admin, metrics or run-history authority. |

**Delta:** bind route-only access operations to the same typed grant operation, with no
parallel dispatcher. Storage, credential handling and bootstrap remain with their owning
components.

MCP keeps the explicit negotiated dates `2025-03-26` and `2025-06-18`, preferring the
latter. Unsupported valid dates receive the preferred date; malformed dates are invalid
params. Version headers are validated per request. Negotiation is not an authentication
gate. Tools use `{name, arguments, _meta?}`. Results have text `content`, object
`structuredContent` on success and `isError`; protocol errors retain their JSON-RPC code
and request ID.

Public DTOs have explicit snake_case tags after authorization/redaction. Collections are
objects such as `{"runs":[]}`, `{"repositories":[]}` and `{"grants":[]}`. Empty
collections are arrays. Dictionary keys are untouched. `promote` acknowledges `{id}`
with optional requested follow-up state; issue deletion acknowledges
`{id,deleted:true}`. **Delta:** remove post-serialization reflection and renaming; tag
the run detail and its projections directly. The published snake_case shape already
exists and remains the sole spelling.

Run selectors use `id`, full or an unambiguous hexadecimal prefix of at least eight
characters, resolved inside scope to an exact ID before mutation. Scoped run/promotion
pagination withholds global sequences and uses record-ID cursors. Full run detail
retains authorized timing and diagnostics; `view=dashboard` omits heavy diagnostics.
`run_get` defaults to a compact summary, paged at 100 steps. Read-only views never renew
issue locks. Only `issue_lock` does so through C2's mutation gate.

Run and event identity use only `object_sha` (accepted pushed object) and `commit_sha`
(tested checkout commit), as pinned in [Shared contracts](README.md#shared-contracts).
There is no `sha` output alias. Input SHA selectors keep their declared selector
semantics.

### Steps, logs and events

**Delta:** display the immutable admitted step plan supplied by C2/C4. `step_count`
describes the complete logical inventory, independently of paging; `steps_planned_exact`
distinguishes an exact total from a lower bound. Every planned leaf is present
immediately. Unreached entries display pending, without adding a run status. Retries
fold into their plan entry while retaining native attempt/node/container identities for
logs. Literal loops expand in the plan; dynamic loops and recursion expand their
placeholder in place and show `≥N` until exact. Structural nodes are not steps. The
affected tools are `run_get`, `run_list`, `status`, `wait` and `run_wait`; the UI run
list and run detail use the same projection. One step has one UI log part with its
attempts. C1 does not compute a second plan. Public run fields are `object_sha`
(accepted object) and `commit_sha` (tested checkout), without a `sha` output alias.
`publish_retry` descriptions refer to passed logical steps and complete bound attempt
evidence, with no grouping vocabulary.

Run display states remain `queued`, `running`, `passed`, `failed`, `cancelled`. Reasons,
unknown evidence, assistance and retry advice come from C2/C5, not UI message parsing.
**Delta:** remove the run-success `delivered` alias; publication delivery remains a
separate fact. These display-state changes affect `run_get`, `run_list`, `run_wait` and
`status`. Commit identity, source SHA and tested SHA are shown according to C2's typed
record; a tag object is not silently relabeled as a commit.

Retained log reads preserve filter, byte cursor, completeness and truncation metadata
from C8. Active missing retained output is pending; an unknown step is missing; a
terminal missing log is a fault. Live `/containerlog/stream` returns plain bytes, not
SSE or status markers. **Delta:** retire `artifacts`, `artifact_get`, artifact HTTP
routes and downloads, `oberth artifacts`, `--artifact-retention`,
`--artifacts-budget-bytes` and artifact chart retention settings. `run_purge` and its
`what` selector cover logs only; old run/artifact data is not imported.

The `events_wait` arguments carry `repos`, `refs_glob`, `kinds`, `own`. C2 applies actor scope
first. `EventPage` carries `events`, `next_after_id`, `latest_id`, `oldest_id`,
`has_more`, optional `gap`; waits add timeout information. Events carry `id`, `kind`,
`at`, public run/promotion/repository/ref/SHA identity, summary and bounded data;
internal deduplication keys stay hidden. **Delta:** use the eight kinds in [Shared
contracts](README.md#shared-contracts), including observation-only `run.updated`, owned
by the component recording each fact. The filter is `events_wait.kinds`. C2's [Events and
observation](c2-service-actions-and-history.md#events-and-observation) defines its
payload `{run_id,repo,ref,object_sha,commit_sha,data?}`, internal count pair in `Data`,
revision dedupe key, fact-and-event transaction without coalescing, and
separate 24-hour/newest-10,000 progress retention. C1 preserves those semantics and
cursor gaps; it emits no independent update events. Delivery is at least once. Clients
deduplicate IDs and continue with `next_after_id`, including on empty filtered pages and
timeout. Push and `run://<id>` resource notifications are observations. Reconnects
resume with `Last-Event-ID`; gaps/backlog limits are explicit, never completion
evidence. `GET /mcp` must not silently reset a supplied old/future Last-Event-ID cursor
to the current head; use C2's cursor contract.

**Delta:** Retire `secretstore_plan`, `secretstore_sync_receipt` and
`secretstore_verify` and their agent arguments; C9 installation commands keep
that work. Retire `events_since`, `events_subscribe` and their arguments. Agents
use `events_wait`; interactive sessions receive pushed notifications.
Authentication has no failed-login table, failure buckets or failure-limit
replies; invalid credentials still refuse.

**Delta:** MCP tools and CLI tool listings describe each exact input and result
with useful descriptions. Success prose and descriptors follow those
declarations; preserve error classes, event membership, order, counts and
identities. This does not exempt the wire protocol from comparison.

**Delta:** `issue_import` is an installation-wide administrator mutation using
C2's [import action](c2-service-actions-and-history.md#manual-issue-import) and
C7's [saved-issue contract](c7-durable-state.md#imported-manual-issues). Its typed
arguments are `jsonl` and optional `dry_run` and `upstream`; `upstream` follows
C2's [explicit mapping rule](c2-service-actions-and-history.md#issue-upstreams).
The ordinary public request bound applies. Transport preserves refusal fields.

C5's [release authority](c5-native-execution.md#release-authority) supplies the
account chosen in admission. No tool or argument configures a release identity.

## Allowed dependencies

C1 uses the exact C1 allowlist in [dependencies.json](dependencies.json). It imports
shared values/strictjson, C8 `Filter`/`Meta` for CLI and MCP presentation,
and C6 only to mount the dedicated source handler. C2 actions/views/events
are interfaces declared at the consumer boundary and injected by C11; no
C2/C3/C4/C5/C7/C9/C10 import.

## Invariants and budgets

- Authenticate every data request. Invalid credentials are 401; lookup failure or
  expired authentication budget is 503 with `Retry-After: 1`, never anonymous fallback.
  Authentication has five seconds. Scoped missing and hidden records are
  indistinguishable; counts, prefixes, event payloads and pages obey the same scope.
  Tool visibility cannot replace C2's authorization or atomic audit writes.
- Preserve core admission, identity, grant and secret boundaries. C1 adds no
  workload hardening; [C9 invariant 7](c9-install-and-upgrade.md#invariants)
  owns the default floor and egress. Public errors use the shared injected classifier and only
  explicitly public reasons. Unknown tool failures return a correlation reference, not
  dependency chains. Credential material never enters request diagnostics, UI markup,
  event summaries or audit targets.
- Body limits are 1 MiB ordinary JSON, 128 KiB admin, 96 KiB enrollment. Enrollment
  execution has 15 seconds. Invalid arguments cannot reach a handler or produce mutation
  intent.
- Long polls are `wait`, `promote_status`, `run_wait`, `events_wait`: 64 per credential,
  immediate refusal at capacity. `timeout_s` defaults/maxes at 600 seconds within the
  configured service ceiling. Write deadlines include that ceiling plus one minute.
  Explicit acceptable SSE gets an immediate comment, 20-second heartbeats and exactly
  one final RPC message. Each frame has a five-second write/flush budget. Validate and
  authorize before streaming; disconnect cancels observation only and joins the writer.
- Session bounds: 16 sessions per credential, one-hour idle lifetime, 64 run
  subscriptions per session; event streams default 4/max 64 per credential; replay
  backlog default 500/max 10,000, fetched in batches of 100. Reauthenticate at 30
  seconds with a five-second budget; changed authority closes delivery. Event summaries
  follow the public message contract and data stays within 8 KiB.
- Native log streams: 32 per server, access checks roughly each second, credential
  rechecks at 30 seconds, five-second checks and 30-second writes; close the source and
  join the watcher on cancellation. Keep C8's bounded scans. MCP `run_logs` defaults to
  200 tail lines only when no window filter is present; explicit zero/false/empty
  remains explicit. Bulk issue results reserve a row per requested ID inside 256 KiB; no
  body truncation masquerades as a complete record.
- Drain withdraws readiness first, refuses new work with 503/`Retry-After: 1`, and ends
  observers with RPC `-32001`. Already accepted work retains its owner. Never retry
  mutations automatically after drain or a lost reply; inspect durable state.
- Static assets are embedded, fixed-map lookups with content-based versions. Shells
  contain no private data. The shell has a same-origin CSP without inline script or style; pages use
  text nodes only. The bearer stays in this origin's browser storage and is sent
  only in the Authorization header. Asset URLs carry content versions. JSON conditional reads and gzip do not compress
  streams. Backup writes keep their 30-second idle bound.

**Delta:** C1 never writes a secret, token or private key to logs, events, diagnostics or command
output. Server diagnostics use a fixed field list. The UI shows a new pushed run within
five seconds and preserves the CLI's run, step and log identities. Steps stay in
plan order; consecutive runs of at least two rows with the same positive `parallel_group` share an inset rail with visible ends and at least 3:1 panel contrast, keeping one line per row at one text level. The CLI and agent block use `┌ │ └` for each group; single rows carry no mark. `credential_intent` shows one `secret` tag without secret names or
values. Issue bodies are plain text. Imported issues show their original author,
closer, ordered history and imported mark through the same issue view. The status
screen shows one reason, its detail and run counts. Executor-noise lines appear
as returned by the log read;
the script has no folding rule. Live updates use `events_wait` to trigger reads.
Signed-out visitors see only the sign-in card. The header starts hidden and appears
only after an accepted bearer or browser certificate sign-in; a 401 hides it.
Navigation keeps the sign-in card until the server answers.
First-load connection trouble shows a reconnecting notice while the header and
sign-in card are both hidden.
An available answer clears this notice on every route.

## Inspections

The requirement sentence is the inspection contract. These inspections are planned.

| Requirement | Inspection |
| --- | --- |
| [REQ-C1-002](REQUIREMENTS.md#req-c1-002) | `TestPrivateBoundedDiagnosticsContract` (contract_test) |
| [REQ-C1-003](REQUIREMENTS.md#req-c1-003) | `ux-gate` (walk_step) |
| [REQ-C1-004](REQUIREMENTS.md#req-c1-004) | `ui` (walk_step) |
| [REQ-C1-005](REQUIREMENTS.md#req-c1-005) | `ui` (walk_step) |

The UI walk opens imported open and closed manual issues, checks original
author/closer, ordered history and the imported mark, and closes an imported open
issue through the ordinary manual action.
`TestMCPPostRequiresJSON` refuses other media types before invoking a handler.
`TestUIFieldsAreContractFields` checks the secret tag against the step field.
`TestViewPreservesReasonRemedyAndDetail` checks full detail through HTTP, MCP and the
UI. `TestToolRegistryAndDecodeContract` exercises every registered handler, invalid
typed arguments, unknown fields, null-as-absent and complete responses. Stream
inspections prove cancellation joins observers without cancelling accepted work. The
`ui` walk checks the pushed run appears within five seconds and matches CLI identity,
steps, status and logs. `ux-gate` calls each component's complete catalog test, checking
40/80/120 characters and rejecting ellipses; native detail remains whole.

## Retired terms

Reflective wire aliases → explicit snake_case values; phase grouping → steps and native
attempts; historical artifact reads → retained logs.
