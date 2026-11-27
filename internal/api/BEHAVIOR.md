# Caller-visible inventory

Recorded before implementation from the transport reference and slice-01.

- Health, liveness and readiness are public. Private reads authenticate first;
  invalid credentials, temporary lookup failures and drain have distinct replies.
- Verified certificates and bearer credentials bind live authority; conflicting
  credentials refuse. Hidden records and missing records share service refusals.
- Tool listing, typed calls, omitted arguments, invalid arguments and protocol
  discovery use MCP 2026-07-28 metadata on each request. Unknown operations refuse.
- Run reads preserve queued/running/passed/failed/cancelled states, plan order,
  complete counts, paged steps, native attempts and separate diagnostics.
- Log reads preserve waiting/running/copying/cut/purged/missing facts supplied by
  the log owner. Live streams contain bytes only and release their source on exit.
- Issue reads never acquire or renew a lock. Issue mutations use typed operations.
- Promotions acknowledge IDs; administration uses bounded body-only operations.
  Backup is an independently bounded stream; enrollment has an isolated inventory.
- Waits reserve per credential, authorize before streaming, emit heartbeats and
  one final reply, cancel observation on disconnect and join their writer.
- Finite events_wait preserves filtered empty-page advancement and explicit gaps.
  MCP has no initialization session, GET stream or Last-Event-ID recovery.
- JSON reads support conditional responses and gzip; streams bypass compression.
- Retained artifact readers, wire aliases, grouped execution views and message
  parsing are deliberately absent. Browser code is outside this task.

Public API tests and remaining integration work are mapped in the component report.

R1 contract decisions: each ordinary tool has a unique input struct. Required
nested fields are checked before service preparation. Wait deadlines include a
five-second response margin beyond the bounded service timeout. An expired wait
returns timed_out; a disconnected observation never cancels durable accepted work.
The service owns selector combinations, grant policy, authoring paths, lock
history policy, audit target derivation and bulk issue response fitting. Transport
checks wire shape, bounded lengths and explicitly tagged repository syntax.

The registry has 78 service tools. Publication listing exposes durable delivery;
secret-store planning/verification/receipts belong to C9. Native Git replaces bulk
ref-deletion tools. Explicit issue_close calls and expiring locks replace bulk
close/unlock. Promotion uses one tested source; agents use native Git for deletion and tags.
Release reruns return a warning without a force field. run_wait preserves until; run_get uses the
canonical view selector. The view selector and whole issue bodies are canonical; superseded aliases are not accepted.
