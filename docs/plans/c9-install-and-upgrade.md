# C9: Install and upgrade

## Purpose

C9 turns reviewed installation choices into a working deployment and maintains it
through upgrades, rollback and backup. It owns `internal/installer`,
`internal/setuptui`, `internal/secretstore`, `internal/kubeconfig`
and `charts/`. Plain prompts end with a review
screen; unattended commands use the same engine. C9 does not create clusters,
install host prerequisites, manage ingress, schedule runs or rewrite workloads.

## Owned data and public contract

The [Shared contracts](README.md#shared-contracts) define shared values. C2 owns
authority and grants; C5 owns executor policy content; C7 owns snapshot inspection
and archive encoding; C11 owns commands, client profiles and certificate renewal.
C9 uses injected ports and the narrow client, never concrete service or store code.

| Surface | Contract |
| --- | --- |
| Inputs | **Delta:** Flags or plain prompts fill `Choices`; the newest applied installer revision fills unset fields. Credentials are private file references. Validate all independently knowable problems together. |
| Review | Show context, cluster identity, namespaces, addresses, images, source registration, controller settings, store, credential paths and every write. Show the policy choices defined in invariant 7. |
| Plan and apply | Resolve inputs, installed revision, target release, user values and identities once. Preview and apply use that same operation; recheck mutable target and ownership facts before writes. Changed facts require a new review. A plan proves only its named reads, never an unperformed enforcement check. Unknown checks stay unknown. There is no saved-plan service. |
| Commands | **Delta:** C11 exposes `install` with `--dry-run` and `--verify`, `upgrade`, `rollback`, `backup`, `unseal`, `secretstore setup`, `secretstore check` and `secret put`. `oberth restore --dry-run` inspects a complete installation archive offline; no restore apply action or restore-completion audit action is shipped yet. There is no uninstall command; the install guide gives the native release-removal procedure. `secretstore setup` has no `--print`. The in-step command is `secretstore exec`. Verification is read-only; there is no policy probe or secret-policy sync. |
| Completion | Exit 0 means the requested operation and checks completed or an explicit no-op succeeded. Install has no forge step; exit 3 belongs to `repo add`. Phase events drive plain progress lines and timings, including finish and total. Preserve `upgrade.completed` after verified completion. |
| Saved access | Save a private profile with the server URL, CA, client certificate, SSH endpoint, SSH trust and public installation metadata. Record the administrator's push key as `AdminKey`; every consumer uses that field, and printed commands use its saved path. Successful renewal also removes abandoned digest-named administrator .key.tmp files. Print the exact paths, deploy public key, selected identity and one next action. Public metadata contains no credential bytes or readiness verdict. Imported public metadata cannot supply credential paths. |
| Chart | One server replica/PVC, separate pipeline namespace, services, Secrets, scoped permissions, source registration and native controller integration. Defaults and explicit false/zero/empty choices survive rerun and upgrade. Remove unread values. |
| Maintenance | Results bind cluster/context, namespace, versions, chart revisions, schema, backup and rollback paths. C9 owns backup transfer and encryption. C7 owns database validation and the bounded helper protocol. |

**Delta:** Store rules are static: the server's writer role can write or delete but not read
step secrets; a step's templated policy reads only its own account's subtree. The private delivery listener fetches only with the live verified requesting Pod's identity, never the writer role; its terminal unavailable failures produce a durable C8 receipt before refusal, and success supersedes old secret failures. Header and bounded body validation, fresh authentication and the live pipeline-namespace check precede delivery slots: one per Pod, four per run, and source-client-burst divided by three globally (at least one). Capacity returns 503 with Retry-After 1 and creates no receipt; the step client retries only that response with one-to-four-second backoff within its deadline and a thirty-second transport ceiling. Step files remain private memory, and the step session is revoked before command execution.
Grants change no store rule. The server creates step accounts through C5.
[C4 store grants](c4-admission-rules-and-plan.md#owned-data-and-public-contract)
owns the single-account selection and complete exact-grant coverage. Store setup
and checks use C9's verified client; store admin credentials never reach the daemon.

## Backup

**Delta:** Complete backups use age with the operator’s backup identity. They contain the
canonical database archive and report, all five installation Secrets and, when authorized, a native bundled-store
Raft snapshot. The installer verifies the live cluster UID and administrator ownership before
reading Secrets or requesting a ten-minute projection for the dedicated `oberth-backup` account.
Its store role has snapshot read and self-revocation only, bound to that account in the server
namespace. Fresh setup provisions the role. Missing backup account/role access on older stores
omits the snapshot while retaining database and Secrets; archive metadata and output say so.
Secrets and store data stream into encryption without plaintext staging. The stream has the configured
C7 size bound plus framing allowance. Each member has a digest checked by offline restore review.
Publication is a hard link which never overwrites; absent directories and unsupported hard links
refuse. Verify staging identity before publication/cleanup; cleanup authority ends at first unlink.
Dry runs create no archive or cluster work. `--database-only` and pre-upgrade archives explicitly
exclude store data and installation Secrets. External stores remain their operator’s responsibility.
Restore review checks age authentication through EOF, database integrity and complete inventory,
without cluster calls or restored files. Applying a restore and a default local bounded-retention
schedule, existing-store backup-role setup and store snapshot validation remain stage 2 work. Keep age and unseal identities separately, outside the archive.

## Installation answers and shared resources

**Delta:** The server namespace and SSH/HTTPS node ports are installation answers,
not fixed chart names: `--namespace`, `--pipeline-namespace`,
`--ssh-node-port` and `--https-node-port` select them. Every namespace-derived
value follows the chosen server namespace. Server and pipeline namespaces
differ; both roles refuse `default` and names beginning `kube-`.
Review shows both ports and checks the cluster's
allocation rules and existing holders before writes; a conflict names its holder
and asks for another port in interactive use.

**Delta:** An installation marks only namespaces it creates and never updates an existing
namespace. Namespace role and holder checks read the native releases, including
their installed namespace, stored manifest and recorded answers; marks alone are
not proof. A namespace rendered by any chart's release is held, regardless of
chart identity. Refuse a namespace held in either role by another release.
An existing installation keeps its recorded pipeline namespace. During apply,
before the first cluster write, save namespace and port answers so an interrupted install
can resume. Explicit answers may replace that record only when no release revision
exists; a pending or deployed revision must agree with its recorded answers.

**Delta:** Existing workflow resource definitions are borrowed, never adopted, owned or
included in the installation's deletion reach. A partial or incompatible borrowed
set refuses before writes. Compatibility requires every resource kind and template
form the product submits to survive unchanged: Workflow, CronWorkflow,
WorkflowTemplate, ClusterWorkflowTemplate and task results, including container,
script, containerSet, resource and suspend leaves, inline DAG and inline steps,
source artifacts and outputs. Samples cover every admitted leaf form,
controller-written status and a paused schedule at their native write paths.
Check native group, scope and served/storage versions as well as payload
preservation. Owned
definitions keep their normal chart update path; the chart recreates missing
definitions it owns. The install guide warns that deleting the release that owns
shared definitions removes every installation's runs.

## Native release history

**Delta:** A foreign release revision is an ordinary native revision. Its real
number, status, version and manifest govern version checks, apply and deletion:
objects absent from the reviewed chart are removed as by the native release tool.
It is neither local nor verified; its values are never read. User choices come
from the newest applied installer revision, with new defaults below those choices
and explicit answers. Approval binds the foreign revision numbers. There is no
substituted baseline and no skipped downgrade check.

A stale operation record is superseded by the next upgrade. An interrupted apply
over a foreign deployed revision can be repaired by the reviewed re-apply.
Rollback refuses only when a foreign revision is newer than its recorded target.
Upgrade accepts verified releases only; `--chart` belongs to install. Native
history retention can prune every installer revision; that refusal states the
limit and a usable remedy rather than inventing installation history.

Release inputs follow [Release verification](#invariants); completion follows
the Completion contract above.

## Allowed dependencies

[dependencies.json](dependencies.json) gives exact imports. Installer uses the
narrow administration client, shared runtime values, name validation, bounded
configuration readers and C7's snapshot and bounded helper protocol. It imports
no service, wiring, native executor, source implementation or concrete Store.
`internal/setuptui` asks, reviews and displays engine progress. C11 composes ports.

## Invariants

1. **Read before approval.** Plan, dry-run and verification make no cluster,
   identity, policy, client-profile or credential writes. Temporary local files
   are private and cleaned. Return independent offenders together, with their
   remedies.
2. **Exact target.** Every child command and SDK call uses the selected context
   and verified cluster identity. Never change current-context or the user's
   kubeconfig. Private merged copies have owner-only permissions, cancellation
   cleanup and conservative recovery. There is no cluster-type trust bypass. Enter selects a shown single-context default; multiple contexts require an explicit choice. A URL derived from a loopback API host requires a reachable URL prompt or --url. Choice markers record explicit input; saved origins remain sparse across reruns and upgrades. Rollback restores source provenance and replaces current settings with the recorded source settings and source chart defaults. Generated chart inputs use only the target schema's declared keys.
3. **Current identities.** Clear private buffers and require explicit rotation
   review. Keep server, SSH, audit and store identities across
   reruns and upgrades. Added names preserve the served certificate's names;
   removing a name refuses while the leaf still carries it. CA rotation explicitly
   updates source and client trust. The installation CA is ECDSA P-256; every
   certificate a browser can meet is signed by that CA. Client and SSH keys
   may be Ed25519. Missing identities refuse before writes.
4. **Credential custody.** Choose a safe owner-only sink outside checkouts before
   generation; unattended installs require a new owner-only file. Refuse links
   and unsafe existing files. Save credentials before
   the next operation can lose them, and keep them after partial failure. Private
   client keys stay on the user's machine; installation keys live in Secrets
   that install creates and the chart references. Keys never enter chart values,
   output, timings or diagnostics. The daemon has no store policy-write authority.
5. **Install administrator.** Install generates the admin key pair locally and
   gives the server only its public key in one Secret read at startup. Use
   [C2's InstallAdmin contract](c2-service-actions-and-history.md#administrator-only-actions).
   Install reuses its saved key and certificate or issues a matching certificate
   from the installation CA. It needs no server exec or second authentication
   model. Reuse verifies the target and key binding. Later administration uses
   the authenticated unscoped-admin API. External-store setup retains its separate
   operator authority; bundled-store custody follows [Backup](#backup).
   Only the selected profile holding an existing installation's
   administrator key may update it; any other install into that namespace
   refuses before any write.
6. **Secret authority.** Setup writes the static writer and step rules with exact
   account/namespace/audience bindings and validated names. A step reads only its
   own account's secrets. Admin credentials go only to the verified store Pod.
   Ordinary upgrades and grant changes need no store admin token or policy sync.
   Server CA and projected-token files may be symlinks to non-empty regular files
   within their size limits. Missing, empty, oversized or non-regular targets
   refuse. Replacing a projected-volume link is supported rotation, not a reason
   to reuse stale bytes; this does not relax custody of generated client keys.
7. **Delta:** **Defaults and review.** Baseline Pod defaults and run egress policy are on.
   Pod defaults drop capabilities, disable privilege escalation and use RuntimeDefault seccomp.
   New pipeline namespaces enforce Pod Security `baseline`; new server namespaces enforce `restricted`.
   Existing namespaces keep their labels. Review pairs server/pipeline namespaces with their labels.
   Run egress opens internet destinations and excludes link-local, RFC1918, CGNAT, IPv6 ULA,
   node/API addresses and discovered or operator-supplied cluster Pod/Service CIDRs.
   Exceptions admit DNS UDP/TCP 53, the API Service and backends on their exact TCP ports,
   same-run-namespace Pods, server ports derived from Services and the real bundled-store labels.
   Extra CIDR/port/TCP-or-UDP exceptions are explicit. Internet access may be narrowed to them.
   The installer binds discovered network facts to review and rechecks before writes.
   Saved egress=true uses these rules; review shows old -> new rules before upgrade confirmation.
   Install and upgrade both accept --no-egress; no policy remains when it is selected.
   Image pulls use the node network. Enforcement requires a NetworkPolicy-capable plugin;
   install does not claim proof or run a policy probe.
8. **Delta:** **Bound calls.** Planning and verification have a two-minute total budget.
   Every blocking command, read and status poll inherits cancellation and its advertised deadline.
   Apply announces a ten-minute readiness wait; ten-second attempts retry unavailable or other-version servers. Expiry names a last version mismatch.
   Permanent authority or trust failures stop immediately; classify transient retries, preserve causes and join owned work, including verifier dial callbacks.
9. **Delta:** **Native configuration.** Keep server and pipeline namespaces and identities
   distinct. Managed controllers get reviewed settings; external controllers are
   verify-only with actionable corrections. Argo's global run cap defaults to 4;
   zero is unlimited. The cap is controller configuration only; cap-only changes leave
   server and store Pod templates unchanged. The pinned controller reloads the cap. No additional run cap, CPU formula, queue or workload
   resource rewriting. Argo `failedPodRestart` is on with `maxRestarts: 1`. Control Pods have fixed measured CPU/memory requests; only store memory is capped. Controller workflow state and repository-dependent server Git processes remain uncapped. The three control Pods use an installation-scoped PriorityClass of 1000000 with preemptionPolicy Never, above ordinary CI and below system priorities; it is not a global default. Derived control priority values require that the target chart defines its PriorityClass template, including rollback targets. A pipeline-namespace PriorityClass-scoped ResourceQuota refuses CI Pods using that control class.
   Chart roles follow component contracts: no Workflow deletion, no secret-approval
   mirror and no arbitrary exec in the server namespace. The server may create
   and delete step service accounts only in the pipeline namespace.
   C5 owns the task-result Role and admission policy; C9 renders and tests them.
   Policy expressions guard optional fields with `has(...)`; a plain Pod UPDATE
   without `subResource` is admitted. Retain source TokenReview and schedule rights.
   Argo's default background deletion keeps dependent Pods behind the Workflow finalizer. Before Helm removal a scoped pre-delete hook removes only this installation's archive guards, including Cron child inheritance; it uses 50 QPS/burst 100, a ten-minute helper budget and an eleven-minute Job deadline, tested with 500 Workflows. Rollback releases them before the earlier chart and again after readiness; other finalizers remain. Review states the one-time backfill cap of 64 MiB per finished run and advises preserving logs before removal or rollback. By default the controller configuration supplies no `workflowDefaults`.
   Each installation's controller runs with an instance ID equal to the server
   namespace name. C5 and C10 label their native submissions with that ID, so a
   cluster-wide default controller never acts on this installation's runs.
   Operator-supplied defaults that merge into an admitted scheduled child spec
   prevent that child from being recorded by C10's exact spec check.
10. **Release verification.** Follow C13's
    [key-only release trust](c13-own-pipelines.md#release-trust). Before writes,
    verify chart and images against the
    signed `SHA256SUMS` and `release.json`; online, offline and mirrored inputs
    share trust. `--release-dir` accepts the release download layout, including
    a version-named child directory; rollback and recorded repair use these
    same verified offline inputs. A local-chart override is visibly unverified.
    Pin image refs, schema and audit domain; user values cannot replace
    them or their parent keys. Merge new defaults below stored choices and explicit
    overrides, preserving false/zero/empty. Unknown keys refuse. Schema, render and
    ownership checks precede dependency writes; preview uses apply's arguments.
11. **Source-ready apply.** The CA exists before render; source registration is
    part of the same apply with the verified paired plugin digest and endpoint.
    Controller changes roll out the effective executor image index. Digest-pinned
    executor images default to `IfNotPresent`; explicit policies remain explicit.
    The chart refuses server `/tmp` emptyDir below 1856 MiB: two 544 MiB archives,
    512 MiB pack and 256 MiB reserve. Render and check the same quantity, including
    equivalent units, missing values and invalid quantities. Default is 2 GiB.
12. **Maintenance and recovery.** Upgrade shows in-flight runs and does not wait
    for idle. Keep recorded installer settings, review a failed revision and write the
    target-bound rollback record before changes.
    Require a backup or explicit no-backup choice; `--yes` and chart rollback do
    not prove database recoverability. Rollback to a release with the same
    database schema works; rollback across a schema change refuses in plain words
    before anything is changed. Database default bound is 1 GiB, hard maximum
    4 GiB, with framing/extraction bounds and staging-space checks.

After a reboot, the operator opens the secret store by hand with `oberth unseal`.
There is no automatic unseal. Upgrade review explains that a restarted sealed store needs `oberth unseal`, then the upgrade is repeated. Preserve the private recovery file after failures.

The `waiters wake` walk fills global parallelism and any authored Argo semaphore,
cancels one running step and lets another fail. Every waiting run must start within
one minute without a controller restart. If the pinned controller loses waiters,
install pins a version that wakes them or configures native resync to retry them.
The controller, executor and Go module pin upstream Argo v4.0.15 (#16642/#16567). Oberth adds no lock implementation.

## Inspections

| Requirement | Inspection |
| --- | --- |
| [REQ-C9-001](REQUIREMENTS.md#req-c9-001) | `TestResumablePrivateEnrollmentContract` (contract_test) |
| [REQ-C9-003](REQUIREMENTS.md#req-c9-003) | `TestReviewedUpgradeStateContract` (contract_test) |
| [REQ-C9-004](REQUIREMENTS.md#req-c9-004) | `TestBackupIsAgeAndDryRunCreatesNoFiles` (contract_test) |
| [REQ-C9-009](REQUIREMENTS.md#req-c9-009) | `TestVerifiedInstallationTlsContract` (contract_test) |
| [REQ-C9-022](REQUIREMENTS.md#req-c9-022) | `TestExactSecretStoreAuthorityContract` (contract_test) |
| [REQ-C9-027](REQUIREMENTS.md#req-c9-027) | `TestPrivateBoundedSecretDeliveryContract` (contract_test) |
| [REQ-C9-038](REQUIREMENTS.md#req-c9-038) | `TestVerifiedReleaseInstallationContract` (contract_test) |
| [REQ-C9-052](REQUIREMENTS.md#req-c9-052) | `install` (walk_step) |
| [REQ-C9-053](REQUIREMENTS.md#req-c9-053) | `TestOptionalPolicyRequestFieldsContract` (contract_test) |
| [REQ-C9-054](REQUIREMENTS.md#req-c9-054) | `install` (walk_step) |

These inspections are obligations, not claims of completed execution.
`TestResumablePrivateEnrollmentContract` must cover the administrator-key
restriction in invariant 5 as well as interrupted enrollment.
`TestVerifiedReleaseInstallationContract` must cover the complete release-input
and key-only trust contract in invariant 10, including offline rollback and
recorded repair. Both named tests must check their full requirement sentences.
The install walk puts two releases beside one another, repeats an interrupted
first install, checks namespace roles from unmarked release history and borrows
each supported definition/template form without payload loss. It removes only
the new release by the documented procedure and checks the other installation and
shared definitions remain. Upgrade walks use genuinely signed releases, exercise
foreign and pruned history, and repeat rollback and repair without network access.
The install walk also uses a cluster-wide default controller, checks that only
the installation's controller acts on each run and scheduled child, recreates
missing owned definitions, and refuses a second profile without the target's
administrator key before any write.
`TestChartDefaults` checks floor/egress on, global cap 4, paired
plugin registration and one failed-Pod restart. The policy test sends a plain Pod
UPDATE without subResource; the push walk checks finished Pods carry
`workflows.argoproj.io/completed=true` and controller logs have no admission refusals.
`TestSourceScratchFloor` covers equivalent quantities, invalid input and the floor.
`TestStepPodsCanReachServerGitSSH` checks the explicit step-to-server Pod selector
and TCP 2222 rule in default and custom release namespaces.
**Delta:** Install checks saved access, trust, explicit paths, declared image commands and a
real source-consuming run. Upgrade checks preserved settings and the rollback
record. `TestBackupIsAgeAndDryRunCreatesNoFiles` decrypts the produced archive,
checks its complete snapshot and matching report through C7's Inspector, and
proves that a dry run writes no file. This backup production test must also
check the database-only exclusion warning and secret-name-list guidance; complete backups must include store data and installation Secrets and pass offline restore review. It is
not a shipped offline inspection surface. Internal replacement-port tests
do not prove installation recovery. The walk proves manual unseal after store
restart, templated step isolation, restart adoption and `waiters wake`; a plan or
template check alone cannot prove those live facts.

## Retired terms

**Delta:** Identity migration and version fallbacks yield to current explicit
identities. Store policy sync and the policy probe are absent. Native global
parallelism is the only run cap.

The store SDK exports its bounded regular-target reader for configured server
mounts. File refusals retain their field and safe state; trust refusals retain the
store's public reason with the CA field, without bytes or private causes.
