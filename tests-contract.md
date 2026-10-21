
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
runs. `TestActiveRunsOrderAndWaitReasons` drives the real admission gate with
a VM-parked run, a group waiter and a head that does not fit, and checks each
reason, weight and position; `TestScopedActiveRunsStayInScope` withholds
sequences and positions. `TestRunListMCPMatchesHTTPOrderAndPassedAlias` compares
the default MCP/HTTP ordering and preserves the passed-status wire alias.
`TestRunsPageIsOneFlatFilteredList` pins the flat dashboard and head marker.
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

Run cancel and supersede admission release:
`TestSupersedingAWaitingXLHeadReleasesItsReservationOnTheNextGrantPass` (real
SQLite, fake engine) supersedes an XL group member waiting at the FIFO head
behind an L holder and proves the group member and another repository's run
are admitted on the next grant pass, with no restart; it failed before the fix.
`TestWeightedAdmissionAbandonWithdrawsOnlyUngrantedReservations` pins the gate
primitive. Store tests (`run_cancel_test.go`) cover queued/waiting/running/
terminal cancels, the no-successor obligation and its Job name, no re-fire by
owner restart recovery, no CI issue, promotion failure blocking publication,
pending-publication refusal and reason bounds. Service tests
(`run_cancel_test.go`) drive `run_cancel` end to end: queued runs are never
claimed, a cancelled waiting XL head releases admission at once, a running
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
live rollout. [The catalog](docs/watch-csa-test-catalog.md) records that boundary,
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

Protected harness QMP readiness #1143: `TestHarnessProtectedTransportIntegration`
keeps the pass inventory, forged guest output, failure, timeout, changed-guest,
process-join and sandbox assertions. Its native host-driver fixture waits for
the QMP socket within the existing two-second readiness budget before exiting
a successful case. The missing-QMP regression supplies a successful guest
response but requires readiness failure, proving guest readiness alone cannot
complete a case.

Workspace cache mode #1093: `TestWorkspaceCacheMountsOnlyTheCache` builds a
`work`-VCT pipeline with delivered artifacts and proves a
`workspace-mounts: cache` leaf (with and without `workspace-env: none`) gets
only the read-only source, per-Pod `/tmp` and the writable `/work/cache`, the
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
`TestAdmitWorkspaceMountsValues` refuses unknown values; `TestAdmitKVMRefusesWorkspaceCache`
and `TestAdmitNonrootRefusesWorkspaceCache` refuse `cache` on leaves that take no
host cache; `TestTrivyGuardWorkspaceCache` treats `/tmp` as private and
`/work/cache` as shared for cache leaves.

Parallel CI race groups: `TestOberthRaceGroupsCoverEveryPackageExactlyOnce`
compares the store/service/remainder partition with the actual `go list ./...`
inventory and refuses missing or duplicate packages.
`TestOberthRaceGroupLauncherPreservesChecks` records actual Go invocations,
retains race/verbose/uncached execution and the explicit timeout, and proves a
newly listed package joins the remainder.
`TestOberthSplitBurnsUseTheRaceGroupLauncher` checks the admitted Pod commands,
CGO and processor limits; the existing graph and isolation contracts cover
all three race burns and require each one before every cross-build.

Audit read surfaces (#1104): HTTP and MCP must agree on filtered entries and
paging, and refuse scoped and non-admin callers. Status reads the latest backup
completion record's archive hash, timestamp and snapshot head, omitting it for
scoped callers. Upgrade completion is recorded only after verification; audit
record failures warn without failing a completed upgrade. CLI tail tests cover
appends between head and page reads, bursts exceeding 200 matches, and appends
during pagination that must remain eligible for the next poll.

Branch Workflow adoption #1172: startup restores each existing branch Workflow
under its durable run ID, queue/admission sequences and admitted size/group,
including Workflows completed
while the owner was down. Missing Workflows still requeue; API and binding
failures preserve ownership. Shutdown preserves new and adopted non-terminal
branch Workflows and their workspaces. Explicit cancel and supersede still
delete adopted Workflows. Resumed observation retains progress and log bytes,
replays only unrecorded steps, enforces the remaining aggregate budget, and
refuses Workflow UID/spec replacement. Executor identity delivery resumes for
Pods in the token waiter and skips Pods past it. Idempotent adoption cannot
double-count admission, and restored groups block new grants until release.
Transient Workflow read failures are retried within a bounded grace; a
persistent observation failure deletes the Workflow and fails only that
adopted run while the scheduler keeps dispatching. A terminal Workflow whose
observation errored keeps its real result and collects artifacts. Refused
adoptions log and return the run and `<namespace>/<name>` to delete; read
failures name it without a deletion instruction.

KubeVirt admission #1173: `TestKubeVirtAdmissionAccountsEveryDimension` admits
concurrent runs up to each VM/core/memory boundary, parks excess demand and
releases it without blocking another repository.
`TestKubeVirtCapacityKeepsFIFOAndDeclaredGroup` prevents smaller demands or
ordinary members of a declared group from overtaking the capacity waiter.
`TestKubeVirtSchedulerConcurrentRunsAndQueueReason` checks the actual scheduler
and flat run list, including the exact queued reason and absence of an implicit
group. `TestKubeVirtCancelAndSupersedeReleaseBeforeObserverReturns` holds the
observer tail while accepted deletion frees admission.
`TestKubeVirtDeletionDuringCreationKeepsCapacity` keeps capacity until in-flight
creation settles and verifies that a late observer cannot release its successor.
The oversized-run test
proves refusal before submission. Static demand tests cover trigger-specific
documents, live grants, server ceilings and invalid quantities.

`TestKubeVirtRestartRestoresTwoRunsBeforeAdmittingThird` reopens durable state,
adopts two live Workflows under their original run IDs, and parks a third until
one completes. `TestKubeVirtAdoptionCountsOnceAndDrainsReducedCapacity` checks
idempotence, conflicting-demand refusal and release under a reduced grant.
Controller and adapter tests bind restored demand to the verified Workflow
spec without requiring a current grant or repository checkout.

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
immutable promotion ownership. App tests reconstruct exact trigger/size/group/
tested-SHA bindings; actual server-built credentialed promotion and WIF release
Workflows survive changed current identity/policy configuration unchanged, and
resumed Wait refuses changed service accounts, WIF projections/environment,
Workflow UIDs or spec identities. Existing resume/log-budget, executor-token,
publication serialization, KVM recovery and migration assertions remain.

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
VMI failure tests check run/name/UID scoping, typed conditions and recent Events,
unknown/API failure handling and exclusion of free-form messages. Artifact tests
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

# Trusted preflight owner restart (#1172 slice 3)

Trusted conductor attachment remains one-use; restart recovery cannot infer a
positive receipt or reattach its stream. `internal/app/trusted_restart_test.go`
checks exact bound cleanup, negative evidence before cleanup, retention after
cleanup failure and ambiguous-create refusal. Store restart tests cover owner
recovery ordering, immutable receipts, durable one-retry limits across reopen
and across a replacement's pre-reservation restart, cancellation/newer-work and
publication guards, active promotion authority/grants, and rollback when audit
persistence fails. `TestTrustedRestartInterruptsThenRunsFreshPreflightBeforeDelivery`
loses an in-flight observer and verifies fresh evidence before exactly one main
Workflow and delivery. `TestTrustedRestartPromotionStillChecksMainBeforePush`
keeps the immutable promotion plan and proves a moved target prevents its push.
