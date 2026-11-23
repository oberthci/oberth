# C2: Service actions and history

## Purpose

C2 accepts attributed work, checks scope and grants, coordinates publication and
presents run, promotion, publication, issue and event history. Each action checks
authority and inputs, commits its fact with audit and events in one C7 transaction,
then performs at most one external effect. Argo executes; Kubernetes places Pods;
Git fences ref changes. C2 has no execution queue, scheduler or progress merger.

## Owned data and public contract

C2 owns `internal/service`, runtime `internal/clientcert/issuer`, the authority
and message catalogues, and consumer ports. Test authorities live in
`internal/clientcert/issuer/issuertest`; installation owns authority creation. [Shared Go declarations](README.md#shared-contracts)
define values. C7 owns disk state; C11 injects C3/C6/C7/C8/C10 adapters.
C2 keeps per-ref locks, per-run hold slots, scan slots and publication retry timers
in memory, rebuilt from durable facts at startup. One server process is assumed.

| Action | Contract |
| --- | --- |
| **Delta:** Receive / accept | Stable receive ID deduplicates the original actor and exact old/new object outcome. Operator/installer pushes run; agent pushes park unless `oberth.run=1`. Agent pushes to the default branch refuse with a promote remedy. Version tags require fresh default-branch ancestry and release admission; ordinary tags use CI. Parked is an outcome, never a run status. |
| Run / check / cancel | C3 prepares once in the receive gate. Acceptance stores runs, receipt, seal, audit and events before submission on server lifetime. Check authorizes exact revision/bounded overlays and calls inspect mode without effects. Branch cancel requests Stop, then Terminate on repeat; C5 chooses Terminate when live native nodes show no execution has started, independent of stored progress. Release/tag/promotion use Terminate immediately. The escalation hint appears only when the fresh run read still reports running. A cancelled verdict names the cancelling identity from its durable obligation and gives restart guidance once: a new push or requested rerun, or a new promotion from the original passed run for promotion work. Only admins force release/tag cancellation; non-admin cancel/rerun is limited to in-scope branch pushes. |
| History / logs | Status, wait, lists, detail and logs resolve the same scoped identity and stored plan. Lists use stored counts. Steps remain in plan order and copy `credential_intent` and `parallel_group` from the stored plan, without exposing a secret name or value. Latest runs are visible; helper containers stay hidden unless requested. The repository list carries, when asked (`recent_runs`), each visible repository's newest 20 runs oldest first as ID, status, ref and creation time, read in one bounded store query per page under the caller's repository scope. C8 chooses failed-then-running default log focus from supplied paths. C2 supplies ReadRequest.Purged from its audit record and PlannedSteps from the admitted plan. Purge is terminal log evidence only, with audited writer fencing and protected-evidence checks. |
| **Delta:** Sync | Publish only the exact current mirror head of one named non-default branch, including parked branches. Refuse default, internal promotion/release refs and ambiguous selectors. |
| Publication | Lock `(canonical repo, full ref)` cancelably, reread durable state, claim the attempt by CAS, then call C6's fenced push/read-back. Different refs proceed independently. Retry exact tested objects without rerunning work. Admin abandonment waits for the same ref's delivery to settle. A moved branch head abandons publication; its run stays passed. |
| Promotion | Promote one exact tested commit. Fresh main-branch policy (`uplinks_may_promote`) decides which uplinks may promote; there is no promotion approval grant. A moved target requires preparation and tests against its new head. Never force a promotion push; reprepare/test at most two successors, then ask to promote again. Fresh default-branch policy decides allowed skip-CI; known red refuses. |
| Issues | Small scoped task list with revision CAS, five-minute explicit locks and staleness computed on reads, under the installation's windows (24 hours; 2 hours for active work) unless the read names its own. A list carries scoped counts (open, closed, each open work status, stale), its total, the page's offset and a cursor each way. CI issues cover the default branch (its pushes, passing promotion runs and, once its publication is delivered, a fast-forward promotion's tested run, counted as the branch's newest result) and schedules; optional workflow files, other branches and tags create none: newer red updates follow C7's person-owned-field contract, and a later pass closes, or the installation administrator with a reason. Assisted, cancelled, restart-cut or eviction evidence changes neither CI red nor green. Agents close manual issues explicitly. |
| Authority / enrollment | Every method uses the authority table below, not tool visibility. An uplink's scope is an upstream, covering every repository under it; invitations never name a repository. Invitation reservation precedes CSR verification; identity/scope come from approved invitation or current certificate. Enrollment redemption returns the server's SSH endpoint within the verified TLS session. Grants are exact file/step/path; revocation works despite invalid unrelated grants and reports incomplete application. C10's per-repository barrier fences authority publication; a saved grant succeeds and reports its schedule resync as a separate warning. |
| Schedules / holds / backup | Delegate schedules to C10; manual force overrides only user pause. Select the live native identity and audit hold actions under a short per-run gate before dispatch; release that gate before native I/O so release never waits for an inspection command. List, arbitrary command inspection and release remain; no continue or file tool. Commands may change files and downstream outputs, as the help states. Native results are authoritative; the original code is diagnostic text. Requesting assist leaves ordinary evidence. The audited exec marks Assisted before dispatch, including a failed dispatch. Finish applies [C5's evidence rule](c5-native-execution.md#accounts-and-holds) atomically; CI projection, promotion and publication refuse Assisted evidence, with the native skip and restart qualifications owned there. List and release do not mark. The inspection audit keeps kind, step, reason, argument count and the SHA-256 of the argument list, never the command arguments. C4's frozen eligibility excludes declared and resolved secret routes without certifying image or shared state. Assist stays off under [C5 Assist enablement](c5-native-execution.md#assist-enablement). Only one backup runs until its stream closes; another receives `backup already running`. |

**Delta:** Promotion grants, reservations and consumption, trains, `prune_source`,
`tag_after` and bulk branch/ref deletion are retired. Exact-tested-commit promotion,
ordinary publication and single-ref deletion remain. Publication lists use creation time newest first, then descending ID, with exclusive ID cursors in that order. Release/tag rerun requires
admin authority and warns about repeated effects, without a `force` argument;
retain its other credential and terminal-attempt evidence checks.

### Administrator-only actions

Installation-wide admin means an admin key with no repository limit. Every
selected repository is checked too. Other scoped actions still check scope.

| Authority | Actions |
| --- | --- |
| Installation-wide admin | upstream add/list/remove/provide-key; repository add/verify/remove/archive/unarchive; admin access/public-identity/readiness/version/idle/audit-record |
| Installation-wide admin | uplink add/scope/invite/cert/list/revoke/rotate; secret access allow/revoke/dismiss; capability and PVC grant/revoke |
| Installation-wide admin | publication abandonment, run purge, installation-wide issue creation, manual-issue import and issue scope moves |
| Admin within repository scope | receive outbox, audit list, schedule pause/resume/trigger |

**Delta:** C9 owns static secret-store setup and checking. Grants change authority,
not store policies. C2 accepts secret bytes through its authenticated writer port;
the server may write or delete step secrets but may not read them. The tool requires
an approved exact grant before writing; the human `secret put` flow grants first.
Audit `secret.put.requested` before the write, using only public selectors. A stored
value is reported as stored even if revoking the writer session fails.

**Delta:** A stored-secret revoke commits the revocation, narrows live authority
and attempts publication before deleting the exact path under both deterministic
CI and release accounts for the same repository, file and step. Each account's
delete is audited as a request with
public selectors and its tier before dispatch. A wildcard grant targets only its
two wildcard accounts; no other step, repository or path prefix is deleted. An
absent value succeeds. If either delete fails, the grant stays revoked and the
caller is told the value is still stored, with the public repair and repeat-revoke
remedy. Repeating revoke retries deletion even for an already revoked grant.
Dismissal is only for pending requests: approved or revoked tuples are refused
atomically with an `access revoke` remedy and no new receipt state. No retained
account binding or second cleanup path is added.

The service owns `InstallAdmin`, one audited call that records the install
administrator and replaces the install key, and exports its tool bindings for C11.

**Delta:** Issue unlock/close-many, trailer processing and branch-CI issue reconciliation
are absent; explicit locks expire and individual issue actions use revision CAS.

### Issue upstreams

**Delta:** Issue upstream names, including viewer scopes, must be registered in
this installation. Import refuses an unknown name with a reason and remedy
naming `repo add --upstream <name>` or `issue import --upstream <local>`.
Only an explicit import `--upstream <local>` maps a source upstream to a
registered local upstream; omission preserves the source name and never guesses.
Creation without `--upstream` uses the repository's upstream only when there is
exactly one. With none or several, refuse and name the available choices.
Every created or imported issue is visible to viewers scoped to its upstream.

### Manual issue import

**Delta:** `issue_import` accepts one neutral JSON Lines file for one-time setup,
only from an installation-wide administrator. Each file has a count header and
disjoint positive issue numbers. Validate scope and the complete file before
calling C7's [saved-issue contract](c7-durable-state.md#imported-manual-issues),
which owns preservation, replay, number allocation and limits.

Dry run validates the whole file and conflicts without writes. C7 commits one
file at a time; there is no transaction over the entire export. Refusals name
the physical line, issue number when known and correction. A lost reply is an
unknown outcome; use C7's replay contract. There is no previous-database reader
or CI-issue import path in the product.

### Run and step projections

**Delta:** C3 calls C4 Prepare once with C5 hooks. C7 stores the immutable plan and native
attempts; C2 projects it without rereading YAML. Logical paths omit Workflow
prefix/retry suffix. Structural nodes do not count. Unreached static entries
stay pending; dynamic placeholders expand in place and terminal remainder is
unknown. Counts cover the full projection independently of paging.

**Delta:** Run status is queued/running/passed/failed/cancelled; native phase remains
separate evidence. display_reason is empty without a specific cause, otherwise at most 40 characters; Detail preserves
full native text. Public records use snake_case and collection envelopes with
empty arrays. object_sha is accepted pushed identity; commit_sha is tested
checkout identity. No alternate verdict or SHA aliases. Publication has its own
pending/delivered/failed/abandoned state. Lost action replies never imply no work.

Release completion shows tag, commit, publishing run and PublicationReceipt
only from a delivered publication row. That same transaction emits
release.published. A passed run without that receipt remains passed, not released.

### Events and observation

**Delta:** The component recording a fact supplies its events in that same C7 call.
C5 records native progress directly; committed changes wake C2 waiters. Each changed projection writes
run.updated with the fact in one transaction, with no coalescing or dirty-revision
replay. Payload is `{run_id,repo,ref,object_sha,commit_sha,data?}`; unknown commit
is null. Optional step_count/steps_planned_exact appear together inside Data.
A monotonic per-run revision gives `run.updated:<run_id>:<revision>` deduplication.
Finish commits verdict, run.finished, pending publication and CI-issue changes
atomically; no crash window can lose their events or publication intent.
Native progress is observation, never an authority audit record.

**Delta:** After commit, wake waiters by closing/replacing a channel; register before read,
then wait, so a concurrent commit cannot be missed. Event reads capture an upper
cursor and scan `(after,upper]`, at most 500 internally and 200 publicly, filtering
scope before output. Advance past examined filter misses; never skip unexamined
matches. Pages carry events, next_after_id, latest_id, oldest_id, has_more and gap.
A pruned cursor requires a snapshot. Progress retention is separately 24 hours
or newest 10,000, whichever removes first. Ordinary events are kept for 30 days;
C11 calls retention once a day. Streams and waits are observations only. Completion/timeout reads
durable state again. sync_status shows current native holders/waiters/stale units only, without stored intervals.

C7 owns the canonical acceptance, stored run and result values in
`internal/stateport`. C2 and C3 use those values directly. C2 `Decide` checks
caller authority, repository state and archive before sealing. Its `Accept`
adds audit and events and makes one store call, without rereading authority,
policy or grants. One batch includes runs, rejected admissions, deletions and
the receipt; deletion cancels active branch runs and records publication intent
in that same transaction. C3 never splits a mixed push into several calls.
C3 implements the service starter port for reruns, promotions and schedule
children; C2 never prepares, creates or recovers a Workflow.

**Delta:** Promotion uses one exact tested source and fresh main-branch
`uplinks_may_promote`. Retire promotion grants and their lifetime settings,
trains, prune-source, tag-after and bulk branch/ref deletion tools, arguments,
flags, records and dedicated events. Retain ordinary publication, promotion
status/history/retry and single-ref deletion. `promote_status` omits `grant_id`
and `source_count`. Admin release rerun warns without force; cancellation keeps force.

**Delta:** Across HTTP, MCP and CLI, run lists/counts/detail and nested
repository latest_run use the canonical admitted-plan projection: logical steps,
native attempts, stored counts, exactness, object_sha and commit_sha. Authority,
outcomes, step order and domain identities remain strict. Audit includes
authority and administrative records only; omit progress and retired
promotion-grant/train/prune/tag-after/bulk-deletion kinds, preserving retained
ordering, identities and chain validity. sync_status exposes current
holders/waiters/stale units without stored intervals or aggregate history.

## Allowed dependencies

[dependencies.json](dependencies.json) permits shared values and C2's issuer.
C2 declares interfaces for Git, persistence/audit, logs/holds, backup, admission,
execution and schedules. No concrete cache, Store, log store, controller,
installer or transport imports. C1 owns schemas and decoding; C11 owns composition.

## Invariants and budgets

- **Delta:** A zero actor has no authority. Check fresh scope before resolution, counts,
  prefixes, pages and metadata; hidden objects answer not-found. Installation-wide
  actions require an unscoped admin. Failures never fall back to bare/unscoped lookup.
- Store accepted facts, audit and events before effects; failure rolls all back.
  Client cancellation does not cancel accepted work. C3 prepares, creates and
  recovers through C5; confirmed absence fails with a rerun reason. API failure is not absence.
  C11 drains accepted submissions before dependencies close.
- Publication attempt CAS precedes external mutation; preserve predecessor,
  accepted object and branch-head fencing. Five attempts use 2/4/8/16-minute
  delays; explicit retry resets the budget but retains prior-attempt evidence, so only the first-ever delivery skips the target preflight. Permission/policy failures are not
  transport retries. Recovery rearms durable retry times. C2 never retries a
  database operation; C7's busy timeout absorbs contention within caller deadlines.
- **Delta:** Typed inputs use C1's standard bounded decoder; null means absent. A supplied
  expected_revision is positive and checked atomically. Collect independent
  offenders; reasons/remedies fit 40/80/120 with detail separate.
- Long polls ≤10 minutes; run/log reads 10 seconds. Pattern scans reserve one
  per credential before one of two global slots. Branch counts are read on
  demand within two seconds, null if unread. Summary pages ≤100 steps; issue
  pages ≤50; events ≤200, repo/run filters ≤32 and Data ≤8 KiB. These bound
  server work, never execution capacity.
- Logs confer no success or replay authority. The Assisted mark from [C5's evidence rule](c5-native-execution.md#accounts-and-holds)
  disqualifies a run's evidence; requesting assist alone does not. CI failures use only the exact failed step's bounded excerpt.
  Purge keeps run/step rows, receipts and the immutable plan.

Core estimate: 4,000–4,800 Go lines, in runs/history, moving-refs and authority
slices. The first design built came out 1.8 times its paper estimate.

## Inspections

| Requirement | Inspection |
| --- | --- |
| [REQ-C2-001](REQUIREMENTS.md#req-c2-001) | `TestValidatedEnrollmentAndIdentityContract` (contract_test) |
| [REQ-C2-002](REQUIREMENTS.md#req-c2-002) | `TestAuditedAdministrativeEffectsContract` (contract_test) |
| [REQ-C2-004](REQUIREMENTS.md#req-c2-004) | `ui` (walk_step) |
| [REQ-C2-005](REQUIREMENTS.md#req-c2-005) | `TestTruthfulConcurrentHistoryContract` (contract_test) |
| [REQ-C2-006](REQUIREMENTS.md#req-c2-006) | `TestExactTestedPromotionContract` (contract_test) |
| [REQ-C2-008](REQUIREMENTS.md#req-c2-008) | `TestBoundedScopedReadsContract` (contract_test) |
| [REQ-C2-009](REQUIREMENTS.md#req-c2-009) | `TestAtomicGrantPublicationContract` (contract_test) |
| [REQ-C2-015](REQUIREMENTS.md#req-c2-015) | `TestNoPromotionBypassThroughSyncContract` (contract_test) |
| [REQ-C2-018](REQUIREMENTS.md#req-c2-018) | `TestCiIssueClosesAfterLaterPassContract` (contract_test) |
| [REQ-C2-019](REQUIREMENTS.md#req-c2-019) | `release` (walk_step) |

`TestBoundedScopedReadsContract` must also try unknown import upstreams,
explicit mapping to a registered local upstream, unknown viewer scopes and
creation with zero, one and several repository upstreams, then check visibility
through viewers scoped to the chosen upstream. It preserves the existing
bounded-read and per-credential capacity cases.

Grant publication inspections also use `TestSecretRevokeAfterReleaseRouteChange`,
`TestSecretDeleteFollowsFailedAuthorityPublication` and
`TestSecretRevokeRetainsFailureFromEitherAccount` for both tier accounts, ordering
and retryable partial failure; the atomic-publication test alone does not prove
the stored-secret delete.

The administrative contract includes `TestExactAdministrativeScopeContract` over
every action and scoped-admin refusal, audit failure rollback and concurrent backup.
History tests restart a waiter, adopt the same UID and fail absent work without
resubmission. Event tests commit between registration/read/wait, crash between
fact and event and check captured cursors/gaps. Publication tests fence a newer
head, moved promotion targets, retries and abandonment. Grant tests suspend all
parents before publishing authority and report incomplete revocation. The release
walk checks delivered tag/commit/run/receipt and exactly one release.published;
a passed run without delivery is not released. UI/log/source walks remain real
integration inspections, without credit from port fakes.

HTTP counts use the complete scoped run inventory. Container inventory and log
windows use the same authorized log preparation as tools. Revocation commits its
exact grant before publication; a failed publication reports incomplete application.
Authority publication has one fenced Publish operation and no cached-generation port.

Promotion selects the newest build.yaml branch result for its commit; an optional
workflow result cannot replace that evidence.
