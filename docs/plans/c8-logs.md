# C8 — Logs

## Purpose

C8 retains bounded native container output, reads exact run/container spans, and
supplies bounded filtering and evidence removal. It does not observe Workflow
status, compute a step plan, classify execution failures, schedule work, authorize
actors, fetch credentials, publish results, or decide retention eligibility. Live reads use requested
Kubernetes `pods/log` streams; completed containers get one stored copy. Missing or
incomplete output has a reason. C8 takes purge and plan facts from its caller.

## Owned data and public contract

The [Shared contracts](README.md#shared-contracts) are normative for shared admission,
plan, step/attempt/count, failure/retry, actor, source, Git, event, identity and
runtime-configuration types. Local descriptions below specify component behavior and use
those definitions; they do not define rival DTOs.

- `Store` owns `<run_id>.log` and its native container index under the configured log
  root. `ContainerLog` binds `node_id`, `pod_uid`, `pod_name`, `namespace` and
  `container_name` to an immutable half-open `[start,end)` span, reason, detail and state (waiting/following/copying/captured/interrupted).
  Streams and verified provisional-parent attribution are process-only inputs. An empty captured container has an exact empty span; it differs from an
  uncaptured container. **Delta:** remove the `burn` field and historical marker-range
  index variant. Internal byte ranges carry offsets, not a second step hierarchy.
- Keep the narrow tracking, reserve, release-before-open, commit, list, filtered read
  and stored-stream operations. C5 verifies native ownership and opens the source; C3
  wires it. C8 accepts bounded bytes and verified identities, never a caller-selected
  filesystem path or an inferred Pod name.
- C4's immutable YAML plan supplies logical steps from admission. C2/C5 associate native
  attempts and container bindings with those entries; C8 does not discover steps from
  text or derive another plan. One UI log part belongs to each planned step; attempts
  retain selectable `node_id`/container identity. Repeated display names never merge
  output.
- `Filter` carries pattern, surrounding context, line offset/limit, tail and optional
  absolute byte offset, under the caller's context. Head cursors move forward; tail
  cursors move backward. Preserve `Meta`'s snake_case fields: `scan_limit_bytes`, `total_lines`, `matched_lines`, `returned_lines`, `tail`, `truncated`,
  `bytes`, `line_numbers`, `line_offsets`, `scan_complete`, `bytes_exact`,
  `scanned_bytes`, `withheld_bytes`, `withheld_lines`,
  `omitted_long_lines`, `has_more`, `next_byte_offset`, `follow`, `copying`, `waiting`, `reason`, `detail`. Counts over an incomplete scan
  cannot claim whole-log totals. `waiting` marks planned output with no capture yet; it never marks interrupted or captured output. `logs.pattern`, `run_logs.pattern`, `logs.raw`,
  `run_logs.raw` and CLI `--raw` use plain content; remove automatic prefix stripping.
- C1/C2 own authenticated surfaces: MCP `run_logs {id,..}` and `logs {repo?,sha,..}`,
  container inventory via `containers:true`, exact `node_id`/`container` selection, and
  the whole-run `raw` view. Responses retain `run_id`, `output`, window/bounds metadata
  and native selectors; inventories are `{run_id,containers:[..]}` with `node_id`,
  `step`, `pod_name`, `container`, `stored`, `reason`. `step` selects the admitted
  Workflow-relative path, without the Workflow prefix or retry suffix; `node_id` selects
  a native attempt. Remove `burn`, `available_pairs` and CLI `--burn`; no aliases. An
  omitted step uses the recorded failure; ambiguous names refuse with candidates. Strict
  decoding and typed forwarding belong to C1/C2; remove raw-map forwarding.
- Keep `GET /api/runs/{run}/logs`, `/containers`, `/containerlog` and
  `/containerlog/stream`. The last streams plain text, not SSE, with explicit native
  selectors and optional `follow`. `oberth log` exposes container listing and
  node/container selection. Unfiltered MCP `run_logs` and CLI reads default to the last
  200 lines and explain the window. Explicit pattern/limit/tail/ offset/byte-offset
  values suppress that default; null means absent. Direct HTTP and SHA-selected
  logs retain their defaults. C2 maps typed public read states to transport responses
  and supplies purge actor/date. ReadRequest carries Selector, Filter, PlannedSteps
  and Purged; the latter two come from the immutable plan and service purge record.
  C8 stores neither and writes no purge tombstone. Follow is a fact offered to the
  view, independent of the reason; reading retained bytes does not start a live stream.

**Delta:** The public read states are derived from caller facts and capture receipts:

| State | Result and reason |
| --- | --- |
| No body yet | successful empty read, scan_complete=true; `run has produced no output yet` |
| Planned step without native binding | InvalidTransition; `step waiting to start` |
| Running step | InvalidTransition; `step is running`, Follow=true, even on the error |
| Reserved/copying output | InvalidTransition; `step output is being copied` |
| Caller says purged | InvalidTransition; `log was purged` |
| Step outside supplied plan | NotFound; `step is unknown` |

A known step with no matching node/container says `no log matches this selector`;
an interrupted copy says `container capture incomplete`. Retained attempts remain
readable while another waits. Mixed capture reasons become `container output incomplete`
with Truncated=true and all detail preserved. These reasons never promise a stream.

## Allowed dependencies

C8 uses the standard library and `internal/publicerr` for public failures, as listed in
[dependencies.json](dependencies.json). Callers supply contexts, byte streams,
scalar identities and limits. C1 consumes filter types; C2 authorizes retention;
C3/C11 compose; C5 verifies native bindings before opening log streams.

## Invariants and budgets

1. Physical identity is run + Pod UID + container; native node attribution can refine
   only from a verified parent boundary. Namespace/Pod-name changes or unrelated
   children cannot reuse a receipt. Refined attribution preserves span, reason and
   capture state. Following availability requires a current verified observation; missing sources, retired observers and process restart end it without changing captured spans or active copies.
2. Reserve durably before opening a source. Ordinary captured or interrupted attempted copies are
   never reread after restart. Final-container archives may retry only unpublished copies; published spans never reopen, including after restart. New archive commit requires a complete bounded source read. Cleanup accepts any captured terminal receipt, preserving its explicit gap or interruption. Missing terminal sources gain one durable PodMissing or never-ran receipt; failed archive storage still blocks deletion. Only a reservation whose source never opened may be
   released on cancellation. Stream to an unlinked bounded spool outside append locks;
   serialize only the file/index commit. Sync bytes before publishing their span; index
   replacement is atomic. Reject invalid or unavailable ranges. Slow logs cannot block
   native status, identity delivery or retirement.
3. One positive aggregate run-log cap includes container copies and ordinary run output:
   default 64 MiB, configured by `--run-log-limit-bytes` / `retention.logs.limitBytes`.
   Preserve a reason even when the remaining byte budget cannot hold its text. C5's copy
   observation shares 30 seconds; readers close on cancellation. Capture failure,
   missing Pods or truncation never manufacture an exit code or rewrite a native
   verdict. No background log follower, reconnect/resume cursor or final-drain protocol.
4. Filtered reads return at most 4 MiB and scan at most 4 MiB, including actual
   read-ahead and repeated tail windows. Regex is at most 512 bytes; context is 0–50
   lines; offsets/limits are nonnegative. Omit records over 64 KiB explicitly; preserve
   complete records and valid UTF-8 boundaries. Cursors progress even across giant
   records or absent matches. Report partial work honestly. **Delta:** delete the
   historical 8 MiB discovery-plus-filter allowance and active marker index; native
   bindings need no text discovery. The `run_logs` and `logs` MCP scans share the new
   bound. Retain the 8 MiB index-decode bound.
5. C2's interactive read budget is ten seconds, including waits. Pattern scans allow one
   per authenticated credential and two globally, acquired in that order; internal
   failure excerpts do not wait for those slots. carry contexts through scanning,
   index-lock waits and reservation/commit reads. C1 caps native HTTP streams at 32,
   checks access roughly each second, reauthenticates every 30 seconds with a
   five-second budget, bounds writes to 30 seconds and joins owned stream work on
   cancellation. These protect server resources; they are not run capacity gates.
6. Log bytes confer no status, identity, grants or retry authority. Preserve plain
   output even when it resembles an old marker. The in-Pod credential wrapper does
   not mask output; the server never receives credential values and does not
   promise to mask workflow text.
7. Confine evidence to the log root, validate run IDs, require regular files, and refuse
   symlink evidence during purge. `UsageByRun` counts owned bodies/indexes; `Purge`
   removes only owned body/index files and returns removed bytes and errors, creating
  nothing. C2 fences writers and repeats protection checks
   under audited purge; C8 cannot decide eligibility. Retention keeps immutable
   run/step/audit facts. do not retain obsolete sidecar names or delete C7's YAML plan.

The human CLI view puts each selected step's own output under its step heading.
Executor containers appear only when a step failed outside its own container,
such as during source delivery; ordinary successful or main-container failures
show the step's own output. Explicit node/container selection remains available;
there is no extra flag to obtain the useful default.

With no explicit step, `oberth log <run>` selects the failed step, then the running one,
and follows it as output becomes available. C2 and C11 preserve that selection for live
and retained reads. Generated reasons are at most 40 characters; remedies at most 120;
complete native text stays in detail.

## Inspections

The requirement sentence is the inspection contract. These inspections are planned.

| Requirement | Inspection |
| --- | --- |
| [REQ-C8-001](REQUIREMENTS.md#req-c8-001) | `live-logs` (walk_step) |
| [REQ-C8-002](REQUIREMENTS.md#req-c8-002) | `live-logs` (walk_step) |

**Delta:** `live-logs` begins before a step starts, explains pending state and follows
the running step without another observation; `retained-logs` repeats the no-step
selection after completion. `TestNativeCaptureContract` covers reservation, interrupted
capture, restart, empty spans, caps and atomic index publication. `TestLogScanContract`
uses an independent byte-range oracle for UTF-8, giant lines, both cursor directions and
cancellation. `TestLogReasonCatalog` checks every reason/remedy and preserves
full detail. `TestPurgeLeavesNoRecordAndReadUsesCallerFact`,
`TestPlannedStepsWithoutPodsAreWaiting`,
`TestRunningReadOffersFollowWithoutPromisingStream` and
`TestUnmatchedContainerDoesNotMakePlannedStepUnknown` check the caller-fact boundary.
`TestMixedCaptureReasonsCollapseWithoutLosingDetails` checks mixed diagnostics.

## Retired terms

Text markers → native attempt/container identities; marker indexes → exact byte spans;
stream-resume journals → once-only capture receipts.

## Copy completion

`Meta.Copying` is true while any selected receipt has an active copy, including
when older attempts already have readable bytes. Views may poll until that copy
commits or is interrupted; reading never opens a source. This fact is independent
of live `Follow` and the run or step execution status.

Default own-output reads trust the native projector's authored-container role.
Container-set membership comes from the native template, including a member named
`main`; sidecars and executor containers do not acquire that role from their names.
