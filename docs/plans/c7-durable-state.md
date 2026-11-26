# C7 — Durable state

## Purpose

C7 persists canonical identities, accepted work, authority, obligations and evidence. It
makes every fact atomic with its applicable audit record and events. It provides bounded,
consistent reads and verified database snapshots. It does not schedule work, execute
dependencies, derive steps from logs, classify native failures, perform source delivery,
push refs, apply infrastructure or own public transport. C2/C3 decide actions; C4/C5
supply admitted plans and native evidence; C6 and C8 own source and log bytes; C9 owns
installation and maintenance.

## Owned data and public contract

C7 also owns `cmd/oberth-store-backup`, the stopped-server backup/restore helper. C7
also owns test-only `internal/testdb` and `internal/storetest`, which production
packages never import. C9 consumes the helper’s bounded process protocol through
stateport; it is not an importable transport or another server composition root.

The [Shared contracts](README.md#shared-contracts) are normative for shared admission,
plan, step/attempt/count, failure/retry, actor, source, Git, event, identity and
runtime-configuration types. Local descriptions below specify component behavior and use
those definitions; they do not define rival DTOs.

`Store` exposes context-bearing typed operations, not a public SQL handle. `Options`
carries immutable protocol configuration, busy timeout, backup bound and clock. Public
records use shared domain types; C1/C2/C11 own snake_case DTOs, HTTP, MCP and CLI. The backup helper is the only C7 command; no public endpoint is introduced. Not-found, invalid input, invalid transition,
ambiguity, lock conflict, unavailable database and schema refusal remain
distinguishable.

| Owned records and keys | Required contract and evidence |
| --- | --- |
| Upstreams and repositories | Durable repository ID plus unique `(upstream_id,name)`; canonical `upstream/org/repo` selectors. Ambiguous bare names refuse. Registration prevents org/name trust-domain collisions. Archive retains history and refuses new work; removal refuses referenced history. |
| Uplinks, credentials, scopes, certificates and invitations | Fingerprint, actor, admin flag, upstream scope, credential digest/expiry/revocation and public SPKI bindings. Never store bearer plaintext, private client keys or fetched secrets. Enrollment consumes its invitation and commits every identity binding and audit record together. Scope and revocation are read fresh. **Delta:** store explicit `kind` and `park_on_push` per selected key, audited; the key push policy fixes operator/installer `park_on_push=false` (always run) and agent `park_on_push=true` (park unless `oberth.run=1`); rotation preserves kind/policy. |
| Runs and receive receipts | Exact repository, actor, ref kind/ref, `object_sha` (accepted object), `commit_sha` (tested checkout), trigger, release flag, schedule entry, explicit workflow selector and immutable history sequence. Receive ID deduplicates the original decision; reused IDs with different contents refuse. Acceptance takes several runs per push and commits the batch, receipt and attribution together. It may record a failed run with findings and no prepared admission; that record cannot submit work or load source. Branch deletion commits cancellation and propagation obligations with its receipt. Sequences order history; they grant no execution capacity. A recent-runs read walks the repository's history index for at most 20 runs per named repository and refuses an unbounded list or one of more than 200 names. |
| Run binding and cancellation | Submission name/binding, uncertain-create settlement and rerun lineage. Cancellation alone writes stop/terminate obligations and persists actor and accepted strategy; Terminate never regresses to Stop. Publication ownership takes precedence over run recovery. |
| Inspection attribution | Keep AssistRequested as request/display/arming intent. Assist atomically audits an exec and marks Assisted before native dispatch, including a dispatch that fails. The inspection audit keeps kind, step, reason, argument count and the SHA-256 of the argument list, never the command arguments. C7 applies [C5's evidence rule](c5-native-execution.md#accounts-and-holds) in the finish transaction, saving Assisted with the native evidence. C5 owns the qualification and native retry/resubmission boundary; [Assist enablement](c5-native-execution.md#assist-enablement) owns activation conditions. |
| Native evidence | Step rows keyed by `(run_id,node_id)`, with boundary/template identity, phase, message, Pod name, timestamps, exit availability, grant evidence, the native never-run skip and Pod-restart facts, and retry ancestry. Retain raw nodes/attempts separately from logical steps. Credential evidence cannot be declassified; missing evidence stays unknown. C5 supplies observations directly; no separate timing or milestone tables. |
| Promotions and publications | One exact tested source, target/base/result/tree and retry lineage. Publication intent binds exact object, predecessor, actor, attempt count and retry time. Receipt, run, promotion, single-ref deletion and issue work settle atomically. A delivery reply is not status authority. |
| Issues and repair projection | Manual/CI identity, scope/repository/branch, title, body, occurrences, state, work status, revision, timestamps, explicit coordination locks, CI work receipts and green receipts. Reads do not renew locks or mutate staleness; a list reads its page and counts from one snapshot. Revision conflict rolls back the whole mutation, including lock renewal; real metadata changes advance revision. A failing run preserves a person's title and lock on a native CI issue. |
| Authority | Exact repository/workflow-file/step/secret grants; secret declarations are computed on demand, never stored. Preserve repository capability grants and repository/tier PVC ownership. Accounts follow C5's release-authority contract; no configured release-role mapping or repository-wide account override is stored. Grant mutation and audit append share a transaction; revocation never silently transfers PVC ownership. |
| Source binding | Store the sealed prepared admission with each run admitted for execution and record its native Workflow name/UID once. A failed receive record may have no admission or native identity. Re-pinning reads those facts after restore; the run record contains the source facts needed for recovery. |
| Audit and observations | Hash-chained audit actions; repository archive and schedule pause are projections of audited facts. Step/run progress, retained agent events and native lock observations are not audit records; only authority and administrative changes enter the audit chain. Purge records preserve run identity and receipts. |

### Admitted steps and observation events

**Delta:** persist C4's bounded immutable plan from the same prepared admission result
used for submission and audit. Its logical key is the native invocation path without
Workflow prefix or retry suffix. The plan contains entries and exactness; no commands,
credentials or full native spec dumps. Store the digest in the audit binding, not the
full plan. Identical replay is idempotent; changed plan/revision/binding refuses. Native
scheduled-child admission commits the plan with its parent/child proof and run row.

**Delta:** Store `step_count`, `steps_planned_exact`, `steps_pending`, `steps_queued`,
`steps_running`, `steps_passed`, `steps_failed`, `steps_skipped`, `steps_unknown` as
columns, updated atomically with the evidence that changes them. Lists omit plan bytes
and never decode all plans/nodes for counts. Detail reads omit the sealed Workflow and use one consistent
plan/evidence snapshot; observed steps use their saved projection. Dynamic expansion changes the projection, not the immutable
admitted plan. Unreached entries create no execution rows. Finished runs resolve the
dynamic remainder rather than leaving it pending. Store C4’s capped result; C4 owns
collapse-to-placeholder behavior and never refuses admission for plan expansion.

Store applies C2's ordinary event retention separately from progress retention.
Events retain monotonic IDs, dedupe keys and ascending exclusive `after_id` reads. The
eight-kind envelope is pinned in [Shared contracts](README.md#shared-contracts); scope
IDs and dedupe keys stay internal. **Delta:** implement C2's [Events and
observation](c2-service-actions-and-history.md#events-and-observation), including
a projection revision and its event written with the fact, plus separate progress retention. Preserve
cursor order and replay across restarts; notices never authorize work. The fact writer
supplies the events; C2 owns reconnect/reset semantics. Use the existing endpoint-seek bounds query, not
a full event scan for the fence; own-event cursors seek the event window. A lagging cursor may scan that whole window before reaching the fence.

Bounded queries supply lists, a run by exact commit or ref, pending publications
and upstream details needed by consumers. Declarations are not a stored fact.

### Imported manual issues

**Delta:** C2's neutral-file import preserves issue numbers, open/closed state,
text, original authors, timestamps and ordered history in the ordinary issue
records, with an imported mark and provenance. It creates no synthetic run,
CI selector or CI reconciliation obligation. Native issues and imported manual
issues share the same later read, edit and close paths.

Commit each file's issues, receipt and audit in one transaction, or none. Refuse
existing numbers and non-manual kinds. A receipt identifies the exact imported
record set so a lost-reply retry confirms it without overwriting subsequent edits.
Import may continue only the initial import series; it cannot write into an
installation with native issues. Advance number allocation above the maximum
imported number in the same transaction, so importing lower numbers later cannot
move allocation backwards.

A file and its encoded request each fit 1 MiB; a record line and the complete
public issue, including provenance and history, each fit 128 KiB. The complete
issue budget is checked at the single save point for import and ordinary writes;
a refusal rolls back the whole mutation. The importer never truncates or raises
these limits. C2 owns the
[import action and its product boundary](c2-service-actions-and-history.md#manual-issue-import).

### Baseline and backup

**Delta:** The current baseline is schema 6, with only current records, constraints
and query indexes and no configured release-role records. Current-schema opens add no persistent schema objects. Existing receive receipt JSON retains all accepted old/new ref values and at most 16 replaced heads, including parked and no-pipeline pushes; inherit only when the old object equals the previous accepted new object. A disposable connection-local index rebuilds from those receipts for sync. Run publications copy their original receive's set into private durable evidence, separate from the live lease; the durable schema stays unchanged. A layout pin for each
schema number pins its lineage by layout; no step receipt is stored. A changed layout requires a new
number. Open applies numbered forward steps in one transaction, preserving immutable IDs,
receipts and grants; the full audit chain and head are verified before commit. A gap refuses read-only and names the source schema's upgrade release; newer schemas refuse. Older backups name their reader release. Each step has a previous-code
fixture proving complete record reads and unknown-ID refusal; the release guard requires both.
Its named coverage test must run and pass. Current-schema opens only verify; creation and maintenance stay distinct. A missing admin database is never created.

Database snapshots pin a committed read-only view, include committed WAL state, verify
schema, integrity, foreign keys and audit before emission, and stream a bounded
manifest/member archive owned and encoded by C7, not by the shared state port. The port
carries interfaces and facts, not an archive format. `BackupReport` exposes only
`schema`, `audit_head`, `integrity`, `foreign_keys`; members carry `name,size,sha256`.
[C9's backup contract](c9-install-and-upgrade.md#backup) owns age-format
encryption and safekeeping; its [command inventory](c9-install-and-upgrade.md#owned-data-and-public-contract)
owns the shipped maintenance surfaces. C7's offline helper uses the same age
format and verifies schema, integrity and the local audit chain before replacement. Snapshot inspection reads unlinked private files, so no plaintext name outlives the process. Run history starts fresh. Cutover
is a one-off operator procedure outside the product.

C7 owns canonical run, acceptance, branch deletion, query and event values in
`internal/stateport`, including the fake/real contract suite. Consumers declare
small ports over these values. One `AcceptBatch` transaction records runs, rejected
admissions, branch deletions and the receipt with audit and events. It cancels
active runs on deleted branches and records their exact publication intents.
No promotion grant, reservation, consumption, train, prune or tag-after table remains.

## Allowed dependencies

Store uses only the shared C7 allowlist in [dependencies.json](dependencies.json), its
embedded SQL driver and local filesystem primitives. It implements caller-owned
persistence ports over shared values; there is no C6-owned source persistence port. C4
pure values come from admissioncontract and stepmodel, not a validator runtime. Canonical names use reponame; source bindings live
in the stored admission and native ownership in the run. No native clients, repository
process, log reader, service, transport, installer or schedule
reconciler may enter Store. Its separately owned backup executable is
a bounded leaf adapter, not a dependency of Store.

## Invariants and budgets

1. One writer, WAL, foreign keys and full synchronization; bounded reads use read-only
   connections with fresh snapshots. Eight ordinary read connections and two audit-scan
   connections remain separate from reserved authentication, readiness and idle readers.
   Token identity and scope share a transaction. Authentication reserves four readers,
   with at most three calls per protocol so one protocol cannot consume the reserve. No
   authorization cache or immutable live reader. Default busy timeout is five seconds;
   caller deadlines bound waits. Shutdown releases the Store mutex before joining
   workers, draining pools and checkpointing.
2. Commit every fact, its applicable audit record and its events in one transaction;
   abort all on failure. Run finish includes publication intent and its events: no
   separate event write may lose work after a crash. Step/run observations are
   progress, not audit actions. No unaudited authority or administrative mutation.
   Reads, telemetry and observations add no authority. Enforce
   credential expiry exactly; refuse removal of the last active unscoped admin. Audit
   domain and schema identity are immutable configured tokens of at most 128 bytes,
   never inferred from rows. Append the next hash using the stored head in the same
   transaction; do not scan or verify the chain for each mutation.

3. Verify the local hash chain on audit reads, backup and restore. Corrupt history
   refuses that read or recovery operation.

   **Delta:** External audit witness support is dropped; no external witness.

4. Execution recovery never refires work. Existing named Workflows require C5 binding
   verification; API errors cannot establish absence. Stranded work fails with the
   restart reason. Cancellation obligations survive lost replies; a stale completion
   cannot clear a newer obligation. Pending publication remains its owner's
   responsibility. C7 applies [C5's evidence rule](c5-native-execution.md#accounts-and-holds)
   atomically with finish. C7 persists one typed
   failure/retry record produced by C5 and projected by C2; human wording cannot decide
   eligibility (**Delta:**).
5. Terminal identities, admitted bindings, grants and receipts keep their
   immutability/monotonicity guards, including the native Pod-restart fact. All step inputs validate before writes; batches
   commit at most 128 rows with no total node ceiling. A partially completed batch
   sequence never claims complete terminal/replay evidence. Missing exits, grants or
   attempts cannot become success or replay permission.
6. Publication attempt CAS happens before external mutation; intent retries do not
   repeat a push or Workflow Create. Preserve five publication attempts and persisted
   backoff, exact predecessor/tag object and atomic finalization. Store the attempted flag separately from the immutable CI sequence bound in existing publication JSON without changing the durable schema; decode existing private attempt metadata into that flag and normalize the old empty-history sentinel to zero.
   Abandoned intents stay terminal. C2's per-repository/ref coordination must not weaken
   these checks or become a global execution gate.
7. Scope/filtering precedes pagination, totals, prefix resolution and metadata decoding.
   Empty explicit scope means no repositories. Preserve stable history cursors and
   consistent owner/status reads. Index actual predicates and ordering. add the receive
   lookup index on `(repo_id,ref_kind,ref,created_at)` with tested tie ordering. Prove
   index use with query plans.
8. Bound server records, not authored workload resources. Plans: C4’s 2048-entry / 1 MiB
   representation cap, with inexact placeholders. C7 validates that constructor
   contract; it does not impose a workload cap. Full step diagnostic detail is stored
   separately; capability JSON: 16 KiB. Event pages: internal scans 500, public C1/C2
   pages 200; summary follows the public message contract; data: 8 KiB; dedupe key: 512
   bytes. Retain bounded observation windows, separately for progress. Purge removes evidence bytes
   only after durable intent and repeated protection checks; run/step rows and receipts
   survive.
9. Snapshot default is 1 GiB, hard maximum 4 GiB; zero selects default, never unlimited.
   Stream large members. Private staging uses 0700 directories next to the database.
   Reserve snapshot allocation plus max(256 MiB,snapshot/4) and 64 KiB metadata; recheck
   around every 128-page step. Sparse file length is not allocated capacity. Serialize
   snapshots sharing a parent directory; cancel, clean partial output, preserve
   write/cleanup errors and sweep owned unlocked staging before startup I/O. Refusal
   names measured space and remedy. Inspection never migrates the source database.

## Inspections

The requirement sentence is the inspection contract. These inspections are planned.

| Requirement | Inspection |
| --- | --- |
| [REQ-C7-004](REQUIREMENTS.md#req-c7-004) | `TestLocalAuditChainContract` (contract_test) |
| [REQ-C7-009](REQUIREMENTS.md#req-c7-009) | `TestNonwideningDurableIdentityContract` (contract_test) |
| [REQ-C7-011](REQUIREMENTS.md#req-c7-011) | `TestTransactionalEvidenceUpdatesContract` (contract_test) |
| [REQ-C7-019](REQUIREMENTS.md#req-c7-019) | `TestNonblockingAuthenticationTelemetryContract` (contract_test) |
| [REQ-C7-020](REQUIREMENTS.md#req-c7-020) | `ui` (walk_step); `TestIssueImportDryRunReplayAuditAndSequence` (contract companion) |
| [REQ-C7-021](REQUIREMENTS.md#req-c7-021) | `TestSchemaNumberPinsBaselineLayout` (contract_test) |

The schema inspection pins each numbered baseline's complete layout and requires a
new number for a changed layout. `TestRemovedReleaseTablePreservesDatabase`
also proves that adding a removed release-role table refuses open with the
data-preserving release or verified-backup remedy, even when the schema number still matches.

`TestTransactionalEvidenceUpdatesContract` must include the native CI failure
case in the Issues and repair projection contract: a person's lock and title
survive, alongside the revision and audit checks.

The import contract tests in `internal/store/issue_import*_test.go` exercise
atomic storage, replay and number allocation. The service's
`TestIssueImportContract` uses the real store and its fake;
`TestIssueImportExactLineAndFileBounds` and
`TestIssueImportEditBudgetIsAtomicAndComplete` check the complete bounds.
The UI walk still checks the ordinary issue view and later edits.

The import inspection loads disjoint numbered files, injects a conflict and an
oversize record to prove whole-file rollback, replays a committed file after an
edit and checks the next native number. It verifies the common complete-issue
bound on import, append and update without losing a prior record.

The local audit inspection changes one chain entry and proves read, backup and restore
refuse it. Instrument normal mutations to prove they perform no chain scan. `TestStoredAdmissionRestoresKeepRoots` verifies durable admission and write-once
native identity after restart. `TestTransactionalEvidenceUpdatesContract` uses
independent writers, a stale revision and injected write/audit/event failures.
It proves fact, audit, events and terminal publication intent commit or roll back
together, and observations create no audit rows. Acceptance tests use multiple
runs in one push, roll the whole batch back on failure and retain a failed run
with findings but no fabricated admission or plan. `TestRealAdmissionRecordsAfterGateAuthorityAndArchiveChange` seals with real C4,
then changes key scope, revocation, grants and archive state before acceptance.
The stored seal still records and replays; only this test seam may import C4.
C9's backup inspection tests exercise C7's archive format and database snapshot
contract. C7 tests cover committed WAL, fsync failures and independent-handle
competition.

## Retired terms

**Delta:** Source custody tables → stored admission and write-once native identity;
transplanted migration catalog → numbered forward steps; per-mutation audit verification →
read/backup/restore checks.

### Fix and resume

The existing origin JSON stores resumed_from, resume_uid and the accepted resume_plan write intent. workflow_uid stays empty, so workflow_identity remains unique without any schema object or schema bump. NativeUID returns resume_uid for a resumed run. Insertion marks Assisted and checks for an existing successor through run_ref in the same BEGIN IMMEDIATE write transaction. This serialization relies on one server process, todays deployment; another writer design needs a unique successor index. Two concurrent inserts have one winner. Ordinary unbound recovery excludes resumed_from; native resume recovery owns it.
