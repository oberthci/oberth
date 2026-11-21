# Requirements

Generated from `requirements.json`; SHA256 54358e38309454124cc7a05d13b93ea72ed5419a7d6f0b7f2363d44420c7a28a.

These are normative requirements. Inspections are planned; this register does not claim they have passed.

## C1

### REQ-C1-002

**Security.** Oberth never writes a secret, token or private key to logs, events, diagnostics or command output; server diagnostics use a fixed list of fields. Without this, a normal diagnostic can disclose private work or credentials. Inspection to write: `TestPrivateBoundedDiagnosticsContract` (contract_test).

### REQ-C1-003

**Ux.** Status reasons fit 40 characters, refusal reasons 80 and remedies 120; native detail stays separate, and views show each message whole. Without this, users cannot read what happened or the next action. Inspection to write: `ux-gate` (walk_step).

### REQ-C1-004

**Ux.** The web UI shows a pushed run within seconds, with the same run ID, step names, statuses and logs the CLI shows. Without this, the web view disagrees with the run users just pushed. Inspection to write: `ui` (walk_step).

### REQ-C1-005

**Ux.** Imported issue views show the original author, closer, ordered history and an imported mark through the ordinary issue page. Without this, preserved issue history is invisible where a person reads the issue. Inspection to write: `ui` (walk_step).

## C2

### REQ-C2-001

**Security.** Enrollment and renewal issue a credential only for the identity and repository scope the operator approved; a caller can never choose another identity or a wider scope. Without this, a caller can acquire another identity or more authority than approved. Inspection to write: `TestValidatedEnrollmentAndIdentityContract` (contract_test).

### REQ-C2-002

**Correctness.** Installation-wide actions, including manual-issue import, require an unscoped admin and report success only with their audit record saved; only one database backup runs at a time, or returns "backup already running". Without this, a repository-limited key can change installation authority, effects can lose their audit, or backups can exhaust resources. Inspection to write: `TestAuditedAdministrativeEffectsContract` (contract_test).

### REQ-C2-004

**Ux.** Run views show each repository's latest runs, hide Argo helper containers unless asked, and show "unknown" rather than a guessed result when evidence is missing. Without this, users miss what ran or what to do next. Inspection to write: `ui` (walk_step).

### REQ-C2-005

**Correctness.** A client waiting on a run always gets its final state, even across a server restart, and a restart never runs accepted work twice or drops it. Without this, users miss completion or see a newer failure silently closed. Inspection to write: `TestTruthfulConcurrentHistoryContract` (contract_test).

### REQ-C2-006

**Security.** Promote moves main only to a commit whose latest branch run passed, or that main's current policy lets skip branch CI, and never deletes main or a reserved branch. Without this, promotion can move main from untested code or delete a protected branch. Inspection to write: `TestExactTestedPromotionContract` (contract_test).

### REQ-C2-008

**Security.** Tools validate arguments and name bad values, including unregistered issue upstreams and viewer scopes; import maps a source upstream only through an explicit --upstream <local>, creation without an upstream uses the repository's single upstream or refuses naming the choices, and one credential's slow log searches leave capacity for other callers. Without this, malformed inputs can hide issues or one caller can stall others. Inspection to write: `TestBoundedScopedReadsContract` (contract_test).

### REQ-C2-009

**Security.** A grant or revoke is saved before publication; revocation survives invalid unrelated grants, reports incomplete application, and a stored-secret revoke deletes its exact path under both tier accounts or reports that it remains stored with a repeat remedy. Without this, uncommitted authority can authorize work or a revoked secret can remain readable. Inspection to write: `TestAtomicGrantPublicationContract` (contract_test).

### REQ-C2-015

**Security.** Sync refuses the default branch and internal promotion or release runs, publishing only the exact current head of one named non-default branch. Without this, sync can move main without promotion checks. Inspection to write: `TestNoPromotionBypassThroughSyncContract` (contract_test).

### REQ-C2-018

