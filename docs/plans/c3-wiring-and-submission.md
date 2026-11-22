# C3: Wiring and submission

## Purpose

C3 turns an exact ref update or manual request into one sealed admission, then
keeps its source and starts one Workflow. It also resolves canonical upstreams,
checks repository connections and manages explicitly confirmed host trust.
C2 decides authority before sealing and records accepted work; C4 admits YAML; C5 executes and
observes; C6 owns Git and source; C11 owns health, readiness and metrics.

## Owned data and public contract

C3 owns `internal/app`, its consumer ports and their fake/contract suites.
The [shared Go declarations](README.md#shared-contracts) define shared values.
C3 keeps only a receive-lifetime admission stash and connection probe results
in memory. C7 owns every durable run, receipt, admission and native identity.

### One prepared admission

C3 calls C4 Prepare directly once per admission with Policy and handles
AdmissionError directly.
C3 is the only caller of Prepare outside admission; there is no outside adapter
or injected Preparer. Audit, source retention and native Create consume the
same sealed result; recovery verifies ownership without preparing or submitting again.

### Adapter contracts

| Operation | Contract |
| --- | --- |
| **Delta:** `Admit`, `Check`, `Submit` | Select build YAML for branch, promotion, schedule and ordinary tags; select release YAML or an approved administrator route only from release-tag authority. Read exact commit bytes, with tested commit separate from pushed object. Load current file/tier grants, account and config snapshots; call C4 Prepare once. Check uses inspect mode without writes, Keep, credentials or Create. |
| **Delta:** `Gate` | C6 invokes it with all updates and a quarantine-bound source before any ref moves. Collect every independently knowable finding with its ref/file and C4's exact grant command. One bad ref rejects the whole push. Tags always require admission, including uplink pushes without a run option. Parked/deleted updates skip preparation; absent optional build YAML starts nothing; missing release YAML refuses. A push carries at most one release tag: the commit or one tag object on it. Delivering it upstream is the gate's last step: a failed delivery refuses the push, a delivered tag is never refused, also on expiry. The gate has 30 seconds; a refusal already there when it expires is returned with its own remedy, and otherwise, an approval racing expiry included, it refuses saying admission did not finish in time. |
| `Deliver`, `Forget` | After refs move, check durable receive deduplication and pass the canonical stateport batch, including branch deletions, to C2 Accept once. Replays share one submission worker. A lost stash permits preparation only before the first durable admission; a refusal then records a failed run with findings. Forget drops the stash when the receive ends. |
| `StartRun`, `Recover`, `Close` | Accept, Keep, Create once, bind name/UID once through C2, then apply durable stop intent. Recovery repeats Keep before binding an owned Workflow; it never calls Create. Confirmed absence fails with `submission lost; rerun`; foreign ownership refuses binding; read errors leave the run unbound. Close drains accepted submissions before dependencies close. |
| **Delta:** `PendingGrants` | Inspect every configured build/release/routed file at one default-head commit. Return grant findings on demand. Missing optional files skip; unreadable or invalid files return an error and no partial list. Nothing is stored or refreshed. |
| Release facts | C4 calls `PrepareHooks.ReleaseFacts` after authored admission checks. C3 supplies the stable vMAJOR.MINOR.PATCH tag, peeled commit, exact containing default head from C6, and passing latest admitted build result from existing repository run history. Successful and in-flight release runs define the known version floor; no known release means no floor. A new release refuses a version any earlier release run used. Reruns reuse the loaded original admission, retain its parameters, check the current floor and refuse changed release identity, file or assistance. No new store record is needed. |
| Source binding | C4 calls `PrepareHooks.BindSource` during preparation. C3 binds explicit canonical consumer/producer policy, default deny (a repository pinning its own older commit is a pair like any other; a refused pair prints the installer command that allows it), registered producer identity and reachability from its refreshed default head: the forge's head decides when it answers within three seconds, and so does the head it last answered to an admission read within 15 minutes, unless a refresh or a delivery has since moved the cache's copy of its refs; for a forge that is slow or unreachable, the head the cache holds decides a commit it reaches only while the cache was refreshed from that forge (its address and catalogue ID) within 15 minutes, and any other outcome refuses the pin naming the producer's forge. A pin of the pushed repository's own commit is read from the receive and the cache, never the forge: the cache's head counts only while it was refreshed within the same 15 minutes, and a commit that reached the forge outside Oberth since the last refresh is refused until the cache sees it. C4 attaches template/artifact/path coordinates. |
| `TrustHost`, `Repin` | Scan through an injected SSH scanner. Retain only operator-supplied fingerprints or keys explicitly confirmed with `yes`; unattended `--yes` alone refuses and prints offered fingerprints. Re-pin reports old/new fingerprints, replaces only that address and preserves unrelated bytes; ambiguous host patterns refuse. C9/C2 own identity generation, persistence and audit. |
| `Checks`, `Jobs` | Probe through C6's Git RemoteRef(HEAD), so protocol errors are failures. Return each live probe result without caching; a later success has no stale failure. Pass observation, log, cancel and hold values through shared types. |

C10 calls Admit; a manual schedule trigger is an ordinary submission. Native
child discovery, proof and pause synchronization belong to C10/C5/C2. C3 owns
no public route/tool, scratch directory, status model or publication record.

**Delta:** The submission audit `<ci|release>.argo.submit.binding` is attributed to the run
actor and commits with acceptance. Read its repo/upstream, ref, commit/object,
trigger, file, namespace, accounts, automount, credential declarations, sources,
receipt and exact spec digest from the sealed view. Include no secret bytes.

C7 owns the canonical acceptance, stored run and result values in
`internal/stateport`. C2 and C3 use those values directly. C2 `Decide` checks
caller authority, repository state and archive before sealing. Its `Accept`
adds audit and events and makes one store call, without rereading authority,
policy or grants. One batch includes runs, rejected admissions, deletions and
the receipt; deletion cancels active branch runs and records publication intent
in that same transaction. C3 never splits a mixed push into several calls.
C3 implements the service starter port for reruns, promotions and schedule
children; C2 never prepares, creates or recovers a Workflow.

## Allowed dependencies

[dependencies.json](dependencies.json) lists exact imports. C3 uses shared
actor/admission/source/native/step/config values, C4 preparation and native schema
validation, and C2 service seams. Git uses `internal/gitport.Git`. Source,
authority, persistence, native and observation implementations arrive through
consumer interfaces. Preparation is the direct C4 call, without an outside
adapter. C3 imports no concrete cache, store, schedule reconciler,
installer, log store, watch or health adapter. C11 injects every dependency.

## Invariants and budgets

- **Delta:** Require every port, attributable actor, canonical repository, valid trigger,
  run identity and fixed-length Workflow name before effects. C6's reponame
  validates names; C3 has no rival parser.
- **Delta:** One sealed view supplies the audit, source roots and Create spec. Nothing
  after sealing reloads policy, grants, defaults or templates. Keep takes the
  sealed source bindings. C6 derives retained roots from their
  recorded revisions and checkout inventory, including tested commit, pushed
  object, own ref and default head; C3 does not pass a separate revision list.
- Acceptance failure permits neither Keep nor Create. Keep failure prevents
  Create. C5 settles uncertain Create by ownership/read-back; confirmed absence
  fails promptly, while an I/O error never proves absence. No recovery refires.
- Accepted work runs on server lifetime despite caller cancellation. Push
  feedback waits at most 10 seconds and includes the run/watch link; this does
  not bound accepted submission lifetime. Stop intent survives Create and
  Terminate never downgrades. C11 stops receives and drains Close first.
- Pending declarations are computed without partial success. Source policy is
  at most 64 KiB, 256 consumers and 64 producers per consumer. C4/C6 own document
  and source bounds. Repository probes have a 15-second deadline.
- Every reason/remedy comes from one catalogue and meets the public 40/80/120
  limits. C3 adds no workload cap, execution queue or submission journal.

Core estimate: 1,350–1,600 Go lines. The first design built came out 1.8 times
its paper estimate; an estimate is a range, and built code is measured.

## Inspections

| Requirement | Inspection |
| --- | --- |
| [REQ-C3-001](REQUIREMENTS.md#req-c3-001) | `TestAtomicDeclarationRefreshContract` (contract_test) |
| [REQ-C3-002](REQUIREMENTS.md#req-c3-002) | `TestReviewedEndpointTrustContract` (contract_test) |
| [REQ-C3-003](REQUIREMENTS.md#req-c3-003) | `TestHonestRepositoryProbesContract` (contract_test) |

`TestPreparedSubmissionUsesSealedSpec` compares sealed audit, source and Create
after changing live defaults/templates. `TestPendingGrantsNeverPartial` checks one head, optional files and failed reads.
`TestGateRefusesWholePushListingEveryFinding` and gate deadline controls check
all-or-nothing admission. `TestDeliverAcceptsOncePerReceive` checks replay;
`TestDeliverAfterCrashRecordsRefusal` checks post-crash findings.
`TestKeepBeforeCreateAndCrashRetry` and `TestRecoverNeverCreates` cover each
handoff and adoption without replacement. `TestStopIntentDuringCreate` and
`TestCloseDrainsInFlightSubmission` check stop ordering and shutdown. Trust/probe
contracts include mixed fingerprints, unrelated lines and out-of-order replies.
The push/grant walks prove actual quarantine integration and a repeated push
succeeding after the printed grant command. `TestMessageCatalogue` checks limits.
