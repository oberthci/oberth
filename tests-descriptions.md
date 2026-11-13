Assisted continuation (retry hold): `TestBuildAssistMarksOnlyCredentialFreeLeavesWithoutOwnRetries`,
`TestBuildWithoutAssistInjectsNoHolds`, `TestBuildAssistSkipsCredentialedWorkflows`,
`TestAssistHoldEligibilityRefusesIdentityAndLaunchers` and
`TestBuildRefusesRepositoryAssistAnnotations` pin which leaves receive the
server-owned one-attempt `retryStrategy` and conditional `podSpecPatch`, the
workflow deadline extension and the refusal of repository copies of the hold
annotations. `TestAssistPatchResolvesWithArgoTemplateEngine` substitutes the
conditional patch with Argo v4.0.8's own template engine for attempt 1, a
failed-command retry and the re-substitution of a retry whose Pod creation was
deferred (`lastRetry` zeroed), and applies it with the same strategic merge,
proving pause, deadline, writable source, read-only cache and scratch cache
variables land on every retry attempt and never on attempt 1.
`TestBuildAssistBoundsInheritedTemplateDefaultsDeadline` proves a leaf without
its own deadline takes the authored `templateDefaults.activeDeadlineSeconds` as
hold deadline and retry bound (a leaf's own still wins; a non-literal default
means no hold). `TestAssistRetryRefusesDeadlineKills` evaluates the injected
`retryStrategy.expression` with Argo's own evaluator in the
`processNodeRetries` scope: the live-spike kubelet kill (exit 143, duration at
the budget) is not retried, nor are SIGKILL, trapped-signal, evicted or
self-exited 137/143 attempts; a command that failed under its budget is. Controller tests with a
fake clientset cover hold detection, bounded exec and exit codes, release argv,
abandon and the concurrent-hold cap; `internal/holdfs` tests cover unified-diff
parsing refusals (traversal, `.git`, renames, binaries, counts, limits), exact
context application and the Pod helper's symlink, precondition, truncation and
release behaviour. Service tests cover owner-uplink/admin authorization, argv
and stdin bounds, patch application through the real helper, the record-before-
release rule, abandon on failed release, step disambiguation and the gate. Store
tests prove the opt-in is immutable, a continue marks the run assisted
atomically with its audit row, the verdict is monotonic, and assisted passed
runs are excluded from `HasPassedRunForSHA`; service tests exclude them from
`greenBranchCandidate` and give them the `assisted: ... not evidence` reason
line without changing status. Chart tests render every `assist.*` value as its
serve flag with the binary default and reject out-of-range values.

Release chart digest #1181 tests execute the release script with real Helm
(skipped when Helm is unavailable). They verify the published digest reaches
the packaged defaults and render without editing source, and reject a stale
archive even when the stamped source chart renders the new digest.

Promotion recovery #752: `TestListPromotionsFindsPendingAdmissionBeyondRecentGlobalPage`
proves exact filters precede pagination, cursor pages preserve oldest pending
admissions without runs, defaults/max bounds hold, and malformed filters fail.
`TestPromotionListRecoversLostIDWithoutMutation` invokes the actual tool/service
against SQLite with the mutation gate blocked, recovers the admission ID and
proves audit/Git/gate remain unchanged; unknown repositories, invalid selectors
and extra input fields fail. Tool-surface/count expectations add the documented
read-only tool while preserving every previously registered tool.

Watch connector #671 tests preserve exact source state during ownership transfer:
all four old identities and intended public target specs must match before CAS,
then only Helm metadata changes. Unknown reply/changed UID retain completed work
and stop. Chart tests retain all previous checks and add source enablement
preservation and the real HTTPS origin trust mount; malformed inputs still fail.

The adoption boundary tests cover public-values immutability, immutable image
pins, preserved network policy, platform/bootstrap denial, strict certificate
bundles, post-CAS state checks and actual subprocess output limits. No mixed
Helm output or token-derived digest is required as test evidence.

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