**Correctness.** CI issue projection preserves C7's person-owned fields and closes an issue only after a later passing run of the same branch; a newer failure keeps it open. Without this, a newer failure can disappear from the issue list. Inspection to write: `TestCiIssueClosesAfterLaterPassContract` (contract_test).

### REQ-C2-019

**Ux.** A release run reports the tag, commit and run it published and emits `release.published`; a passed run without publication is never shown as released. Without this, users cannot tell a passed run from a published release. Inspection to write: `release` (walk_step).

## C3

### REQ-C3-001

**Correctness.** The secrets a repository asks for are computed on demand from its YAML at one default-head commit; unreadable or invalid files return an error, never a partial list. Without this, a pending-grant view can silently omit a required secret. Inspection to write: `TestAtomicDeclarationRefreshContract` (contract_test).

### REQ-C3-002

**Security.** Adding a Git host trusts its SSH key only if it matches a fingerprint the operator gave or confirmed; `--yes` never trusts an unknown key. Without this, connecting a repository can trust an unintended endpoint. Inspection to write: `TestReviewedEndpointTrustContract` (contract_test).

### REQ-C3-003

**Correctness.** Connection checks report the truth: a Git server error is shown as a failure, and a later success for the same host and key clears the old failure. Without this, setup reports success for a repository the user cannot access. Inspection to write: `TestHonestRepositoryProbesContract` (contract_test).

## C4

### REQ-C4-001

**Security.** A step receives a secret or release credential only if an operator granted it for this repository, workflow file and step, and the step reads only the admitted source. Without this, a different step or substituted source can receive approved authority. Inspection to write: `TestExactGrantedSourceAndSecretPathsContract` (contract_test).

### REQ-C4-003

**Security.** A step may create, read or delete Kubernetes objects only of the kind, namespace and action an operator granted; field names must match exactly, so another spelling cannot widen it. Without this, alternate spellings or substitutions can widen a resource grant. Inspection to write: `TestExactNativeCapabilitiesContract` (contract_test).

### REQ-C4-004

**Correctness.** Admission reads each referenced shared template once, refuses reference loops between templates and stops at a fixed nesting depth, so a push never hangs or runs an unadmitted plan. Without this, a normal push can hang, miscount its steps or execute a different plan. Inspection to write: `TestFiniteCanonicalPreparationContract` (contract_test).

### REQ-C4-007

**Correctness.** If admission cannot read a template because of a passing network error, the push says to retry; a certificate or YAML error says what to fix; failed reads are never cached. Without this, a repaired push can remain stuck or repeat an error the user must fix. Inspection to write: `TestHelpfulPreparationFailuresContract` (contract_test).

### REQ-C4-011

**Ux.** A push with invalid YAML or a missing grant is refused before any run starts, listing every problem with its file and fix, and the exact grant command for the operator. Without this, a successful-looking push queues work that cannot run. Inspection to write: `push` (walk_step).

### REQ-C4-012

**Security.** Admission refuses YAML that mounts a Kubernetes Secret, ConfigMap or volume Oberth did not grant, or writes over the paths where credentials are delivered. Without this, a pushed step can read ungranted data or replace credentials. Inspection to write: `TestGrantedMountPathsContract` (contract_test).

### REQ-C4-013

**Security.** Admission refuses host paths, host namespaces, privileged containers and the server's accounts or volumes. Without this, a run can read or change Oberth's state. Inspection to write: `TestServerStateBoundaryContract` (contract_test).

### REQ-C4-014

**Ux.** The grant command printed in an ungranted-secret refusal, run as printed by the operator, makes the same push admissible with no other step. Without this, the printed fix does not let the user continue. Inspection to write: `grant` (walk_step).

## C5

### REQ-C5-002

**Security.** Ordinary step code may read its executor token; its product-granted authority reaches only source for its own run and task-result writes bound to its Pod, never another run or server state. Without this, a readable executor token could change another run's evidence or expose server state. Inspection to write: `TestPrivateExecutorAuthorityContract` (contract_test).

### REQ-C5-006

**Correctness.** A step shows as passed only when Argo reported it passed for this run and attempt; a failure, once seen, stays a failure through cancellation and server restarts. Without this, users can see a failed or unexecuted step reported as successful. Inspection to write: `TestTruthfulNativeObservationsContract` (contract_test).

