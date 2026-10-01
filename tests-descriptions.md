
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
owed, as detailed in [the catalog](docs/watch-csa-test-catalog.md).
V5 source regressions use predictable public Go TLS test data to check that
both clients retain one literal credential despite file replacement, without
key generation or network requests. They reject impersonation and caller
authority hooks, added non-label metadata and a wrong owner index after plan
entry reordering. A public open-descriptor replacement control proves running
inode byte binding. All new checks are UNRUN.