Watch CSA recovery #671 (prepared, all UNRUN): the chart regression strict-decodes
four rendered YAML resources under two release names and checks metadata,
selectors and Pod labels. Unit tests reject unauthorized field tries, owner
identity/cause ambiguity, public-plan aliases and drift; model guarded SSA/CAS
ordering and retained public receipts; and bind the fixed release signing key.
The separately admitted `watch_real_api` test starts from real kubectl CSA,
reproduces original six-conflict/FAILED72 behavior, then calls the same unexported
effect engine for the corrected one-conflict recovery. Real readback must show
unchanged spec/generation/other ownership, normal Helm73 same-UID convergence
and the intended final field owner. Shared ownership, extra SSA conflict, RV,
expiry, failed-record drift and postcommit stop are negative controls.
Source-bound offline CPU2 build and reserved tmpfs/TLS process custody helpers
are prepared; none has run. Signed released-installer/live qualification remains
owed, as detailed in [the catalog](docs/backup-restore.md).
V5 source regressions use predictable public Go TLS test data to check that
both clients retain one literal credential despite file replacement, without
key generation or network requests. They reject impersonation and caller
authority hooks, added non-label metadata and a wrong owner index after plan
entry reordering. A public open-descriptor replacement control proves running
inode byte binding. All new checks are UNRUN.

Native submission tests use real SQLite and a fake engine to submit all
accepted runs under durable sequence ordering, independently of Argo's
parallelism limit. Priority classes, weights and admission groups no longer
describe current coverage. Adoption tests retain exact source/UID/identity
bindings and isolate one refused Workflow from scheduler startup.

Load-tolerant regressions use virtual time for MCP proxy idle deadlines,
immutable module retry budgets and audit mutation gate expiry/cancellation.
The proxy still parses real HTTP over in-memory connections; separate TLS
and HTTP/2 tests retain transport coverage. Held-writer reads and per-commit
handshakes cover all 16 × 32 status and step writes without a continuous reader
loop. Measurement-history removal tests preserve outcomes, logs, timestamps,
termination diagnostics and audit while dropping usage tables and counters.
The historical-schema fixture reconstructs the old retention index and retains
its migration receipt before replaying measurement removal.

MCP agent loop (#1514): store tests cover the migration, the fresh-schema
object list and the downgrade fixture. `TestAgentEventRunFinishedOnceWithReasonAndHint`
records one `run.finished` with failed step, reason and retry hint however
often the run is observed, including after a simulated restart.
`TestAgentEventRunHeldPayload` pins the `run.held` payload (step, pod,
`held_since`, `abandon_at`, `holds_in_run`, `multiple_holds`) and that a
continued hold is not reported. `TestAgentEventLockWaitAndSyncStatus` emits
`run.lock_wait` only past the threshold with the holders, and proves
`sync_status` current holders, wait durations and `max_concurrent_holders`
(1 for a handover, 2 for an overlap). `TestAgentEventsScopedToUplink` proves a
scoped uplink reads no other upstream's events or lock members, that a hidden
repository filter answers like a missing one, and the `own` and `refs_glob`
filters. `TestAgentEventReleaseAndPublication`, `TestRunWaitUntilFinishedHeldAndTimeout`,
`TestRunWaitReturnsOnHold`, `TestRetryHintClassification` and
`TestAgentEventPolicyBounds` and `TestAgentEventRunHoldAbandoned` cover the remaining kinds, `run_wait` states,
every retry hint class and the policy clamps. `TestPromotionPruneSourceOnlyAfterVerifiedPromotion`
keeps the source for a failed promotion, a differing upstream target, an
unmerged source and (until the bound) an unreachable upstream, prunes only a
verified promotion, never changes a settled request, audits every decision and
carries the outcome in `promotion.finished`; `TestBranchDeleteManyUnmergedNeedsReason`
requires and records a reason for unmerged deletions. API tests drive the real
HTTP handler: `TestMCPEventStreamPushesChannelNotifications` (capabilities,
session ID, channel notification shape and identifier meta keys, filter by
`events_subscribe`), `TestMCPEventStreamResourcesAndScope` (`run://`
resources, `resources/updated`, a foreign credential's 404, per-uplink scope),
`TestMCPEventStreamBounds` (bounded backlog with truncation notice, one stream
per credential returning 429, heartbeat, 406 without SSE) and
`TestMCPWithoutEventFeedHasNoPush`. Flag and chart tests render every
`agentEvents.*` value and refuse out-of-range values.