### REQ-C5-007

**Security.** A release step uses the deterministic account chosen once in admission from verified release-kind evidence and exact repository, workflow-file and step grants, without a configured release-role mapping; branch CI receives only CI secret authority and refuses missing CI identity without fallback. Without this, branch code can use release credentials or a second account choice can apply different authority. Inspection to write: `TestSeparateCiAndReleaseIdentityContract` (contract_test).

### REQ-C5-008

**Ux.** Run views list steps in plan order, fold retries into one step, and give a short waiting or failure reason based on Argo's status, never a guess. Without this, users cannot tell what ran or what to fix next. Inspection to write: `ui` (walk_step).

### REQ-C5-009

**Ux.** A failed run's view and `run wait` name the failed step, its short reason and the exact command to read its log. Without this, a failed build leaves users searching for its cause. Inspection to write: `failing-build` (walk_step).

### REQ-C5-010

**Security.** After cutover, a release role authorizes only its exact repository, selected file and template under verified release-kind evidence, with an independent exact-audience grant and deterministic release-step account. Without this, an ungranted template or branch run can acquire publication authority. Inspection to write: `release` (walk_step).

### REQ-C5-011

**Correctness.** Every Workflow the product submits carries the native controller-instance label with C9's installation instance ID, so another controller never acts on it. Without this, a second controller can execute the same run and create duplicate steps. Inspection to write: `push` (walk_step).

## C6

### REQ-C6-001

**Security.** Oberth uses only the key configured for each Git host, never a shared or ambient key, and rechecks an incoming SSH key's repository scope after it logs in. Without this, Git operations can reach another repository or endpoint with unintended credentials. Inspection to write: `TestExplicitGitIdentitiesContract` (contract_test).

### REQ-C6-002

**Security.** Oberth treats main's promotion policy as absent only after a successful fresh upstream read shows no policy file; a failed read or a deleted main never counts as "no policy". Without this, an upstream read failure can bypass the policy protecting main. Inspection to write: `TestFreshUpstreamPolicyContract` (contract_test).

### REQ-C6-006

**Correctness.** When connecting to a pinned Git host, offer only host-key types that have a pin, so hosts with several keys connect, and say so when no pin fits. Without this, a valid pinned repository can fail to connect. Inspection to write: `TestPinnedGitInteroperabilityContract` (contract_test).

### REQ-C6-008

**Security.** Close a Git connection that sends no data for the idle timeout, and end every connection at a fixed total time limit, so a stuck client cannot hold the server. Without this, a stuck client can retain server capacity indefinitely. Inspection to write: `TestBoundedGitTransportContract` (contract_test).

### REQ-C6-009

**Ux.** Deliver the exact admitted checkout as a private copy per step with writable helper scratch on the default install, without requiring a user override. Without this, the first ordinary build fails before reading its source. Inspection to write: `push` (walk_step).

### REQ-C6-010

**Security.** Each source-consuming step gets a private copy of exactly the admitted commit, with committed files delivered without write permission; local edits stay in its Pod, and only Pods of that run's recorded Workflow can load it. Without this, a run can test different source or load another run’s source. Inspection to write: `TestExactRecordedWorkflowSourceContract` (contract_test).

### REQ-C6-011

**Security.** Names, refs and paths from callers are validated once before use in Git commands, file paths or URLs. Without this, caller input can change a command, path or destination. Inspection to write: `TestValidatedGitInputsContract` (contract_test).

### REQ-C6-012

**Ux.** A push prints the started run's watch link, or says the push is parked and that `-o oberth.run=1` runs it. Without this, users cannot find the run or know how to start it. Inspection to write: `push` (walk_step).

### REQ-C6-013

**Correctness.** A step whose source delivery starts while the server restarts waits within its own deadline, then gets its source without spending native retries. Without this, a routine server restart fails otherwise valid multi-step builds. Inspection to write: `server-restart` (walk_step).

### REQ-C6-014

