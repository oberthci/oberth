
Promotion recovery #752: `TestListPromotionsFindsPendingAdmissionBeyondRecentGlobalPage`
proves exact filters precede pagination, cursor pages preserve oldest pending
admissions without runs, defaults/max bounds hold, and malformed filters fail.
`TestPromotionListRecoversLostIDWithoutMutation` invokes the actual tool/service
against SQLite with the mutation gate blocked, recovers the admission ID and
proves audit/Git/gate remain unchanged; unknown repositories, invalid selectors
and extra input fields fail. Tool-surface/count expectations add the documented
read-only tool while preserving every previously registered tool.

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
