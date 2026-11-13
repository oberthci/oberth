Native submission: `TestNativeArgoCapDoesNotHoldBackAnyOfTenSubmissions`
submits all ten accepted runs while Argo owns its controller parallelism limit.
Authored Workflow/template parallelism, requests and placement pass through
the compiler; Oberth does not compute run demand or a concurrency receipt.

Schedule entry identity: `TestScheduleEntryCronAndManualKeepTheSameResolvedIdentity`
compares cron and manual enqueue on one SHA and preserves a plan after file edits.
`TestScheduleEntriesAreIndependentAndSurviveRetryRecovery` uses SQLite to prove
entry isolation, immutable identity, retry/rerun and stranded-run preservation;
`TestScheduleEntryMigrationPreservesLegacyIdentityAcrossReopen` upgrades an old
schema without inventing historical names. Schedule-entry service/API tests,
CreateCI/adoption adapter tests cover durable forwarding.
Workflow builder/admission tests reject repository defaults and non-schedule
forgery, distinguish two entries, and refuse mismatched restart adoption.

Release chart digest #1181: `TestReleaseChartPackagesPublishedImageDigest`
runs image publication and chart packaging with real Helm, then checks the
archive's default `image.ref`, rendered digest and unchanged source values.
`TestReleaseChartRejectsPackagedSeedDigest` substitutes an archive containing
the source seed and requires packaging to fail before accepting that archive.

Promotion recovery #752: `TestListPromotionsFindsPendingAdmissionBeyondRecentGlobalPage`
proves exact filters precede pagination, cursor pages preserve oldest pending
admissions without runs, defaults/max bounds hold, and malformed filters fail.
`TestPromotionListRecoversLostIDWithoutMutation` invokes the actual tool/service
against SQLite with the mutation gate blocked, recovers the admission ID and
proves audit/Git/gate remain unchanged; unknown repositories, invalid selectors
and extra input fields fail. Tool-surface/count expectations add the documented
read-only tool while preserving every previously registered tool.

Run list views: `TestListActiveRunsOrdersByStartThenAdmissionThenQueue` puts
an older claimed waiting run after newer started runs and ahead of the queue,
never in history, and applies ref and uplink repository restrictions;
`TestListRecentRunsTerminalPagination` pages history past interleaved active
runs. `TestFirstRunListReadReportsNativeQueueAndSuspensionWithoutStepHistory`
and `TestRunListsObserveArgoWithoutDependingOnLocalStepHistory` read native
Argo queue/suspension state. `TestExecutionDisplayNamesTheSynchronizationLock`
names native lock waits; `TestScopedRunListUnknownPositionDoesNotInventQueueHead`
withholds global sequences and positions.

`TestRunListMCPMatchesHTTPOrderAndPassedAlias` compares
the default MCP/HTTP ordering and preserves the passed-status wire alias.
`TestRunsPageIsOneFlatFilteredList` pins the flat dashboard: grey queued/waiting pills, plain-language wait reasons, no position number, head badge or red tint on queued rows.
`TestFlatRunsUseOneSnapshotAcrossAdmission` admits a queued run between the two
list queries and proves that the response contains it only once.

CI issue policy #1170: `TestRunIssueProjectionPolicy` covers default branches
(including a configured non-main default), scheduled feature branches, and
excluded feature, promotion, plan/apply and tag runs.
`TestPromotionDoesNotRegisterCIIssueWork` covers both promotion admission paths.
`TestCIIssueProjectionTakesNoLock` verifies another actor can append immediately,
projection never creates or takes over a lock, and green preserves existing
locks. Atomic run/publication tests retain rollback, ordering and deduplication
coverage. Legacy promotion projection tests explicitly seed pre-policy issue
work to cover rollback and proof-aware ordering;
`TestRestartInterruptedPromotionDoesNotProjectIssue` seeds that same legacy
work and proves interrupted restart recovery creates no CI issue. Control-plane
fixtures use the default branch for issue-bearing status and publication checks; promotion
failures retain durable results without filing issues. Legacy issue reconciliation
remains covered without a schema migration.

Run cancel and supersede submission release:
`TestSupersedingAWaitingRunReleasesItsReservationWithoutDrain` uses real
SQLite and a fake engine to supersede work during preparation, release its
submission ownership and let other runs proceed without restarting.
`TestAdoptedAdmissionIsIdempotent` proves adopted ownership is counted once
and does not delay new submissions.