**Correctness.** Every push ends accepted and delivered once immediately despite a lost reply or client exit, or rejected with refs restored; recovery uses the reservation's old values and each transaction's prepared reference/value set, never the transport exit, the hook refuses repeated refs, only refs changed since reservation and equal to the requested value count as applied, and audited abandonment by reported ID completes required rollback before record removal, including for unreadable records. Without this, a client disconnect or restart can lose, replay or misattribute an accepted push. Inspection to write: `push` (walk_step).

## C7

### REQ-C7-004

**Correctness.** The audit log is a local hash chain checked when read and on backup and restore, never on the path of every mutation. Without this, tampered or missing history can authorize changes to Oberth state. Inspection to write: `TestLocalAuditChainContract` (contract_test).

### REQ-C7-009

**Security.** Re-registering a key, deleting an upstream or retrying never widens what a key may access; only an explicit scope change can, and a revoked key never becomes active again. Without this, removal or retry can accidentally grant wider access. Inspection to write: `TestNonwideningDurableIdentityContract` (contract_test).

### REQ-C7-011

**Correctness.** An edit naming the version it read fails if the record changed meanwhile, and every change is saved with its audit or not at all; a failing run preserves a person's lock and title on a native CI issue. Without this, a concurrent user change or failure can be lost. Inspection to write: `TestTransactionalEvidenceUpdatesContract` (contract_test).

### REQ-C7-019

**Correctness.** Recording a key's last-use time never delays or fails a login; under load those updates may be dropped. Without this, ordinary connections can stop behind optional usage accounting. Inspection to write: `TestNonblockingAuthenticationTelemetryContract` (contract_test).

### REQ-C7-020

**Correctness.** Manual-issue import saves each file atomically with its audit and preserved numbers, refuses overwrite and non-manual kinds, confirms identical record-set replay without changing later edits and allocates above the highest imported number; files and encoded requests fit 1 MiB, while lines and complete issues with provenance and history fit 128 KiB, checked at the save point without truncation or raised limits. Without this, an import can overwrite issues, lose half a file or change the numbers used by people and commits. Inspection to write: `ui` (walk_step).

### REQ-C7-021

**Correctness.** The grants store uses schema 5's baseline without configured release-role records, refuses mismatched layouts, and pins each supported schema's baseline so a changed layout needs a new number. Without this, a changed grants layout can be opened under the same schema number and misread authority. Inspection to write: `TestSchemaNumberPinsBaselineLayout` (contract_test).

## C8

### REQ-C8-001

**Ux.** Explain pending run or step state and follow the active run into live logs without requiring a second manual observation. Without this, users see unavailable bytes and cannot tell whether their build has started. Inspection to write: `live-logs` (walk_step).

### REQ-C8-002

**Ux.** `oberth log <run>` without a step follows the failed or running step, showing its own output under a step heading and executor output only for a failure outside that container. Without this, users must find and select the useful log themselves. Inspection to write: `live-logs` (walk_step).

## C9

### REQ-C9-001

**Correctness.** Only the profile holding an existing installation's administrator key may update it; an interrupted install reuses its saved admin key and certificate or issues a matching certificate from the installation CA, and finishes with working admin access. Without this, an interrupted install can lose its only usable administrator identity. Inspection to write: `TestResumablePrivateEnrollmentContract` (contract_test).

### REQ-C9-003

**Correctness.** Upgrade uses the real latest native revision for version checks and replacement, takes user choices only from the newest applied installer revision, asks for review after failure and treats values as data, never chart code; verified completion saves upgrade.completed. Without this, an upgrade or rollback can silently apply the wrong configuration. Inspection to write: `TestReviewedUpgradeStateContract` (contract_test).

### REQ-C9-004

**Security.** A backup produces an authenticated, complete age-encrypted database archive whose inspected snapshot and report match, while a dry run writes nothing; the bundled store is in no backup and install gives the operator no administrator token for it, so review and backup output say secrets must be put again after store loss and show how to list secret names to keep beside the backup. Without this, an operator can save an incomplete or unauthenticated database copy, or a preview can write files. Inspection to write: `TestBackupIsAgeAndDryRunCreatesNoFiles` (contract_test).

