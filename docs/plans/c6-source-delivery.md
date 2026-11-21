# C6 — Source delivery

## Purpose

C6 retains the exact admitted Git source and delivers a private working copy through a
native Argo input artifact to each consuming Pod. It also owns the bare cache, accepted-push outbox,
exact object reads, promotion checkout mechanics and Git publication operations used by
other components. It does not schedule runs, execute repository commands on the server,
resolve workflow dependencies, grant credentials, install controllers, collect output
artifacts or create source claims and checkout Pods. Repository YAML declares source
inputs and paths; Argo extracts them. Private promotion checkouts serve merge
preparation only.

## Owned data and public contract

C6 also owns runtime `internal/gitssh` and `internal/sshserver`; only C6/C11 consume
them. Pure name/object contracts stay in the shared packages.

The [Shared contracts](README.md#shared-contracts) are normative for shared admission,
plan, step/attempt/count, failure/retry, actor, source, Git, event, identity and
runtime-configuration types. Local descriptions below specify component behavior and use
those definitions; they do not define rival DTOs.

- **Recorded source.** A step gets the tested commit, its run ref, the default-branch
  head at admission and the tag set at admission. Other branch heads are absent.
  Revision, descriptor, binding and receipt values live in
  `internal/sourcecontract`. The sealed prepared admission is the complete source
  description stored with its run.
- **Git retention.** C3 stores admission and the run, then calls its own one-method Keep
  port with the sealed source bindings before native Create. C6 derives roots
  from those bindings and their checkout inventory, verifies and pins them with one
  Git transaction per repository. Run refs use `refs/oberth/keep/<run-id>/<object-id>`;
  Git keeps those objects alive. Release and bounded reconcile stay inside the cache and
  use its existing lock and GC pass, with an injected live-owner reader.
- **Native ownership.** **Delta:** C3 supplies C4's PreparedAdmission as the durable
  source binding; C5 records the created Workflow name and UID once on that run,
  including after crash adoption. Load authenticates its Pod and matches that exact
  native identity and admitted input.
- **Load wire.** Dedicated source HTTPS serves only `POST /api/source/v1/load`. Body is
  `{"locator":"<run-id>","descriptor":{"version":1,"kind":"main"}}`; a pin descriptor
  instead has `kind:"pin"`, `repository` and `commit`. The request locator names the run
  owning the caller Pod's Workflow and selects that run's recorded source binding.
  Locators are public selectors, never credentials. Bearer identity comes only from
  `Authorization`. The bounded standard decoder refuses unknown fields and trailing
  JSON; null means absent and required typed values validate once. one shared decoder
  serves the endpoint, plugin descriptors and receipt objects. Native Workflow decoding
  remains C4's separate contract.
- **Completion and errors.** Success is `application/gzip`, followed at EOF by exactly
  one `Oberth-Source-Receipt` trailer: raw URL-base64 JSON
  `{version,locator,descriptor,stream:{admission_digest,payload:{kind,index},digest,bytes}}`.
  `digest` authenticates transmitted bytes; `bytes` counts regular-file payload, not
  compressed length. Missing receipt means failure even after HTTP 200. Pre-stream
  errors are `{version:1,code}`: 400 `invalid_request`, 401 `unauthenticated`, 403
  `forbidden`, 501 `not_configured`, 503 `unavailable` with `Retry-After`. Other methods
  get 405. After streaming starts, failure withholds the receipt.
- **Plugin process.** `cmd/oberth-source-plugin` receives one absolute Unix socket
  argument. `Load` writes a private adjacent temporary archive and publishes it by
  rename only after EOF, receipt and SHA-256 verification. Argo owns extraction/modes.
  `IsDirectory` returns false; Save/Delete refuse; no output/list/stream backend exists.
  Public executor configuration is `OBERTH_SOURCE_ENDPOINT`,
  `OBERTH_SOURCE_SERVER_NAME` and `OBERTH_SOURCE_CA_PEM`. Identity comes only from
  the Pod-bound token at the fixed path Argo mounts in the plugin init container;
  reopen that path for each request or retry so projected-volume rotation works.
  There is no environment override for the token path.
  The server resolves the authenticated Pod to the recorded Workflow UID. No
  inline token, auth command/provider, arbitrary cluster URL or alternate identity.
- **Git ingress and events.** `Cache` owns canonical bare repositories, private
  tracking/custody refs, public-ref ownership records, managed Git configuration and
  receive reservations saved to disk. `ServiceRunner` accepts upload-pack and
  receive-pack only. `ReceiveEvent` retains stable ID, actor/fingerprint, canonical
  repo, `RefUpdate {ref,old_sha,new_sha}`, assist request and exact `ReleaseAdmission
  {default_branch,sha}`. Callbacks use event ID for idempotency; replay preserves
  original attribution and admission. C6 records preparation and final settlement
  durably; corruption quarantines the affected repository. Ref-change callbacks notify
  consumers after changes; they do not grant source access.
- **Other Git consumers.** Exact blob/policy reads, ref inventories and guarded pruning,
  archive/removal, issue-trailer reads, `PreparePromotion`,
  `RemoteRef`, `SyncBranch`, `SyncTag` and `PushPromotion` remain narrow mechanics. C2
  owns authorization, audit and publication retry policy; C6 supplies exact objects,
  merge/conflict evidence and target checks. Promotion has one source; no train
  port. No new user CLI, MCP tool, run state or source event stream is introduced.
  Existing CLI/API adapters remain in C1/C2/C11. The
  source authorization failure metric uses fixed codes
  `unauthenticated|forbidden|unavailable`. unavailable uses the fixed reason values:
  `export-slot|auth-cap|transfer-slot|other`, with no unlabelled unavailable aggregate
  or identity labels. Counts include post-stream failures, not just retry replies.

### Receive settlement

**Delta:** Every received push ends accepted or rejected; a transport exit leaves
no third outcome. Accepted means admitted, prepared and committed: its exact
objects and attribution are retained and delivered once, immediately after
acceptance, even if its reply is lost or the pusher's connection is gone.
Delivery does not wait for a reconnect or restart. Rejected means
public refs are restored, the reservation is rejected durably and nothing is
delivered. A late client or transport failure cannot reverse an accepted push.

The reservation saves the initial reference values and each receive transaction's
exact prepared reference/value set to disk before the reply. The receive hook
refuses a push that names one reference twice. An update counts as applied
only when its reference changed since the reservation and equals the requested
value. Recovery decides which work was prepared from the stored transaction
sets, never from current refs, a transport exit or an in-memory flag. Retain
accepted events until their consumer has handled them; rollback and restart
cannot lose, replay or misattribute a push.

An unreadable reservation blocks its repository and reports that fact with the
existing receive-outbox repair remedy. Its read view still supplies the record
ID. An authorized administrator may abandon that ID with an audited reason,
following the [outbox repair contract](#git-consumer-interface-c6-owned).
Repair never invents an accepted outcome or delivers unverified work.

### Git consumer interface (C6-owned)

**Delta:** the method signatures, value types and three typed Git errors are pinned in
[Shared contracts](README.md#shared-contracts). `internal/gitport.Git` is the only
cross-component Git port; C11 injects a C6 adapter, and C2 wraps it in its own consumer
interface. No concrete cache import is required by C2, C3 or C10. Pure identity
validation lives in `internal/reponame`.

The Go declarations in `internal/gitport` are the method and value contract;
dependencies.json records their ownership and consumers without repeating them.

Errors are `gitport.AbsentError` (missing file/ref/commit), `gitport.AuthorRefusalError`
(permanent invalid author input), and `gitport.UnavailableError` (I/O, lock or
cancellation). Mixed joined causes remain unavailable; absence never conceals an I/O
failure.

Git push options accept only `oberth.assist=1|0` and `oberth.run=1|0`. `oberth.priority`
is refused. Bare options, duplicate/conflicting options, `true`/`false` aliases and
unknown options refuse with the accepted spellings. `oberth.run=0` expresses no explicit
run request; the key push policy still governs (operator/installer always run, agents
park). Assistance never requests CI.

**Delta:** Receive outbox observation and repair belong to C6, exposed by the
existing C1/C11 tools and commands. Abandon completes rollback of applied
changes from rejected or unprepared work before moving or removing the record
and clearing its repository block. If rollback cannot be proved or completed,
keep the record and block, and show the manual repair remedy.

## Allowed dependencies

C6 uses the exact C6 allowlist in [dependencies.json](dependencies.json). Internal C6
calls are allowed; sourceplugin uses only the source protocol/strictjson and artifact
RPC, kubeconfig parsing, HTTPS and local staging. sourcedelivery uses sourcecontract,
native types and bounded read/auth clients. gitcache uses shared
identity/model/retry helpers and native Git I/O. C6 never imports C1–C3/C5/C7/C9/C10/C11
implementations. Catalog, policy, settlement, persistence and publication ownership are
injected callbacks. C3 owns the one-method Keep port, taking the run owner and
sealed source bindings rather than a separate list of revisions. C6 receives a
live-owner reader from composition. No shared source persistence port is built.
Runtime configuration comes from the strict server configuration,
returned as copied values.

## Invariants and budgets

Use the declared commands of the [paired runtime build](c13-own-pipelines.md); do not
discover the entrypoint of an Oberth image through a registry at execution.

### Source guarantees

- **Exactness:** every step receives exactly the main and pinned revisions admission
  resolved. Each step gets its own private copy of the admitted source, including pinned
  sources, in Pod-local storage. Committed files are delivered without write permission.
  A step may create or replace files in its copy; nothing it does there leaves its Pod
  or changes what another step receives.
- **Retention:** those objects survive branch changes, deletion and GC for the full run,
  native retry and hold lifetime.
- **Exclusive loading:** only a Pod owned by the exact Workflow name and UID recorded
  for the run may load its admitted source.
- **Crash safety:** interruption never changes source identity or leaves an owner pinned
  forever; unknown state and failed reads never authorize deletion.
- **Release:** refs are removed when their owner is terminal.

### Source operation

- **Order:** store the run and complete sealed admission first, Keep all roots in every
  repository next, and Create only after Keep succeeds; crash recovery repeats Keep
  idempotently before adopting a found owned Workflow. It never creates a
  replacement; a confirmed absent Workflow fails with a rerun message.
- **Identity:** C5 records Workflow name and UID exactly once at Create acknowledgement
  or adoption; a replacement cannot rebind the run.
- **Lifetime:** release after the run is terminal and native exit handlers finish; holds
  keep it live. Load returns 403 for a terminal run and retryable 503 before UID
  recording. A rerun owns new refs and says to push again when its recorded commits are
  unavailable.
- **Handoff:** outbox and promotion consumers take their own refs before the run
  releases its refs, without a combined obligation ledger.
- **Recovery:** refuse repository removal or archive while keep refs exist, naming their
  run. After restore, re-pin all stored non-terminal runs before deleting unowned refs;
  a failed read or re-pin stops deletion.
- **Git transaction:** verify admitted commit roots before one update-ref transaction
  per repository under its lock; name refs by object ID and reconcile in the bounded
  existing GC pass.

### Checkout snapshot

**Delta:** record the checkout inventory once at admission: tested commit, the run's own
ref, the default-branch head and bounded tags as of admission, including tag objects and
tags not pointing to commits. Deliver that snapshot without other branch heads; never
refresh its facts at Load. Tag refs keep their own objects alive; Load fails closed if a
recorded tag object is gone. The receipt binds the recorded admission digest and
delivered payload digest.

Submission declares only Keep; cache release and reconciliation use an injected reader
of live run owners.

### Native schedules

**Delta:** Schedule parents stamp locator `scheduled`; Load resolves it to the caller Workflow's run with
every ownership check. Pins refuse. A live owner with gone source stops deletion only in its
repositories. A manual schedule trigger uses ordinary admission; force bypasses no source or grant.

### Git and endpoint budgets

- Authorization has two in-flight calls per verified Workflow and a global
  `SourceClientBurst/3` bound; excess returns immediately. Caller authentication and
  recorded Workflow ownership are checked; the sealed admission decides what source the
  run may load. There is no repeated admission, replay computation or
  authorization-result cache at Load. A busy export refusal does no Git or repository
  locking. None of these limits gates run submission. More than 16 flooding Workflows
  can delay another tenant's Loads; no isolation guarantee is claimed. **Delta:** retain
  one Git/compression slot, release it before network writes, and bound active staged
  transfers separately at two. Archives are request-private, with no persistent cache or
  cross-request reuse.

- Main delivery contains regular files plus sanitized detached `.git` history, index and
  the recorded checkout snapshot. Never export hooks, remotes, credentials, private refs
  or repository configuration. Main skips symlinks/gitlinks; pins reject them and
  contain no `.git`. Pins are private copies at admitted `/work/sources/<name>` under
  the same source guarantees; at most four, each 128 MiB / 4096 files. Main is 512 MiB including delivered metadata. Pack
  output and traversal metadata each retain their 512 MiB guards; no source ref-count or
  undeltified-history cliff. Exact file reads are at most 1 MiB and paths at most 4096
  bytes. **Delta:** pack overflow is a terminal 403, not an endless 503 retry.

- Active snapshots outlive concurrent ref release and GC; repository locks never span
  network sink writes. Cancel, close and join owned workers. Export has one 10-minute
  total deadline and one-minute write bounds; authorization phases have 10-second
  bounds. Reconcile uses bounded owner pages inside the existing GC pass. **Delta:**
  staged archives cap main at 544 MiB and pins at 160 MiB (payload limit plus 32 MiB
  framing allowance). Two main archives plus a 512 MiB pack and 256 MiB metadata reserve
  require 1856 MiB below the chart's 2 GiB default scratch limit. Stream blobs directly
  into local archives; remove Git scratch before releasing its slot. Directories/files
  are 0700/0600. Startup removes owned interrupted exports before serving; cleanup
  failure refuses startup. C9 guards configured chart capacity. A first clone runs under the server lifetime for at most 30 minutes within the one-minute idle bound, one per repository; a caller whose deadline passes is told it continues, and a clone that ends after its caller left announces its refs only while the repository is registered under the same ID.

- Plugin trust is an explicit HTTPS origin/CA/server name, without proxies, redirects or
  system-root fallback. Dial/TLS handshakes have 10-second bounds; response-header wait
  follows Load context. Retry 503 and refused/reset connections before response headers inside Load:
  full jitter, Retry-After floor
  capped at five minutes, cancellation throughout. **Delta:** use a backoff ceiling of
  two seconds; ordinary busy waits are 1–3 seconds. For `POST /api/source/v1/load`,
  capacity replies use Retry-After 1; `other` uses 10. Longer header floors remain
  exact, up to five minutes. No server export queue or template retry requirement. No
  receipt means discard; fixed diagnostics disclose no bearer or raw dependency body.
  A step starting during server restart waits within its own deadline, without
  consuming native retries. Log `waiting for the Oberth server` every 30 seconds.
  Request scratch is removed when that call ends; no cross-call scratch owner is kept.

- Git receives reserve durable actor/admission before mutation, retain gate failures and
  replay before refresh can overwrite attribution. Only public heads/tags may change;
  tags are creation-only and retain raw tag identity. Default/reserved branch deletion
  refuses. Current control-plane bounds stay: 256 updates/receive, 4096 public refs,
  255-byte ref suffix. Bounded deletion recovery and invalid-ref sanitation remain
  safety behavior. These receive bounds are distinct from source-history export limits.

- Older branch results cannot overwrite a newer mirror head. Branch head check and
  publication share the repository lock; branch force-sync is an explicit existing
  operation. Tags and promotion targets never force-push. Promotion retains its exact
  planned base/result, conflicts and object pins; C2 decides whether to
  reprepare/retest. Git commands use sanitized environment and managed no-detach
  configuration. Cancellation preserves its cause, signals only the pinned command group
  and reaps its descendants within the cleanup bound.

Validate names with `internal/reponame`, object IDs with `internal/gitoid`, and refs and
relative paths with C6's validators once before Git commands, file paths or URLs. Reuse
validated values and pass arguments directly; never interpolate caller text into shell
scripts. A started push prints its watch link; a parked push explains `-o oberth.run=1`.

## Inspections

The requirement sentence is the inspection contract. These inspections are planned.
The push inspection interrupts receive before and after durable preparation and
commit, including client loss and transport failure after acceptance. Restart
must settle from the reservation alone: accepted objects are delivered once;
rejected refs are restored and never delivered. Lose the reply after acceptance
and prove delivery proceeds at once without the client; damage a reservation,
inspect its repair ID, abandon it with a reason and prove the repository unblocks.
Prepare several transactions with different reference sets, including a ref
already at its requested value, and verify the applied test and exact delivered
subset. A repeated reference must refuse in the hook. Interrupt rollback during
abandonment and prove the record survives until restoration completes.

| Requirement | Inspection |
| --- | --- |
| [REQ-C6-001](REQUIREMENTS.md#req-c6-001) | `TestExplicitGitIdentitiesContract` (contract_test) |
| [REQ-C6-002](REQUIREMENTS.md#req-c6-002) | `TestFreshUpstreamPolicyContract` (contract_test) |
| [REQ-C6-006](REQUIREMENTS.md#req-c6-006) | `TestPinnedGitInteroperabilityContract` (contract_test) |
| [REQ-C6-008](REQUIREMENTS.md#req-c6-008) | `TestBoundedGitTransportContract` (contract_test) |
| [REQ-C6-009](REQUIREMENTS.md#req-c6-009) | `push` (walk_step) |
| [REQ-C6-010](REQUIREMENTS.md#req-c6-010) | `TestExactRecordedWorkflowSourceContract` (contract_test) |
| [REQ-C6-011](REQUIREMENTS.md#req-c6-011) | `TestValidatedGitInputsContract` (contract_test) |
| [REQ-C6-012](REQUIREMENTS.md#req-c6-012) | `push` (walk_step) |
| [REQ-C6-013](REQUIREMENTS.md#req-c6-013) | `server-restart` (walk_step) |
| [REQ-C6-014](REQUIREMENTS.md#req-c6-014) | `push` (walk_step) |

`TestKeepBeforeCreateAndCrashRetry`, `TestKeepPreservesEveryRootThroughGC`,
`TestRunRefsReleaseAfterExitHandlersAndHold`, `TestPublicationPinsBeforeHandoff` and
`TestRestoreRepinsBeforeDeletingOrphans` cover source lifetime. One Keep contract runs
against its fake and real Git implementation. Load controls change run, Workflow UID,
commit and descriptor separately; a terminal run gets 403 and missing recorded UID gets
retryable 503. `TestValidatedGitInputsContract` supplies argument, path and URL
injection cases and proves no child command or filesystem effect. The `push` walk checks
both the started-run link and parked-push next action. The server-restart walk restarts the server while several real source-consuming
steps start, expects waiting rather than connection failure, and requires the run
to finish green within its own deadlines. Real source delivery includes
writable plugin scratch, main history and pinned-source contents.

## Retired terms

Source manifests and lease journals → sealed admission and Git keep refs;
checked-binding proof → run Workflow name/UID; fragments → native templates.