Store tests (`run_cancel_test.go`) cover queued/waiting/running/
terminal cancels, the no-successor obligation and its Job name, no re-fire by
owner restart recovery, no CI issue, promotion failure blocking publication,
pending-publication refusal and reason bounds. Service tests
(`run_cancel_test.go`) drive `run_cancel` end to end: queued runs are never
claimed, a cancelled waiting run releases submission ownership at once, a running
run's Workflow is deleted and the owner never overwrites the cancel, the
permission/force matrix, idempotent refusal, promotion cancel without merge or
push, and no re-fire across a restart while an uncancelled stranded run is
requeued. Scope tests treat `run_cancel` as a scope-checked mutation; CLI
tests cover MCP forwarding, `runs cancel` selection and in-pod admin-socket
wake.

Watch ownership #671: adoption tests exercise complete preflight before any
mutation, metadata-only JSONPatch UID/RV tests, replacement between read/patch,
foreign/partial owner and scope/spec/target/expiry/duplicate-input negatives,
partial-response retention and exact public subset reporting. Certificate-only
bundles accept a public self-signed origin leaf and reject private key blocks.
The upgrade regression first failed on the old enablement pin; it now preserves
stored enablement while immutable image overrides remain forbidden. Real chart
renders cover absent/false/true enablement, invalid types and separate origin CA.

Adoption boundary regressions reject chart/image/policy/unrelated-values
overrides and non-Linux bootstrap, retain public input through sealed memory
despite caller-file replacement, exercise stdout/stderr bounds with actual
subprocesses, reject malformed first PEM blocks even before valid certificates,
and detect drift in already-owned objects during partial forward repair.

Private routing #764: `TestPrivateRoutingAllShapes` and
`TestPrivateRoutingCacheFailures` cover private root/unknown/nested modules,
malformed requests, unsupported and missing versions, and every cache fault.
`TestLatestAndListUseSupportedTags` checks canonical tag-only resolution.
`TestGoResolverPrivateRouting`, `TestGoResolverLatestEndpoint`,
`TestGoResolverPublicFallback`, `TestGoResolverOriginal404Control` and
`TestGoResolverUntrustedMirrorStops` run actual Go subprocesses with cold caches
against verified TLS loopback mirror/public endpoints and a non-forwarding TLS
CONNECT trap: private coordinates never reach either fallback, the original
404 control reaches both, and public availability behavior is preserved. The
fixture disables public sumdb and VCS access to prohibit external traffic; the
production environment test separately verifies public sumdb is untouched.
`TestGoProxyEnvironmentPreventsAmbientBypass` checks CI/release and inline
containers override ambient proxy values while preserving public checksum policy.