### REQ-C9-009

**Security.** Install publishes a server certificate only after checking it matches its key, covers the server's names and is valid now. Without this, install can publish unusable trust or trust another endpoint. Inspection to write: `TestVerifiedInstallationTlsContract` (contract_test).

### REQ-C9-022

**Security.** Secret-store setup writes one static rule set: the server may write or delete but not read step secrets, and a step reads only its own account's secrets, rejecting account and path names that could change a rule's meaning; admin credentials go only to the verified store Pod. Without this, setup can widen secret access or send recovery authority to an unrelated workload. Inspection to write: `TestExactSecretStoreAuthorityContract` (contract_test).

### REQ-C9-027

**Security.** The secret client talks only to the configured secret store over verified TLS, ignores proxy settings, and leaves no partial secret file when a fetch fails. Without this, normal install or credential use can leak secrets through ambient configuration, output or leftover files. Inspection to write: `TestPrivateBoundedSecretDeliveryContract` (contract_test).

### REQ-C9-038

**Security.** Install, upgrade, rollback and recorded repair use online or --release-dir download-layout inputs, including a version-named child directory, only if chart and images match the signed release record under C13's key-only trust; there is no unsigned fallback. Without this, a release install can run substituted bytes. Inspection to write: `TestVerifiedReleaseInstallationContract` (contract_test).

### REQ-C9-052

**Ux.** Complete install, upgrade and recovery with usable saved access, explicit output locations and one clear next action for any pending state. Without this, users leave setup without working access or cannot tell whether an upgrade completed. Inspection to write: `install` (walk_step).

### REQ-C9-053

**Correctness.** Every cluster admission-policy expression guards optional request fields; a plain Pod update without subResource is admitted. Without this, a missing request field can refuse every Pod update and stop native completion. Inspection to write: `TestOptionalPolicyRequestFieldsContract` (contract_test).

### REQ-C9-054

**Correctness.** A side-by-side installation uses reviewed namespaces and node ports, leaves existing namespaces untouched, refuses namespaces held by any chart's release and isolates its controller by the server namespace instance ID; it borrows compatible definitions without owning or deleting them, tests every admitted leaf form, controller-written status and paused schedule, and recreates missing own definitions through the chart. Without this, installing beside another release can change its namespace, collide with its ports or delete shared definitions. Inspection to write: `install` (walk_step).

## C10

### REQ-C10-001

**Correctness.** Every submitted CronWorkflow and its child Workflow metadata carry the native controller-instance label with C9's installation instance ID. Without this, another controller can fire or execute the same schedule. Inspection to write: `push` (walk_step).

## C11

### REQ-C11-001

**Correctness.** Audit tail shows every new audit entry exactly once, including entries added during the read. Without this, users can miss audit entries or loop forever. Inspection to write: `TestCompleteAuditTailContract` (contract_test).

### REQ-C11-002

**Ux.** Redeeming an invitation prepares all local files first and checks the returned certificate matches your key, so a used invitation always ends in a working login or "ask for a new one". Without this, onboarding can consume an invitation without leaving a usable matching credential. Inspection to write: `connect` (walk_step).

### REQ-C11-007

**Correctness.** The CLI writes credentials only into a directory it owns that others cannot write, uses owner-only files and removes partial credential plaintext when a write fails. Without this, a command can trust a substituted credential directory or leave private client material exposed. Inspection to write: `TestOwnedPrivateCliFilesContract` (contract_test).

### REQ-C11-012

**Security.** Activate a newly issued bearer only after its full one-time output and durable binding to exactly one uplink succeed. Without this, an undisclosed or unbound token can become live. Inspection to write: `TestOneTimeBearerActivationContract` (contract_test).

### REQ-C11-013

**Security.** The CLI sends saved credentials only over verified HTTPS to their bound URL and authority, refuses a differing CA override before using saved identity, and ignores settings files other users can write. Without this, a command can disclose saved credentials over plain HTTP, to an overridden endpoint, or to the same URL under a different CA. Inspection to write: `TestInstallationBoundClientAuthorityContract` (contract_test).

