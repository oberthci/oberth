
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