`TestClientRejectsAmbientAuthorityOnEveryRequest` (#770) asserts actual verified
TLS 1.3 seal-status, login, KV read and revoke requests under hostile synthetic
Vault environment metadata, set both before and after client construction.
Case variants, namespace, token, arbitrary headers and wrapping are covered;
the deliberate SSRF header and independent concurrent session identities remain
intact, and the client must not mutate the process environment.
`TestClientSDKConstructionErrorsDoNotReflectAmbientInput` checks forbidden
header names and malformed environment configuration at constructor and clone
boundaries: fixed bounded errors, no reflected marker, and zero HTTP requests.

Watch CSA recovery #671 (prepared, all UNRUN): strict chart YAML must retain the
four connector identities without duplicate labels. V2 tests require only the
sole legacy Update apps/v1 fetch-token.args leaf to be relinquished; foreign,
shared, atomic, malformed and drifted ownership fails. Exact structured409
causes, all-four identity preflight, full metadata UID/RV CAS, expiry/revision
guards and retained partial/uncertain receipts prevent unqualified Helm.
The `watch_real_api` regression uses genuine CSA/API1.36.2/Helm4.2.3 with the
same private effect engine: original six conflicts and actual FAILED72,
corrected one conflict, persisted metadata-only subtraction, all-four SSA
Force=false dry-runs and ordinary deployed73 convergence on the same UIDs.
The synthetic fixture does not qualify signed released-installer admission or
live rollout. [The catalog](docs/backup-restore.md) records that boundary,
the remaining production proof and exact source-bound CPU2 custody/build gates.
V5 source regressions additionally require one frozen API/Helm identity,
impersonation/hook refusal, static public credential path-replacement binding,
kernel executable-inode binding, observed fieldset ordering and complete
metadata preservation. These remain UNRUN and confer no execution admission.

Artifact downloads #1011: `TestBinaryArtifactsRoundTripByteIdenticalOverHTTPCLIAndMCP`
serves 5 MiB and 64 MiB pseudo-random binaries (NULs, invalid UTF-8) from a real
artifact store through the production service and handler, and requires exact
size and SHA-256 over raw HTTP, `oberth artifacts` stdout and `--output`, and
`oberth mcp call artifact_get --output`. `TestArtifactDownloadsFailOnDigestMismatchOrShortBody`
proves a wrong, malformed or missing digest and a short body fail every save
path with no file left. `TestOversizedArtifactCollectionStoresNothingToDownload`
covers the per-run collection limit, and `TestArtifactDownloadAbortsInsteadOfEndingShort`
proves the handler aborts rather than ending a short body cleanly.

Technology-specific execution is refused by current validation and recovery;
its former guest/QMP execution tests do not describe the current native model.

Workspace cache mode #1093: `TestWorkspaceCacheMountsOnlyTheCache` builds a
`work`-VCT pipeline with delivered artifacts and proves a
`workspace-mounts: cache` leaf (with and without `workspace-env: none`) gets
only the read-only source, per-Pod private scratch and the writable `/work/cache`, the
three cache variables, no tool/Trivy/artifact defaults, while a sibling keeps
every default. `TestWorkspaceCacheStaysOnItsTier` proves CI and release cache
leaves mount only their own root and never reference the other, and a release
tier without a root mounts nothing. `TestWorkspaceCacheWithoutTierRootKeepsAuthoredCaches`,
`TestWorkspaceCacheOnCredentialedReleaseLeaf`, `TestWorkspaceCacheRefusedOnNonLeaf`
and `TestWorkspaceCacheCoversEveryPodContainerShape` cover the cold fallback,
credentialed release leaves, Pod-leaf placement and every container shape;
`TestWorkspaceCacheRefusedOnCredentialedCILeaf` refuses `cache` on a CI-tier
`secretstore exec` leaf, admits `none` there, and admits `cache` on the same
leaf on the release tier.
`TestAdmitWorkspaceMountsValues` refuses unknown values;
`TestAdmitNonrootRefusesWorkspaceCache` refuses `cache` on leaves that take no
host cache; `TestTrivyGuardWorkspaceCache` isolates scratch storage per Pod and
`/work/cache` as shared for cache leaves.

Parallel CI race groups: `TestOberthRaceGroupsCoverEveryPackageExactlyOnce`
compares the store/service/gitcache/remainder partition with the actual `go list ./...`
inventory and refuses missing or duplicate packages.
`TestOberthRaceGroupLauncherPreservesChecks` records actual Go invocations,
retains race/verbose/uncached execution and the explicit timeout, and proves a
newly listed package joins the remainder.
`TestOberthSplitBurnsUseTheRaceGroupLauncher` checks the admitted Pod commands,
CGO and processor limits; the existing graph and isolation contracts cover
all four race burns and require each one before every cross-build.
The Git-cache wrapper retains its static package contract before invoking its
dedicated race group; that package must not also run in the remainder. `TestOberthReleaseRaceGroupsMatchBranch`
requires the release gate to fan out into those same four groups, with matching
commands, watchdogs and resource envelopes and separate read-only tool copies.
The launcher pins race detection, uncached execution and two parallel tests per
package for both branch and release runs.

Audit read surfaces (#1104): HTTP and MCP must agree on filtered entries and
paging, and refuse scoped and non-admin callers. Status reads the latest backup
completion record's archive hash, timestamp and snapshot head, omitting it for
scoped callers. Upgrade completion is recorded only after verification; audit
record failures warn without failing a completed upgrade. CLI tail tests cover
appends between head and page reads, bursts exceeding 200 matches, and appends
during pagination that must remain eligible for the next poll.

Branch Workflow adoption #1172: startup restores each existing branch Workflow
under its durable run ID, queue/admission sequences and exact execution binding,
including Workflows completed
while the owner was down. Missing Workflows still requeue; API and binding
refusals fail only their run; Workflow read failures preserve ownership. Shutdown preserves new and adopted non-terminal
branch Workflows and their workspaces. Explicit cancel and supersede still
delete adopted Workflows. Resumed observation retains progress and log bytes,
replays only unrecorded steps, enforces the remaining aggregate budget, and
refuses Workflow UID/spec replacement. Executor identity delivery resumes for
Pods in the token waiter and skips Pods past it. Idempotent adoption cannot
double-count submission ownership or block new submissions.
Transient Workflow read failures are retried within a bounded grace; a
persistent observation failure deletes the Workflow and fails only that
adopted run while the scheduler keeps dispatching. A terminal Workflow whose
observation errored keeps its real result and collects artifacts. Refused
adoptions record a failed run with the exact `<namespace>/<name>` to inspect,
without deleting it or stopping adoption of other Workflows. Native
suspend/resume/stop/terminate preserve the recovered execution identity.

Retry verdicts #1176: `TestDAGRetryThenPassIsPassed` reproduces a retried
DAG whose first attempt failed and second passed: the steps carry the planned
names, the run is green, and the failed attempt stays observable as superseded.
`TestDAGRetryEveryAttemptFailsIsFailed` and
`TestPodRetryEveryAttemptFailsPointsAtLastAttempt` keep exhausted retries red
with FailedStep and the row taken from the last attempt, even when it failed
at an earlier step. `TestNoRetryStrategyIsUnchanged` pins pipelines without a
Retry node. `TestNestedRetryInsideDAG` covers a retried pod inside a retried
DAG inside a burn. `TestPublishReportsSupersededAttemptOnceThenFinalVerdict`
reports and replays each attempt once, with no retry suffix in any key, and
not again from a restored reporter; `TestWaitResumedRetriedDAGIsGreen` adopts
such a Workflow after a restart and reports only the passing retry.

Helper mount isolation: `TestAuthoredMountsCannotShadowServerHelpers` covers
literal, normalized, runtime-templated, parent, nested and conventional alias
paths across all admitted Pod container forms. `TestAuthoredBlockDeviceCannotShadowServerHelper`
extends the boundary to block devices. `TestBuildProtectsHeldPodHelperFromAuthoredShadows`
checks actual held-Pod construction, and `TestExistingHeldPodShadowIsNotPatchCapable`
checks retained Pods and release through the image fallback. Segment siblings
remain supported and existing own-PVC device admission stays unchanged.

Capability recovery: `TestWorkflowCapabilityUIDMigrationDiscardsUnboundReceipts`
runs the forward migration twice, discards UID-less authority and keeps grants,
runs and audit history. `TestOldDeviceGrantWorkflowRecoversThroughRealMigration`
first proves that real migration refuses an old UID-less receipt, then uses an
explicit trusted UID-bound test fixture to exercise adoption and token recovery.
A same-name replacement, missing UID, changed tuple, grant epoch/revocation and
same-ID requeue cannot inherit authority; new submissions remain strict.
`TestStartupAdoptsOtherWorkflowsAfterOneBindingRefusal` proves startup and
other adoption continue; `TestAdoptedNativeControlsPreserveRecoveryIdentity`
covers native controls after restart.

Promotion/tag/release adoption #1172 slice 2: each trigger keeps the same run,
queue/admission order and credential classification through two owner restarts.
Tests cover live and finished-while-down Workflows, persisted steps, no new
submission, retained active workspaces, and terminal run/promotion cleanup.
A moved-main case changes the target after adoption but before completion and
refuses the push. Post-push/pre-finalization crash tests exercise the real
durable publication outbox: the next owner recovers the same ledger row without
adopting, creating, deleting or pushing again, and preserves annotated tag object
versus peeled commit identity. Missing Workflows still interrupt these triggers
without re-fire. Historical non-release tag Jobs retain their interrupt path;
app adoption refuses to infer release capability from a tag name. Newly
created and adopted shutdown tails preserve their
Workflows and promotion workspaces. A non-adopting engine retains all original
conservative recovery assertions; only its capability fixture changes, because
capable engines now adopt these triggers. Store tests retire old owner_restart
obligations for all triggers but keep explicit cancellation obligations and
immutable promotion ownership. App tests reconstruct exact trigger/source/
tested-SHA bindings; actual server-built credentialed promotion and WIF release
Workflows survive changed current identity/policy configuration unchanged, and
resumed Wait refuses changed service accounts, WIF projections/environment,
Workflow UIDs or spec identities. Existing resume/log-budget, executor-token,
publication serialization and migration assertions remain.

Retained timing #1128: `TestTimingPaginatedLargeDAG` collects 65 leaves in two
bounded LIST pages; the blocking transport test retains its five-second API
ceiling, failed verdict and exit codes, and now proves an expired read context
cannot suppress durable explicit unknowns. Snapshot tests refuse partial pages,
API errors, continuation cycles, changed resource versions, duplicate Pods and
page bounds. Cross-attempt tests retain exact owner, Workflow UID, Pod/name,
node and namespace refusal. Init timing and logical readiness tests preserve
mixed script / unobserved image-pull semantics and exclude scripts/expressions.
Store tests cover atomic claim/grant/terminal timestamps, repeated requests,
parked admission episodes, write-once facts, immutable Workflow/Pod/container
attempt bindings, restart re-fire separation with inherited admission order,
TTL-independent reopen, legacy StartedAt ambiguity and no-backfill migration
replay. Native run_get returns the durable versioned observations without a
cluster. Existing migration fixtures drop timing objects before reconstructing
older schemas; exact schema, ledger, gap and preservation assertions remain.
Pod termination diagnostics retain exact run/Workflow/Pod identity and exclude
free-form messages. Artifact tests
preserve user files, enforce bounds/collision/tier gates and retain diagnostics
when user collection fails. Chart tests pin the separate Event LIST Role and
binding while retaining frozen Role assertions.
`TestAdoptedWaitRetainsTimingWithoutDuplicateAttempts` drives a pre-restart
owner and a resumed adopted Wait against the real store: the Workflow binding
and every node keep exactly one attempt record, pre-restart observation times
survive, and a GC'd Pod cannot downgrade retained evidence. Migration v30
stacks after v29; ledger, gap and v29-to-v30 replay assertions are unchanged.

# Restart handover (#1172 slice 4)

`internal/api/handover_test.go` verifies immediate readiness refusal, including
an in-flight readiness probe, rejection of new mutations, JSON/SSE reconnect
responses and joined observers, and completion of accepted mutations during
drain. `TestHTTPHandoverBoundsStalledRequests` verifies forced connection closure
at the HTTP drain deadline. Scheduler readiness becomes true only after recovery
and false on exit; readiness snapshot tests forbid per-request probes and accept
neither stale nor failed samples. MCP and JSON client tests exercise 503,
connection loss and truncated bodies while preserving exact targets and original
deadlines; mutation and unknown-tool requests stay single-attempt.
`TestMCPReconnectAllowlistIsReadOnly` cross-checks every reconnectable tool
against the server's tool list and audit mutation gate, and
`TestMCPReconnectShrinksLongPollTimeout` checks that a reconnected `wait`
asks the server for less than the remaining client deadline. Retry budget
tests cover cancellation, expiry, the 120-second default and configured
windows, and dial timeouts as transient. `hack/test-chart.sh` checks the
75-second termination grace default, also under reused values, and rejects
one shorter than the drain. Idle-wait tests observe queued and running
work, fail closed, and bind the upgrade check to the selected Deployment.

# Retired execution recovery

Old preflight execution is terminalized rather than reattached. Native
execution keeps exact source, UID, target and operator identity custody;
historical execution journals do not authorize new work.

Upgrade idle-probe retries: `TestUpgradeWaitRetriesTimeoutBeforeUpgrade` uses a
fake HTTP transport and clock through the real installer wait path: a five-second
probe timeout followed by idle reaches Helm upgrade, while persistent timeouts
consume exactly the two-minute wait budget, refuse upgrade and preserve the last
probe cause with throttled diagnostics. `TestWaitIdleTransientFailuresBackOffAndRecover`
checks transport/503/truncated-body recovery and backoff reset on a valid busy
sample; protocol-error and cancellation tests pin immediate refusal and prompt
cancellation. `TestIdleSnapshotAdminProbeDoesNotWaitForStorage` blocks a background
read while 100 admin probes answer from memory, then proves expiry, refresh and
failure invalidation. Missing/invalid/late samples cannot establish idle.

`TestUpgradeWaitDeadlineRetainsChildProbeDiagnostic` splits child output at every
byte and proves the local deadline retains its last reported probe cause even
when kubectl is killed; a subsequent busy snapshot clears that stale cause.


Idle sampler freshness follow-up: `TestIdleSnapshotSlowReadsRemainAvailableToWaiter`
uses 4.5-second reads at six waiter start phases; the waiter gets idle within
seven seconds, including initial retry backoff. `TestIdleSnapshotExpiresDuringStuckRead`
keeps the ten-second freshness bound even if a reader ignores its five-second
deadline. Store queue-reader tests hold the primary transaction, observe only
committed queue state, see the next commit immediately, and verify read-only,
bounded reader lifetime. Error-body tests cover bounded draining of HTTP 5xx;
the local deadline test also rejects the retry-progress suffix in its final cause.

Native queue semantics: priority inputs and Oberth run ceilings are refused.
Submission follows durable admission/queue sequence without class weights,
backfill or per-group grants. `TestNativeArgoCapDoesNotHoldBackAnyOfTenSubmissions`
keeps native Argo execution parallelism separate from submission.

Upstream publication retry uses conservative server/transport classification,
five durable attempts and 2/4/8/16-minute backoff. Tests cover permanent hook,
policy and identity refusals; lost push and finalization replies; ordinary
non-forced default/promotion pushes; exact tag identity; fresh finalization
contexts after a push deadline; progress/audit persistence across restart;
budget reset only by explicit retry; and promote_retry reuse of an unchanged
tested merge versus re-testing after target movement. Historical migration
fixtures remove v38 before reconstructing older schemas.

Status-consistency regressions hold completion at list rendering and long-poll
timeout boundaries, enforce status filters after refresh, and prove reads do
not infer completion from remote ref equality or mutate the ledger. Publication
status and its owners are read in one committed snapshot, including behind an
uncommitted writer. Retry tests cover queued, delivered, failed, stale/lost
replies, unreadable status, admitted dispatch failures and no-work refusals.
Run-list/detail/CLI tests keep failure phase and publication backoff visible.

Branch deletion (#1490): `TestReceiveDeletionCallbackNeverReentersReceiveLock`
delivers a deletion callback under the held receive lock (the v0.17.84-v0.17.95
self-deadlock). `TestReplayPendingDeletionCompletesWithinBudget` replays a
stranded deletion within a deadline and proves a refresh does not re-create the
branch before propagation; `TestReplayPendingKeepsSkippingQuarantinedRecord`
keeps operator-quarantined records untouched. `TestBlockingUpstreamNeverBlocksReceiveAndRetriesBounded`
runs receives while the propagator is stuck on a forge that never answers, then
exhausts the bounded attempts. Prune plans, stale confirmation, merged-only,
reserved-branch and typed-confirmation refusals, per-batch audit and outbox repair
have their own tests.

Native deadlines and synchronization:
`TestPendingWorkloadHasNoServerStartupDeadline` and
`TestNativeWaitingWorkflowHasNoServerWatchdog` leave pending work to Kubernetes
and Argo. `TestBuildPreservesTheDocumentDeadline` keeps authored deadlines;
`TestNativeMissingDeadlineRemainsMissing` leaves them optional.
`TestBuildSubmitsSynchronizationUntouched` and
`TestExecutionDisplayNamesTheSynchronizationLock` cover authored locks and
curated native reason lines. Server startup allowances, automatic infrastructure
retries and the pipeline-wide release mutex are absent.

Background status probes: `TestUpstreamFailureThresholdAndBackoff` checks exact
retry boundaries, retained check times, three consecutive failures and recovery;
`TestUpstreamSharingKeepsCredentialAndEndpointBoundaries` covers organization
sharing and distinct host/port spellings, usernames, credentials, local paths and
IPv6 zones. `TestUpstreamSharingPreservesHostPinDecisions` performs real SSH
handshakes with different pins for host case and zero-prefixed ports in the same
file, preserves acceptance/rejection in both orders, and shares identical
spellings across organizations.
Concurrent/cancellation tests keep one in-flight sample and retain evidence on
shutdown. `TestStatusProbeFlagsAndChart` checks binary defaults, chart wiring and
schema/flag validation. `TestStatusProbeIdentityUsesResolvedCredentialsAndPins`
checks projected-key fallback and rotation. The receive-adapter integration test
runs readiness, mirror refresh, promotion delivery and real tag pushes with an
unavailable diagnostic; dashboard behavior preserves escaped errors/check times.

Startup promotion recovery: `TestStartupSkipsPromotionAlreadyDeliveredByAnotherPromotion`
creates a real interrupted promotion and a later delivered promotion, repeats
startup recovery without new retries or failure logs, and checks the typed
admission refusal for a delivery racing recovery. Existing grant, failed-step,
manual-cancel and removed-uplink recovery cases remain enforced.

# Bootstrap cell review follow-ups (#1533)

The mandatory Python suite verifies release environment preservation, own wall
budget enforcement, exact required Go pass events and safe event test identities,
decoded/split Go output scanning, attempted mutation rejection for POST/PUT/PATCH/
DELETE, stalled TLS teardown, refusal of every owned fresh-host path and exclusive
unit creation against files and symlinks. CI additionally reruns the real CSR
fixture with its test-only generated-key leak control and requires the parent
scanner to reject that otherwise-passing Go fixture. The 18 named fixture pass
events come from the normal rest race run rather than a separate non-race run.
`test_failures.py` exercises advisory-plan success versus hard-check failure,
package-bound terminal fixture passes, private failure metadata and preservation
of the originating stage/check. A real one-shot cell alarm followed by a stalled
final scan must still terminate under its independent failure-scan alarm.
The real CLI plan must refuse zero available disk with exit 1 and the named hard
check; a synthetic green JSON result with a nonzero exit must still fail.

Plugin macOS onboarding: `TestPluginDNSRemedyWaitsForNameAndReprobes` covers
IPv4, IPv6, hostname targets and the before/after resolution boundary.
`TestPluginDNSRejectsStaleOrInvalidAddresses` rejects invalid endpoints and
stale extra DNS addresses.

`TestPluginOffersLoadBalancerFromGenericReachability` checks proposals without
provider/distribution matching. `TestPluginWaitLoadBalancerIsBoundedAndSelected`
checks selected namespace/Service, delayed ingress and bounded missing-Service
waits. The address wait is read-only and follows the approved install start.

`TestPluginKindPrintsCompleteRemedyBeforeInstall` parses the printed kind YAML
and verifies both Oberth NodePort mappings and explicit host ports.
`TestPluginKindRecognizesSelectedClusterMappings` pins Docker inspection to
the selected kind cluster and avoids printing a replacement remedy when its
mappings are already present.

`TestPluginPrerequisitesOnlyPrintMissingToolCommands` runs the wrapper under
sh, bash and zsh when available: missing tools only print exact commands/links,
including with affirmative stdin. Brew is never invoked. The installation
option is rejected; existing prerequisites let validation succeed.

Mac onboarding #1548: `TestPluginNetworkPolicyLiveProbeAndCleanup` drives the
real probe through fake Kubernetes resources and exec outcomes: enforced,
absent, namespace/exec denial, unavailable image, cancellation, lost create
response and cleanup refusal. It verifies the shared image pin, restricted
tokenless Pods, allowed/blocked/recovered sequence and UID-bound cleanup.
`TestPolicyPreflightUsesLiveEvidenceForEveryDistribution` keeps enforcement
independent of kind/managed/unknown controller and distribution names.
`TestProbeEvidenceRejectsAndRecovers` covers drop/reject/reset enforcement,
refused baseline, missing recovery, both selections and verified HTTP overlap.
`TestReview1548RejectingSourceWithoutEnforcement` injects independent source-only
bursts without a dataplane or policy-dependent failures and requires unverified.
`TestReview1548DuringSampleCanHappenOnlyAfter` rejects a delayed handshake before
any selected attempt; early/failed completion tests reject missing overlap.
The positive model evaluates actual Pod labels against egress selectors; it does
not treat policy existence as enforcement.
`TestPolicyTrafficScriptDistinguishesTransportFailures` executes the shell
classifier against timeout/refusal/reset, HTTP-error and unexpected-body outputs.
`TestPolicyRealHTTPConnectionPressureAndStall` bridges actual localhost HTTP
bytes/reset connections without any enforcing dataplane: a fresh-connection
failure while a long response occupies the server remains unverified.
`TestPolicyIndependentAlternatingSourcesSeeded` measures original and periodic
source-counter fault windows over seeds 1..1024 for refusal/reset, using virtual
time with nonzero modeled I/O. It logs false-pass counts, including a nonzero
residual in the periodic model; it makes no universal error-rate claim. Positive
seeded controls can complete all rounds, and selection tests require both sources.
The human plan test retains measured scope and the lack of causal proof.

Mac onboarding #1549: `TestPluginPlanAndInstallKeepKubeconfigBytes` retains two
operator files, comments, current-context fields and both contexts without changing bytes
after a successful plan and a non-dry-run install that reaches the Helm boundary.
Operator-created kind clusters use the same selected-context read-only loading
and private child-copy boundary as every other existing cluster.

Mac onboarding review regressions cover ingress-only/egress-only data planes,
unrelated HTTP failure, delayed programming, namespace-label and partial-node
exclusions, exact evidence scope, and stale/active/foreign probe cleanup.
The original no-enforcement fixture now correctly expects unverified rather
than treating a timeout as proof of absence. Every child runner executes real
pinned client-go credential persistence into its private copy and checks source
bytes/modes/sibling sets and an absent migration destination. Merge tests retain
precedence, context, relative assets and -- separation. An actual add-repo -> CLI
-> child runner fixture proves the direct script path. Stored disabled policy
survives preflight and upgrade arguments, preserves its warning, and verifies
as disabled from installed values rather than the flag default.
Setup verification and saved-release probes also execute real client-go refresh
through their production wrappers; source bytes/modes/sibling sets and the absent
default migration destination remain unchanged, and private copies are removed.

Mac onboarding #1553: `TestInstallRejectsClusterCreationOption`,
`TestWizardOnlyOffersExistingClusters` and
`TestSavedAnswersRejectRemovedClusterCreationState` keep the CLI, setup choices
and resumed answers on existing operator-provided clusters. Printed kind
remedies remain guarded instructions with a separate kubeconfig destination.

Mac onboarding R3 #1548: `TestExistingUpgradeDoesNotRequireFreshProbe` reaches
the real Execute Review boundary with unverified capability for stored true,
false and missing values; `TestRunUpgradePreservesPolicyWithoutProbePrivileges`
exercises real upgrade application with no fresh-probe requirement. Replacement,
renewed and unknown-lifetime namespaces never acquire stale deletion authority.

Mac onboarding R3 #1549: `TestSweepReclaimsOnlyUnlockedOwnedCopies`,
`TestSweepPreservesUnverifiedAgeAndReportsCleanupFailure`,
`TestSweepPreservesMalformedOwnershipMarkers` and
`TestCLIPrivateKubeconfigSignalsAndCrashRecovery` cover active leases, stale
marked copies, unknown/future/trailing metadata, directory replacement, all
handled signals and SIGKILL followed by startup reclamation after child exit.

Integration #1549: `TestConcurrentPrivateCopyLifetimes` runs four concurrent
owners through 100 create/cleanup cycles each, then requires an empty copy root.
A swept entry that disappeared is already cleaned; existing malformed, replaced,
foreign, future or leased objects retain their strict preservation rules.

The combined bootstrap cell uses actual Helm release discovery and the live
probe against a restricted loopback API. Existing-release plans preserve true
and report unverified as a warning; a fresh plan must exit 1 on the hard
network_policy check. Exactly the denied scratch-namespace POST is expected;
all other mutation attempts refuse. No CNI enforcement is claimed by this fixture.
The normal rest suite still covers enforced probe traffic and UID-bound cleanup.

Native cancellation isolation: the running-run cancellation test waits for both
independent Workflows to be submitted, without assuming creation order or an
Oberth workflow ceiling. It requires deletion of only the selected Workflow,
a live peer and preservation of cancellation against the owner's late result.

Private-copy refresh stress repeats the concurrent lifecycle/sweep cases 50
times. After lease acquisition, disappearance is checked before marker age is
judged; existing future, malformed and replaced objects remain preserved, and
the separate pre-delete identity check remains required.

Semaphore retirement checks the live retention marker before Helm removes a
previously managed object. An operator replacement is left untouched and upgrade
refuses unless it was explicitly marked keep; missing observation clients refuse
too. The regression models the pinned Helm deletion path.