### REQ-C11-021

**Correctness.** The server reports not-ready when it cannot serve (invalid certificate, database stuck 90 seconds) and stays alive while its own loop progresses, even while waiting on other services. Without this, users receive traffic on a broken server or see a healthy server repeatedly restarted. Inspection to write: `TestHonestResponsiveHealthContract` (contract_test).

### REQ-C11-022

**Ux.** If a local `.local` server name does not resolve, the CLI briefly asks the system's multicast DNS resolver and still checks TLS against that name. Without this, a default local installation can appear unreachable. Inspection to write: `connect` (walk_step).

### REQ-C11-023

**Ux.** Setup generates only the keys needed for the chosen connection, requires no SSH key for a browser-only identity and shows the identity, scope, profile and repository binding in use and how to change each. Without this, a normal connection fails opaquely or uses the wrong profile. Inspection to write: `connect` (walk_step).

### REQ-C11-024

**Ux.** `oberth promote <sha>` needs no input file and reports the tested run, the new main commit and the CI run at that commit. Without this, promoting a tested commit requires manual preparation. Inspection to write: `promote` (walk_step).

### REQ-C11-025

**Correctness.** The server starts each part once, drains HTTP before stopping parts in reverse order, joins workers before closing storage and refuses missing parts or settings. Without this, shutdown can lose accepted work or use closed dependencies. Inspection to write: `TestServerShutdownContract` (contract_test).

### REQ-C11-026

**Ux.** Run results through oberth mcp carry a rendered text block with complete reasons after the server's JSON. Without this, the agent cannot show the same complete run view as the CLI. Inspection to write: `connect` (walk_step).

### REQ-C11-027

**Security.** The server renews its leaf certificate at two thirds of its life with the same key, name and validity checks as install, and serves its checked CA chain; readiness requires a currently valid leaf. Without this, a running installation can lose usable trust or claim readiness with an expired certificate. Inspection to write: `TestServedLeafRenews` (contract_test).

### REQ-C11-028

**Ux.** After install or join saves the SSH endpoint, repo add registers the repository and checks that the forge accepts its deploy key as administrator, or binds a visible registered repository as a joined caller; its per-remote file:/// address has pack commands that use that SSH endpoint, effective fetch/push addresses and transport policy are checked with a remedy on refusal, only its own configuration is changed, every other remote and personal setting is preserved, and the next push with oberth.run=1 is the smoke run. Without this, connecting a repository requires several manual steps or leaves an unusable checkout binding. Inspection to write: `connect` (walk_step).

## C12

### REQ-C12-001

**Security.** The agent instructions require MCP registration with the verified CLI's absolute path, rejecting a bare name or project executable. Without this, opening a project can execute its code with client credential paths. Inspection to write: `TestAgentRegistrationContract` (contract_test).

### REQ-C12-003

**Correctness.** Serialize CLI installation and atomically publish only executable bytes verified under C13's key-only release trust, keeping the current working executable after a failed update. Without this, concurrent updates can leave the agent without a working CLI. Inspection to write: `TestAtomicCliInstallationContract` (contract_test).

### REQ-C12-007

**Ux.** The agent instruction file names only existing commands and tools, uses neutral wording and is read whole in fewer than 100 lines. Without this, the agent follows missing commands or misses part of the guidance. Inspection to write: `ux-gate` (walk_step).

### REQ-C12-008

**Ux.** The optional run box shows active and watched runs using only public CLI JSON through the verified executable and the agent uplink profile, without delaying session start. Without this, agents lose the run view or read a different scope from their tools. Inspection to write: `ux-gate` (walk_step).

## C13

### REQ-C13-001

**Correctness.** The first release is made by hand with hack/release.sh and the documented release steps; automated publication follows cutover and uses only an admitted release run with short-lived delegated credentials, with the signing key as the one long-lived secret reaching only its granted signing step. Without this, a release can publish substituted bytes or expose broader credentials. Inspection to write: `TestExactShortLivedPublicationAuthorityContract` (contract_test).

