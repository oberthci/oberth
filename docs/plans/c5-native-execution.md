# C5: Native execution

## Purpose

C5 supplies C4's native preparation hooks, creates one sealed Workflow, observes
that same object and opens exactly bound container logs and hold operations.
Argo executes the authored graph, retries, locks and TTL deletion. Kubernetes
places Pods and enforces deadlines. C5 owns neither source delivery, durable
history, grants, publication nor a second admission layer.

## Owned data and public contract

C5 owns `internal/argojob`, `internal/argowatch`, native binding values and image
constants. [Shared Go declarations](README.md#shared-contracts) define values;
consumer ports supply native clients, run writes and log capture.

| Operation | Design |
| --- | --- |
| **Delta:** `Hooks` | Transform the admitted native object, verify named account UIDs, append source trust and wrap eligible assist leaves. C4 alone calls Prepare and seals; C5 does not decode or admit authored YAML. |
| `Submit`, `Adopt`, `Cancel` | Post the sealed spec once under C3's fixed-length name, applying an already-saved cancellation as Terminate in that POST. AlreadyExists or an uncertain reply gets one GET and a run/spec-identity check. Adopt reads cache then one live GET on a miss, verifies original UID/binding and never rebuilds. Only NotFound establishes absence. Leave completed Workflows unchanged. First branch cancellation reads live native nodes: Terminate unstarted work, Stop started work; offloaded progress stays Stop. Patch shutdown with a test of exact UID/current mode; Stop never replaces Terminate. No Workflow Delete. |
| **Delta:** `Project`, observation | Map native node boundary paths onto the immutable plan, stripping Workflow prefix and retry-child suffixes. Latest attempt decides; retain attempts and native Pod/container identities. Use plan order, with numeric loop indexes. Dynamic entries expand in place; terminal remainder becomes unknown. Persist each failed step's reason and failure class so earlier failures survive shutdown and restart. Only native shutdown evidence or a previously active node permits a stopped diagnosis; Stop preserves exit-handler diagnoses. Shutdown messages written before Pod creation also identify stopped steps without a previous observation. Decode compressed nodes with Argo's packer; offloaded/missing nodes mean incomplete evidence. |
| Watch | Two native informers for Workflows and Pods, managed fields stripped, 10-minute resync and watch-error health. Expose Workflow, Pods, OnChange and Healthy; reads are read-only. Client-go workqueue handles observation keys and retry after observation I/O errors. Sync before recovery and child discovery. Cache membership grants no authority. |
| Logs | Verify Pod controller UID and node identity before and after opening pods/log. Select a failed init when main never started. Retire live-follow availability for missing sources and retired observers. Archive each finished container within a bounded 30-second attempt; retry only unpublished copies. C8 stores bytes and verifies durable receipts before collection. |
| Holds | List, inspect with arbitrary commands, and release an exactly bound held container. Commands use the step identity and mounts and may change files and downstream outputs; the tool help says so. There is no continue or file tool. C5 owns the paired helper and adds no public endpoint or event cursor. |

Every Workflow C5 submits carries `workflows.argoproj.io/controller-instanceid`
with C9's installation instance ID. Authored metadata cannot select another
controller.

### Executor identity

Ordinary step code can read the executor's Pod token, including through an
output symlink or a parameter-substituted output path. The token fetches its
own run's admitted source. Oberth grants it task-result create/patch in the
pipeline namespace: its own Pod's result or unread results under dash-prefixes
of that Pod's name, as specified below. It reaches no other run's result or source.

A resource step's Pod token belongs to its resource account and also grants
that account's approved resource operations in the capability namespace.
Admission permits no authored container, input artifact or helper in a resource
step, so authored code cannot read that token through these output routes.

Argo mounts `exec-sa-token` in its executor and source-plugin containers; the
authored main container receives no token mount and ambient Pod automount is
false. These mount choices do not keep the executor token secret from the step.
The preparation hook uses the pipeline account as executor account and replaces
that volume source with a projected Pod-bound 3600-second token, root CA and
namespace. Identity evidence records the pipeline account and optional resource
account. Preparation verifies these accounts without a separate executor account.

C5 owns and tests the task-result Role, pipeline-account group binding and one
small cluster admission policy; C9 renders them. The Role permits only task-result
create/patch. For calling Pod name `P` and its last dash segment `H`, a result
with Workflow label `W` must have name `P` when `W == P`; otherwise `P` must
start with `W-` and the result must have name `W-H`. The policy checks both new
and stored results on patch and requires the pipeline namespace and one
non-empty Pod-name authentication extra. Missing optional fields refuse.
This permits unread results for other dash-prefixes of `P`. Generated run and
scheduled-child Workflow names are prefix-free at dash boundaries, so these
aliases name no other Workflow and nothing reads them. The policy matches task
results only, never Pods or controller deletes.
Secret-store login pins its granted audience. Identity projections require a
non-empty audience and its exact grant. Granting the API server's own audience
is an administrator's decision like any other identity grant.

### Release authority

**Delta:** Admission chooses a release step's deterministic account once from
verified release-kind evidence and the operator's exact repository, workflow-file
and step grants, then seals that choice. Native execution uses the sealed
account without choosing again. There is no configured release-role mapping.
Branch CI and ordinary tags receive no release credentials. An identity
projection still requires its independent exact-audience grant.

**Delta:** Configured release identities have no tool, command, configuration,
stored row or audit action. Remove the `release_wif_*` tools and arguments,
`release-wif` command tree and its flags, `argo.releaseWIF` configuration and
generated objects, `release_wif_overrides` records and the repository-wide
account selector; nothing is migrated or aliased.

### Accounts and holds

Each leaf uses C4's sealed selection and identityname for its tier. An ordinary
leaf without selected store grants uses the pipeline account; a granted leaf
uses its selected step account. A resource leaf uses its capability account.
Native execution never selects grants again. CI never selects a release
identity; missing CI accounts refuse without fallback.

**Delta:** Assist remains off under the [enablement conditions](#assist-enablement). Eligible
failed final-attempt containers offer list, exec and release, with no continue
or file tool. An audited inspection dispatch or an armed step whose final result carries a Pod restart recorded on the native node or is neither success nor a positive never-run skip excludes
the run from CI, promotion and publication evidence. Commands use the step
identity and mounts, may change files and
outputs collected downstream, and the help says so. The native result is
authoritative; the original command code is best-effort diagnostic text.
Eligibility excludes declared and resolved secret routes; it does not certify
image contents, fetched values or readable shared and previously written state. Closed hold outcomes and their event kinds are not retained.

C4 computes eligibility over authored effective templates before the assist
transform and freezes it in the sealed plan. Unknown decisions are ineligible;
Assist applies only to steps the plan lists one to one: leaves beneath retried
groups and loop expansions stay unarmed, and every resolved use of a template,
including uses hidden beneath dynamic or bounded subtree placeholders, must be
represented by an eligible plan entry before that template is wrapped. A leaf's
own retries remain supported. C5 records actual arming
on the sealed plan entries. Authored helper paths or environment names remain
admissible as on main and make the colliding step ineligible. Inline steps and DAG
tasks use the same graph node keys; native lookup follows their boundary and
invocation because inline nodes have no template name. Any input artifact
except the product source input, any output artifact or any archive location
makes a leaf ineligible.

C5 wraps the failed container's command with its paired helper, preserving argv,
streams, authored retries and deadlines. It adds no Pod deadline. Defaults are merged only into an eligible template about to be armed; unarmed Workflows keep main's sealed shape. Workflow secret/token/CSI admission is unchanged. The helper owns a monotonic TTL (default 20 minutes, maximum
4 hours). Cooperative release or expiry kills registered inspection process
groups and returns the saved nonzero code. It publishes the stopped signal before killing groups; racing inspections answer closed, not a fabricated child exit. A release reply is written before owner exit. TTL bounds only a functioning
helper's wait; collection, native observation and finalization have no bound
from it. Native lifecycle reasons and exits may differ from the original code,
whose diagnostic text goes only to stderr and may be absent or lost. No closed hold outcome is stored.

Discovery uses main's running and ready hints. Live helper status supplies
held-since and approximate remaining time, or availability unknown. Actions recheck native
ownership and pass the Pod UID to the helper. Start and registration share its
close mutex. An uncertain exec reply is never replayed. The immutable `run.held`
notice is best effort and cannot block native finalization. Asking for assist
alone leaves a run ordinary evidence. An audited inspection exec sets the
monotonic `Assisted` mark before dispatch, even if dispatch fails. At finish, the
same transaction sets it when a sealed armed step has neither a succeeded final native
result nor a positive never-run skip, or when its native FailedPodRestarts counter is positive (a restart the controller recorded on the node). C5 durably retains that restart fact from native status; a later observation cannot clear it. Only native Skipped type and Skipped phase with no cached or retained Pod or exit qualify: the step did not run in this native lifecycle (false when or empty expansion; loops cannot be armed). A Workflow retried or resubmitted natively by someone with direct Workflow authority is outside the boundary; the product never does it, and a source guard rejects native retry/resubmission calls. Omitted, other types/phases and missing results stay excluded. It excludes
CI projection, promotion and publication. Armed steps that all succeeded remain
ordinary evidence unless inspected or the controller recorded a restart on the node. Unarmed steps keep ordinary evidence eligibility regardless of restarts. C5 saves frozen final native nodes before
terminal delivery for armed runs, including a first terminal observation; retry
delivery needs no new native read. Evidence completion stays independent of
terminal acknowledgement. List and release do not themselves set it. `AssistRequested` remains display and arming intent. Push feedback groups refusals by reason across the receive, shows at most four reasons and one and-N-more summary, and keeps each line within 80 characters. It uses an authored name only when the complete name and reason fit; otherwise it keeps the complete reason.

### Assist enablement

This is the single list of conditions for switching assist on. It stays off.
Built, dormant means the implementation and tests are present; it does
not close the separate review or real-cluster checks.

| State | Condition |
| --- | --- |
| Built, dormant | The structural never-run skip is ordinary evidence in this native lifecycle; wording does not decide eligibility. |
| Built, dormant | A restart recorded on an armed step's native node excludes the run, even after success; the retained fact cannot be cleared. Ordinary unarmed steps keep native recovery. |
| Open | Pod replacement can leave the restart counter at zero: deletion may be queued before a failed status write, or a Pod may disappear before Running is recorded. A native fix or different evidence ruling must close both sequences with both reviewers; no Pod-UID journal or second execution record is added to Oberth. |
| Open | Revisit the direct-Workflow-authority retry/resubmission ruling with both reviewers. It remains outside the product boundary; the product's source guard is not proof that an administrator cannot retry a Workflow. |
| Open | Both reviewers review the enablement change itself. Inspect-only remains; continue in place does not return. |
| Open | Complete the cluster walk with the paired helper and real source: command fidelity, endings, identity, discovery, restart, secret routes and composition. Measure helper exit, native collection and finalization separately; helper TTL does not bound finalization. |

## Allowed dependencies

[dependencies.json](dependencies.json) gives exact imports. C5 uses shared
admission/identity/native/source/step/config values, native clients, its watch and
C8 log capture. Only C4 native-schema validation is callable; no Prepare,
source runtime, concrete Store, service, transport or installer imports.
`internal/argowatch` has no application imports. C11 injects implementations.

## Invariants and budgets

1. Audit, retained roots and Create use one sealed admission. C5 returns native
   name/UID for a write-once C2/C7 binding at acknowledgement or adoption. Until
   binding, source Load returns retryable 503. Recovery never posts replacement work.
2. Argo Succeeded stays passed even with tolerated failed nodes; under shutdown its root must have succeeded. Other shutdown verdicts are cancelled; killed steps keep native exits as evidence, not author failures. Missing exit
   evidence is unknown. Run failure is infrastructure only if every failed step
   has typed infrastructure evidence. No message parsing or reboot inference
   grants retry authority; retry hints are advice only. Cancelled runs have no run-level failure or retry hint; step diagnoses remain evidence.
3. **Delta:** Preserve authored commands, resources, placement, locks, retries and deadlines.
   Argo's `failedPodRestart` is on with `maxRestarts: 1`. Oberth adds no
   automatic infrastructure retry or run cap.
4. **Delta:** Append `safe.directory=/work/src` once at the next GIT_CONFIG index for main
   source consumers, preserving authored values; pins receive none. Use declared
   Oberth image commands without registry metadata lookup.
5. Observation reads informer caches; a miss gets one live GET. API errors do not
   establish absence. Stop/Terminate keeps observation through exit handlers and
   holds. Native TTL defaults to one day. The run-record finalizer blocks deletion until the final record and every archive receipt are durable, even after the evidence deadline. Terminal nodes without a Pod get a durable never-ran or Pod-missing receipt after a live absence check; captured partial receipts remain readable and collectible. Native TTL collects owned Pods and ends their PVC holds. Authored Pod GC is replaced by collection at Workflow TTL, with no post-finish restoration. Finished-Workflow collection has its own observation worker, retry queue, one copy worker and shared API client budget of five requests per second with burst one. It admits at most eight Workflow attempts per 15-second pass and eight live Pod lookups per attempt, at five collection reads per second with burst one. Collection cannot occupy the live observation loop or its two capture workers; live observation keeps its existing client budget. Newly settled live observations hand off their evidence deadline: only an actual archive failure after it invalidates evidence, once; waiting for collection execution does not. Their existing transient attempt bound precedes historical archive backoff. Replay cannot take ownership while the live observer still tracks the run. Captured node receipts skip live GETs and an attempt reuses its first Pod lookup. Unarchivable sources back off from one minute to one hour even through watch events, with a visible run-log archive deferred reason and retry delay. Historical collection only archives logs and patches retention; it never reprojects steps, rewrites captured log attribution or changes evidence completeness. Shutdown leaves Workflows intact.
6. Reasons are closed phrases of at most 40 characters with full native text in
   Detail. Held-step inspection commands are bounded by their deadline and output
   limit. C4 owns spec/plan bounds.

| Typed evidence | Reason | Failure class |
| --- | --- | --- |
| no node / Pending | `waiting to start` | — |
| Pending for the run cap | `waiting for a run slot` | — |
| Unschedulable / synchronization wait | `waiting for a node` / `waiting for sync limit K`, `waiting for mutex N` (N is the lock part after the repository segments; `waiting for a lock` when the name is not plain or does not fit) | — |
| image pull errors | `image pull failed` | unknown |
| ContainerCreating / PodInitializing | `starting containers` | — |
| main exit N / OOMKilled | `exited with code N` / `out of memory` | author |
| DeadlineExceeded | `step timed out` | deadline |
| failed source init / authored init without a server receipt | `source delivery failed` (or `failed before starting`) / `init container failed` | author / author |
| NodeShutdown / DisruptionTarget | `node shut down` / `Pod disrupted` | infrastructure |
| Evicted without typed cause | `Pod evicted` | unknown |
| Stop / Terminate, including exit 143 | `stopped` | cancelled |
| failed/error without exit or Pod | `ended without an exit code` | unknown |
| Skipped / Omitted | `skipped by condition` / `omitted` | — |
| terminal generic start; authored containers never ran | `failed before starting` | infrastructure |
| failed source or secret request with an exact server receipt | `source delivery failed` / `secret delivery failed` | infrastructure |
A run is queued until one of its steps has started (a node of it ran or
failed, in any attempt; a skipped step has not) and running from then on,
never queued again; its start is the earliest start of a started step. A
queued run's reason is its first waiting step's reason (a lock, a run slot,
a node, `image pull failed`, `starting containers`), else `waiting to start`;
`unknown` is passed over. While queued it keeps the most advanced reason seen:
every waiting reason < starting containers or image pull failed; within either
rank the newest reason shows. Progress writes no event and no
control revision.
A Pending step whose Pod ran, exited, was evicted, disrupted or timed out says `starting containers`
until its node phase advances; waiting for a node and image pull failures keep their waiting reasons.
Step reasons follow the displayed status after evidence merges; newer Pod exits cannot label queued or running steps as passed or failed.

A server receipt binds run, Workflow UID, node, step path, physical Pod UID and native Pod restart counter. Only the server records its own terminal delivery failures in C8, never a retryable refusal. A later successful delivery of the same kind supersedes the failure, including across restart and reordered writes. The native failure must fall during delivery or within ten seconds after it (native timestamps have whole-second precision); a live source Pod must have a failed source init, confirmed by a fresh owned Pod read when its cached status lags. Database reads recover within the delivery request deadline before a terminal refusal; step text, images, container names and client-supplied records grant nothing. Failed source/secret steps without a receipt stay author; cancelled and deadline outcomes retain their native class. The never-started fallback requires native waiting states with no prior start or
restart. Image, deadline, node and authored-init diagnoses retain their typed
reason, failure class and retry hint.

A plugin-socket failure caused by a source-template result reference is author
failure. Waiting for a restarting server belongs to the C6 plugin, within the
step deadline; it does not consume the author's native retries.

Core estimate: 2,200–2,650 Go lines. The token swap and token rotation require
real walk proof.

## Inspections

| Requirement | Inspection |
| --- | --- |
| [REQ-C5-002](REQUIREMENTS.md#req-c5-002) | `TestPrivateExecutorAuthorityContract` (contract_test) |
| [REQ-C5-006](REQUIREMENTS.md#req-c5-006) | `TestTruthfulNativeObservationsContract` (contract_test) |
| [REQ-C5-007](REQUIREMENTS.md#req-c5-007) | `TestSeparateCiAndReleaseIdentityContract` (contract_test) |
| [REQ-C5-008](REQUIREMENTS.md#req-c5-008) | `ui` (walk_step) |
| [REQ-C5-009](REQUIREMENTS.md#req-c5-009) | `failing-build` (walk_step) |
| [REQ-C5-010](REQUIREMENTS.md#req-c5-010) | `release` (walk_step) |
| [REQ-C5-011](REQUIREMENTS.md#req-c5-011) | `push` (walk_step) |

`TestProductMountsStayInsideAdmissionRoots` proves every mount native execution adds is under a root in admission's exported `ReservedMountRoots`.

The executor contract applies the hook through Argo's Pod patcher and checks
mounts and projected sources. `internal/argojob/argojobtest` holds the task-result policy and access oracles.
`TestTaskResultPolicy` compiles CEL and checks own
result, foreign result, missing extras, UPDATE relabeling, plain Pod UPDATE
without subResource and unmatched DELETE. `TestWorkflowNamesPrefixFree` checks
run and scheduled-child names. Submit/adopt/cancel tests prove one POST, ownership,
absence and monotonic shutdown. Projection tests cover retry folding, order,
dynamic remainder, typed failure classes, missing evidence and short reasons.
`TestBInspectionSurface` permits arbitrary executables, redirection and file changes;
`TestBInspectionSurfaceContract` checks the tool help and removed tools.
`TestBOriginalDiagnosticIsNotNativeExit` keeps native results authoritative when
the original diagnostic is present or absent. `TestBAllResolvedSecretRoutes`
checks declared and resolved secret exclusion without certifying residual state.
`TestBExecAdmissionBarriers` checks start/register against close, and
`TestBImmutableHeldEventRealStore` checks immutable notices against the real store.

The executor authority inspection also uses `TestTaskResultTokenIsConfinedToItsOwnPod`
and `TestTaskResultRoleHasOnlyNativeExecutorPermissions` for API authority, and
C6's `TestExecutorTokenFetchesOnlyItsOwnRunsSource` for source authority. The
mount-shape test alone does not prove either boundary.

The walk plants an output symlink, uses the leaked token against another run's
results and expects refusal; proves Argo accepts the token swap and rereads a
rotated token; checks completed Pod labels and no controller admission refusals;
restarts the server during a multi-step source run; and exercises adoption,
Stop/Terminate and logs. Held-step activation checks are listed only under
[Assist enablement](#assist-enablement).