### REQ-C13-003

**Correctness.** The release's component list describes exactly the binaries and libraries shipped and where each came from, built only from verified inputs. Without this, a release can describe or sign bytes different from those built. Inspection to write: `TestReproduciblePackagedBytesContract` (contract_test).

### REQ-C13-013

**Correctness.** Publishing a release record never moves "latest" backwards, updates only its own aliases, and deletes only objects it confirmed it owns. Without this, a retry can publish an older release or delete unrelated outputs. Inspection to write: `TestMonotonicReleaseFinalizationContract` (contract_test).

### REQ-C13-016

**Ux.** One build target produces the matching server image, source-plugin image and chart for local use and release, with declared startup commands that require no application-time registry lookup. Without this, local development assembles missing artifacts by hand or fails before source delivery. Inspection to write: `push` (walk_step).

### REQ-C13-017

**Ux.** From fresh install, reach the first green build in at most 8 commands and 10 flags, then the first release in at most 4 more commands; missing time baselines or 25% time growth fail the gate. Without this, the normal path gets longer and slower without review. Inspection to write: `ux-gate` (walk_step).

### REQ-C13-018

**Correctness.** With pinned build inputs provisioned, the complete check runs without external network access and never installs or substitutes a check tool. Without this, a normal check depends on an external service or silently changes its toolchain. Inspection to write: `ux-gate` (walk_step).

### REQ-C13-019

**Security.** Guard diagnostics never disclose a private name or matched pattern, including through filenames, link targets or malformed policy. Without this, a guard refusal can publish the private text it is meant to keep out. Inspection to write: `ux-gate` (walk_step).

### REQ-C13-020

**Correctness.** Branch and promotion CI use one build workflow, with every declared template admitted; no promotion-file selection or skipped-template admission path exists. Without this, the tree needs a second routing mechanism or admits different work from what it can execute. Inspection to write: `push` (walk_step).

### REQ-C13-021

**Security.** Release signatures are checked only against the pinned public key, with no transparency log, and signing needs no network; one provisioning command updates C13's canonical key and C12's generated fetch key material together. Without this, release consumers can trust different keys or a stale fetch pin. Inspection to write: `release` (walk_step).

## C14

### REQ-C14-001

**Correctness.** Documented commands and examples are checked against the current release, and examples pass branch and tag names to shell steps as quoted arguments, never inside script text. Without this, users follow commands or defaults the product does not support. Inspection to write: `TestCurrentPublicDocumentationContract` (contract_test).

## Retired ids

These ids are never reused.

| Id | Where it went |
| --- | --- |
| REQ-C1-001 | There is no failed-login table. |
| REQ-C2-016 | merged into REQ-C2-002; administrative scope and audited effects share one contract |
| REQ-C2-017 | retired with REQ-C3-005; no cross-call scratch cleanup |
| REQ-C3-004 | merged into REQ-C3-003 |
| REQ-C3-005 | retired; no scratch survives one call, so no C3 recovery cleanup exists |
| REQ-C3-006 | moved to REQ-C2-019; C2 owns release receipts and release.published |
| REQ-C5-001 | Held steps use live inspection and release; native results remain authoritative and the original code is diagnostic text. |
| REQ-C7-001 | merged into REQ-C9-004 |
| REQ-C7-013 | There are no promotion grants. |
| REQ-C7-016 | dropped; chart owns the database path |
| REQ-C9-015 | dropped; no external witness |
| REQ-C9-043 | merged into REQ-C5-002 |
| REQ-C11-004 | merged into REQ-C1-002 |
| REQ-C11-005 | dropped; shared decoder contract applies |
| REQ-C12-002 | The agent client uploads no deploy key and edits no host settings; C11 owns the joined profile and pinned transport. |
| REQ-C12-005 | merged into REQ-C11-023 |
| REQ-C12-006 | REQ-C11-028 owns the single repository connection command and its smoke push. |
| REQ-C13-004 | README walk and parity capture rule |
| REQ-C13-011 | merged into REQ-C9-038 |
