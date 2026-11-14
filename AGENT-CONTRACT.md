# Oberth cross-component contract

This document defines the compatibility boundary between Oberth, repository
uplinks, repository-owned pipelines, Kubernetes, and the configured upstream
Git forge. The server architecture is the authority when this document and an
implementation detail disagree.

## Required inputs

### Source-built maintenance backup

`cmd/oberth-store-backup` is a separate Linux maintenance utility, not a native
MCP endpoint or an ordinary signed-release artifact. Its source/tool hashes,
exact live volume and original schema/audit identity, private recipient custody,
and retained sandbox launcher require a concrete reviewed execution plan.
Export uses the unchanged stable-copy snapshot primitive and read-only schema,
integrity, foreign-key and audit verification. It performs no migration or live
restore. Output write errors are joined even when the pinned crypto tool reports
success; structural DER completeness is separate from mandatory authenticated
private rehearsal. Recovery material covers DB/WAL, not full PVC or external
witness continuity. Never reset witness records to make an old snapshot pass.

In addition to ordinary unit/race/vet/lint gates, backup changes require the
`backup_synthetic` tagged test binary and the fixed
`scripts/qualify_database_backup.sh` invocation in the private no-swap sandbox.
The fixture must pass every assertion and positively join its process/cgroup;
missing sandbox protection is fatal, not a skip. Exact build/invocation and
resource/custody requirements are in [database-backup.md](docs/database-backup.md).

### Operator database backups (#1208)

The released CLI uses SQLite's online backup API through a read-only source
connection and a pinned read transaction. It folds committed WAL pages into one
verified `oberth.sqlite` member, then streams the archive. Source main/WAL data
are not written; derivative SHM reader locks may change. Schema, integrity,
foreign-key and audit verification still precede archive emission. Legacy
main/WAL archives remain readable. The maintenance utility above explicitly
uses `WriteStableVerifiedBackup` and retains its old protocol and 128 MiB bound.

`backup`, `backup database`, `restore`/`--inspect` and `backup export-secrets`
accept the operator's `--max-database-bytes`: default 1 GiB, hard maximum 4 GiB,
zero selects the default, never unlimited. The limit applies to logical pages
and restored database members, not duplicate live WAL versions. Both nested
archives, framing, metadata, ciphertext and decompressed extraction remain
bounded. Large snapshot transfer, local extraction, hashes and restore uploads
stream; no database-sized byte slices. Failed output streams are canceled and
partial files removed. Online plaintext staging is a 0700 directory beside the
source database. Before staging, statfs available bytes must cover the pinned
snapshot plus max(256 MiB, snapshot/4) and 64 KiB metadata. Every Step(128) is
bracketed by rechecks of remaining unallocated snapshot bytes plus that reserve;
file length alone never counts sparse pages as allocated. Refusal aborts and
cleans staging, with volume/free/required bytes and remedies named. A directory
flock serializes snapshots sharing the database parent through emission.
Restore target preview uses the same guarded path and never retries/bypasses a
space refusal. External writers remain outside these observational guards.

SIGPIPE becomes a returned write error so cleanup runs. Before identity/SQLite
startup I/O, the server sweeps same-euid unlocked staging regardless of age;
online backups also sweep entries older than ten minutes. Held locks protect
active snapshots. Cleanup attempts every candidate and logs per-entry failures
without disabling later backups; space checks remain mandatory. Workstation
verification uses its private working volume, and generic inspection/verification
honors TMPDIR. The isolated maintenance writer/verifier explicitly retain their
qualified private scratch storage and 128 MiB protocol.

Default-limit requests omit the new flag for old-server compatibility. A custom
limit may retry an unknown-flag response without the flag only if the old
128 MiB server cap is stricter or equal; local bounds and verification remain.
Unsupported smaller limits, missing backup commands and old cap refusals use
hash-stable raw copies (absolute 4 GiB per file), then snapshot them locally.
No new privileges or server RBAC are granted; operators need the existing
helper-Pod access.

Upgrade warns loudly before mutation when migration is pending or source schema
is unknown. It does not attest/create a fresh backup; `--yes` is never evidence
of backup success. A verified retained backup and its private key are the
operator's responsibility; Helm rollback alone cannot undo a migration.

### Persistent data

Production server PKI names are additive across installs and upgrades. Omitting
`--tls-extra-dns-name` or `--tls-extra-ip` preserves both the existing role
permissions and the deployed TLS names. `oberth preflight pki` checks the role
against the deployed configuration and the public certificate currently served;
it requires `BAO_TOKEN` and supports `--namespace`, `--openbao-namespace`, and
`--context`. Both install and upgrade accept `--tls-remove-dns-name` and
`--tls-remove-ip`, but refuse removal while the public leaf still carries that
SAN. Retire the name from the served certificate before revoking its permission.
PKI upgrades refresh the tunnel CA pin; after CA replacement, other clients
pinning the old CA must refresh their trust anchors.

The upstream default branch cannot be deleted through a receive or receive
replay, including by an administrator. Authenticated uplinks retain deletion
access to other branches within their existing repository scope, as part of
their push access. Each upstream deletion requires a durable `branch.delete.intent`
audit record before Git runs and a `branch.delete.succeeded` or
`branch.delete.failed` outcome, including the actor, branch, previous SHA and
receive event ID. Audit failure prevents an unaudited upstream deletion.

Upstream branch deletion records (`branch_deletion_list`) end `propagated`,
`failed` or `skipped`; `error` carries the reason line. An archived upstream
(the forge refuses the push as archived) is final after one attempt: `skipped`
with "upstream archived", the mirror deletion kept, never restored. A `failed`
record becomes `skipped` with a reason line, without any ref change, when a
later deletion of the same branch propagates, when an hourly sweep finds its
branch gone from the mirror, or through the unscoped-admin, audited
`branch_deletion_resolve` (#1500). A feature-branch run whose branch was deleted
on the mirror while it published finishes `passed` with that reason line; only
its publication is abandoned.

Stray mirror refs outside `refs/heads/` and `refs/tags/` (such as
`refs/remotes/upstream/*`, which caches configured before v0.17.99 gained from
git's default fetch refspec on their upstream remote) are removed only by the
unscoped-admin `ref_delete_many` (#1504, CLI `oberth branch prune-refs`): a dry
run lists them, each classed `default-branch`, `tag`, `unreachable` or
`not-commit`; a confirmation needs a selection (prefixes or exact
`{ref, expected_sha}`), the dry run's `plan_id`, and `allow_unreachable` for refs
whose target is not reachable from the default branch or a tag. At most 500
refs per call, each deleted on its exact object ID in one local transaction; a
symbolic ref is deleted itself, never its target. `refs/heads/`, `refs/tags/`,
`refs/oberth/` and `refs/replace/` are never listed or changed, and nothing
changes upstream. `ref.prune.intent` lists every ref and object ID before Git
runs; `ref.prune.succeeded` or `ref.prune.failed` records the outcome. The cache's
upstream remote carries no fetch refspec, so fetches and pushes write no
`refs/remotes/upstream/*`.

- One PVC mounted at `/data` stores `oberth.sqlite`, bare Git caches under
  `/data/git`, immutable run workspaces under `/data/work`, retained logs under
  `/data/logs`, and the accepted-push outbox.
- The server owns `/data/git/oberth.gitconfig` and rewrites it at startup. It is
  the only global Git configuration any server-side Git command reads: no
  ambient `$HOME/.gitconfig` applies, matching the system config already
  disabled by `GIT_CONFIG_NOSYSTEM`. It disables `gc.autoDetach` and
  `maintenance.autoDetach` so background maintenance can never `setsid()` out of
  the command's process group, orphan onto PID 1, and leak as an unreapable
  zombie. Because a transport child (`git-upload-pack`, `git-receive-pack`)
  loses `GIT_CONFIG_COUNT` from its environment, this file is the only layer
  that reaches it; do not replace it with environment variables alone.
- Linux CLI Git commands start in distinct process groups. Oberth enables
  unprivileged child-subreaper adoption, observes the leader with
  `waitid(WNOWAIT)`, and keeps that leader unreaped while signaling its group.
  This pins the group ID against reuse. Adopted zombies are reaped through
  pidfds only after verifying their group and parent; unrelated commands keep
  their own `exec.Cmd.Wait` status. Cancellation/deadline causes are preserved,
  descendant draining has a five-second refusal bound, and cleanup errors are
  surfaced. Procfs and pidfd wait support are required before starting a CLI
  command; no capabilities, hostPath or Kubernetes RBAC are added. Managed Git
  maintenance must keep descendants inside their command group; the reaper
  never signals outside that group. Darwin uses kqueue to keep the same
  leader-before-signal invariant and native init orphan reaping; other platforms
  refuse CLI process execution before forking.
- The chart's `prepare-caches` init container creates the CI cache root
  `/var/cache/oberth/ci` and release cache root `/var/cache/oberth/release` and
  hands them to UID/GID `65534` before the server starts; no manual node
  preparation is required. The two roots must not be equal, nested, aliased, or
  located under a critical system directory.
- Native Git CLI is the only source backend. The runtime is scratch with static
  Git, static Oberth, the `oberth-git-ssh` alias and a CA bundle, UID/GID 65534.
  No shell or OpenSSH is present. Git's GIT_SSH points at the native pinned-key
  transport with GIT_SSH_VARIANT=simple; GIT_SSH_COMMAND is never inherited.
  Single-source promotion retains conservative path conflicts and uses native Git plumbing.
  Ordered merge trains use native three-way merges and always test the combined result.
  Removed backend/shadow flags, chart settings, status fields and counters
  are not accepted. See docs/gitcache.md for explicit values migration.
  The cache init
  and restore file-tool container use the shared digest-pinned BusyBox utility
  image, overridable with chart `utilityImage` / installer `--utility-image`
  for air-gapped mirrors; backup/restore inherit the installed utility image
  and accept `--utility-image` for older deployments without that value.
  Restore probes the target server schema in a separate native init command;
  it never runs a shell in the server image. Runtime public certificate/key
  reads use authenticated admin API names, never filesystem paths. Installation
  uses exactly one server exec to create its first admin; all later admin and
  installer operations use the API. No bootstrap token is stored in a Kubernetes
  Secret. Backup streams a verified, bounded server snapshot over the admin API.
  No maintenance helper mounts a live database into a pipeline.


### Deployment protocol identity

The daemon and in-pod administrative commands read three administrator-owned,
nonsecret environment values once per command: `OBERTH_SCHEMA_IDENTITY`,
`OBERTH_AUDIT_DOMAIN`, and `OBERTH_WITNESS_KEY_INFO`. Fresh-install defaults are
`oberth-schema-v1`, `oberth-audit-v1`, and `oberth-audit-witness-v1`. Explicit
empty values, whitespace, control characters and values longer than 128 bytes
are rejected. The audit encoder appends its fixed NUL domain separator.

Existing deployments must carry their original exact values through their
normal chart upgrade. An operator can first seed these public values while the
previous binary is installed, then upgrade the binary while retaining them.
They are protocol identifiers, not branding: changing an existing deployment's
values invalidates its schema identity, historical hashes or witness key.
Configuration is never inferred from database rows. Inspection, bootstrap SQL,
legacy-schema verification, migrations, every audit append and full/incremental
verification share one immutable per-Store configuration. The original schema
identity CHECK and every applied ledger row remain unchanged; bootstrap SQL is
parameterized only at its two quoted identity operands. The record encoder and
HKDF algorithm remain byte-identical under an explicit existing configuration.

A mismatched schema or existing hash domain fails before migration/recovery;
a writer also verifies the preceding record under its configured domain before
appending, preventing concurrently opened writers from mixing domains. A
mismatched witness domain derives another identity and fails existing external
continuity checks; it must never be used to reset or bypass those checks.

Audit mutation gates verify protected state on a separate read-only WAL
connection, end that snapshot, and perform timestamp/continuity verification
without holding the primary writer connection. A final short BEGIN IMMEDIATE
transaction compares the verified head ID/digest and the same reader's
data_version before committing the gate. Head equality alone is insufficient:
same-head tampering of the prefix, pending rows and receipts must also be checked.
When only data_version changes, the gate ends the writer transaction and
re-verifies a fresh snapshot on the same reader, with at most two extra rounds.
The re-verified head and checkpoint must equal the external proof; external I/O
runs once. Moved heads, changed proofs, tampering and exhausted retries refuse.
Live readers share a startup-cached read-only DSN with query_only and the Store's
busy timeout. A mutation gate has a 30-second total deadline, including waiting
for refresh. Witnessed gates recheck the complete chain. Local-only and
timestamp-only gates may reuse a successfully fenced in-process prefix and
verify its new suffix; outside commits, connection/schema/checkpoint changes,
failed gates and one-minute expiry force full verification. Startup and periodic
cycles still scan the complete chain. Local prefix pins survive fresh cycles
and failed gates, so a valid shorter history cannot clear a rollback refusal.
Writable Store openers create an eight-connection read-only WAL pool after
migration and recovery. Repository/upstream mappings and status/list queries
use it; they never reserve the single writer. SSH identity
and idle/readiness sampler pools open explicitly during daemon startup and keep
their reserved capacity. Audit listings and chain/witness scan queries have a
separate two-connection read-only pool so they cannot exhaust mapping slots;
audit verification snapshots retain their dedicated readers.
Token identity and scope reads share one fresh read-only transaction. Shutdown
claims ownership under the Store mutex and releases it before draining pools
and checkpointing. Private inspection snapshots retain their original
connection policy. Audit cache-key sampling and the final mutation fence still
use the writer connection for their continuity guarantees. See
[audit-verification-cache.md](docs/audit-verification-cache.md) for the cache's
trust assumptions and complete invalidation rules.

### Kubernetes Secrets

- Optional release federation is disabled unless an administrator supplies
  `--argo-release-wif-config` (`argo.releaseWIF` in the chart). This public-only
  configuration binds exact namespace/canonical repository/per-repo release SA,
  named container/script leaf and an administrator-defined DNS-label role (at
  most 32). Existing image-writer/image-reader/chart-writer roles keep their
  behavior; unknown/unmapped names refuse. Optional exact `step_service_accounts`
  mappings select dedicated WIF-only accounts without secret grants or Vault
  roles. They cannot alias shared/repository/CI/other-step identities, consume
  the secret store, or apply to branch/promotion/schedule runs.
  Role pools and targets must differ; CI, shared identities, token opt-outs and
  unnamed/unsupported leaves are denied. A 600-second role-audience JWT and
  nonsecret external_account ADC are projected read-only only into main; helper
  mirrors, ordinary sibling leaves and controller wait receive neither token
  nor credential mount. Existing explicit Vault projections and their separate
  wait protections remain intact. Startup copies freeze the boot capability
  maps. Audited, server-owned runtime repository overrides (`release_wif_grant`,
  `release_wif_revoke`, `release_wif_reset`, #1497) layer on that default; they
  bind only existing roles to a repository's own release ServiceAccount, never
  dedicated step identities. Each admission pins one effective snapshot, whose
  public digest joins the immutable spec/audit binding. No provider, IAM,
  release-consumer cutover or key retirement is activated by shipping this
  feature. See [release WIF](docs/release-wif.md) for separate acceptance gates.
- Administrator-only `argo.releaseTagRoutes` / `--argo-release-tag-routes-config`
  maps canonical repository and tag globs to clean `.oberth/<DNS-label>.yaml`
  release documents. Version tags and explicit administrator routes retain the
  release tier; ordinary tags share build.yaml with branches, promotions and
  schedules. Ambiguous matches, invalid/reserved paths and missing
  selected files refuse. The selected file supplies its own normal size and
  resource contract consistently for sizing, planning, inspection and execution;
  the audit binds its path and commit. Default-branch tag admission and all
  identity gates still apply. Release preflight/identity plans/declarations
  include routed files with their source-file provenance. Dispatch requires
  `Run.RefKind == tag` and the stored bare name. Reserved build/schedule/
  tests contracts cannot be route targets. Dedicated WIF-only step accounts also
  require `step_routes.<template>` pinning an existing exact route file and pattern;
  copying a template into the default file or another pattern cannot reuse it.
  Preflight inspects file bindings without granting execution. See [release tag dispatch](docs/release-tag-dispatch.md).

- The server reads a persistent SSH host private key and HTTPS certificate/key
  from the names configured in the Helm values. When no existing Secret is
  named, the chart generates each once, reuses it on upgrade, and keeps it
  across uninstall (`helm.sh/resource-policy: keep`). The SSH host key also
  derives the deployment's public audit-witness identity and must never be
  regenerated casually.
- PKI leaf renewal swaps the served certificate without restarting listeners.
  Failed renewal makes readiness fail within the last 10% of its lifetime,
  capped at one hour before expiry; an expired or not-yet-valid leaf is always
  unready. Successful renewal clears the error. Administrator metrics expose
  expiry, renewal failure and its bounded diagnostic. Renewal and secret-store
  probe tasks are canceled and joined before server teardown completes.
- Upstream Git authentication is supplied by the configured Secret. Oberth does
  not mint a replacement when one is configured. Per-upstream deploy keys are
  additional data keys (`<keyFile>-<name>` / `<keyFile>-<name>.pub`) of that
  same Secret: `upstream add --dedicated-key` provisions one for that SSH
  upstream and applies it under a per-upstream server-side-apply field manager
  (so no registration can prune another upstream's key), the existing
  whole-Secret volume mount
  projects each as a file on tmpfs, and the server builds a per-host SSH
  config referencing the projected files in place — no Kubernetes API read,
  no key bytes outside the Secret volume, and RBAC stays scoped to the two
  named Secrets. Upstream names are DNS-1123-label-restricted at registration
  because they become data keys, file names, and SSH config path elements.
  Pre-existing upstream rows keep an empty key name and continue on the
  shared fallback key, which the generated config offers only to hosts
  without a dedicated entry (per-host names are negated from the fallback
  block because OpenSSH accumulates IdentityFile directives across matching
  blocks). Activating a freshly added dedicated key requires a server
  restart; until then that upstream's Git operations use the shared key.
- Pipelines without declared secret-store paths receive no secrets on either
  trigger. Pipelines with approved paths receive secret-store-sourced
  credentials: branch pipelines may declare upstream-scoped paths only
  (system paths are rejected at admission); release pipelines may additionally
  declare system-namespace paths against the administrator allowlist. Release
  credentials are only admitted after the pushed tag has been peeled and its
  commit proven reachable from the freshly fetched upstream default branch.
- Secret-store-sourced release secrets (OpenBao) never become Kubernetes
  Secrets. Pipelines that declare approved secret-store paths bind to the
  trigger's own credentialed identity: release runs to the repository's own
  per-repo ServiceAccount when one is provisioned — its OpenBao role's policy
  carries exactly that repository's org/repo upstream namespace plus its own
  approval-table grants — falling back to the shared credentialed
  ServiceAccount otherwise; these are the only identities whose policies
  carry approval-table release grants. CI runs bind the repository's own
  per-repo CI ServiceAccount when one is provisioned — its OpenBao role's
  policy carries exactly that repository's org/repo upstream namespace and is
  structurally grant-free (no approval-table grants, no release secrets) —
  falling back to the shared ci-secrets ServiceAccount when no per-repo CI
  identity exists. Per-repo CI identities are always created alongside their
  release counterparts by the installer, so every repo with grants gets both
  tiers. Each role binds its exact ServiceAccount name, so release credentials
  are unreachable from a branch push at the Vault layer, not only at
  admission, and a CI run can only reach its own repo's upstream namespace,
  not every registered org's subtree (issue #433). Before creating a
  credentialed Job, argojob verifies the selected per-repo ServiceAccount
  actually exists in the pipeline namespace and fails closed with a
  remediation message when it does not (the materialization gap: a grant
  recorded before the installer resync has an identity on paper but not in
  the cluster). Contract: this verification requires the chart's server Role
  in the pipeline namespace to grant read-only `get` on `serviceaccounts`
  (`charts/oberth/templates/rbac-argo.yaml`); removing that rule turns every
  credentialed CI and release run into a Forbidden failure at Job creation
  (issue #467). Release preflight (`secretstore verify --release-tier --repo`)
  additionally requires `create` on `serviceaccounts/token`. The established
  server Role scopes this to the shared credentialed ServiceAccount; a separate
  Role and RoleBinding scope it to the exact `argo.perRepoIdentities` release
  accounts when that list is nonempty. Both bind only the server ServiceAccount
  in the pipeline namespace (issue #478). This grants
  the server identity token requests, not the pipeline identities; CI-tier,
  executor and unrelated accounts stay outside the token allowlist. Two
  credential chains are supported:
  - **Native (preferred):** `oberth secretstore exec` first pins a no-symlink
    `--dir` root, checks effective-user ownership and tmpfs backing, refuses
    setuid/setgid, tightens an existing public or sticky emptyDir root to
    `0700` through its pinned descriptor, and rechecks before any OpenBao
    login or fetch. A prefetch refusal may report only root UID/GID, effective
    UID, mode bits, and filesystem kind for diagnosis, never credential bytes,
    paths, or directory entries. The numeric diagnostic is retained in the
    failing step's normal redacted log and available through `run_logs` after
    the container exits; no arbitrary filesystem diagnostic endpoint exists.
    It then authenticates in-Pod using the
    ServiceAccount's projected token, fetches the declared paths, and writes
    files relative to that pinned descriptor at
    `$OBERTH_SECRETSTORE_DIR/<name>/<key>` (`0400`, memory-backed emptyDir),
    strips credential env vars, and wraps the child's streams in redaction.
    The server delivers the oberth binary into the source claim at seeding
    time and injects the secrets emptyDir volume automatically. Owner-private
    mode confines access by other UIDs and public paths, but does not by
    itself isolate secrets from a same-UID Argo `wait` container. Native Argo
    wait mirrors are retained. The executor token separately keeps its random
    relocated path in private memory-backed storage; that protection does not
    depend on deleting the secret or main release-token mirror.

  Values and value-derived digests never appear in etcd, Job annotations,
  node disk (on swapless nodes or verified RAM-only zram), or server-side
  logs; only names and paths are annotated. `secretstore exec` exempts active
  swap only for canonical `/dev/zramN` partition entries whose kernel
  `/sys/block/zramN/backing_dev` reads `none`. Disk-backed zram, disk/file
  swap, mixed devices, and unknown/unreadable/malformed device metadata retain
  the warning or `OBERTH_REQUIRE_SWAPLESS` refusal. A device name alone never
  establishes memory-only storage; the existing absent `/proc/swaps`
  portability behavior is unchanged.
- Installer-managed production OpenBao starts with verified TLS from its first
  listener: a credential-free bootstrap Pod writes the CA and server keys
  directly to the retained OpenBao PVC before Helm creates the StatefulSet.
  Private keys never enter Helm values, a Kubernetes Secret, etcd, or pod
  logs. Only the public CA certificate is copied into Oberth's ConfigMap trust
  mount. Existing HTTP installs stage the same bundle on their current PVC,
  publish the TLS config, then roll the chart's OnDelete pod.

### Repository contract

- A repository provides its pipeline as Argo Workflow YAML in the `.oberth/`
  directory: `.oberth/build.yaml` for branch-triggered CI and
  `.oberth/release.yaml` for tag-triggered releases. Both are standard
  `argoproj.io/v1alpha1 Workflow` resources. Oberth sets `metadata.name`,
  `metadata.namespace`, and labels (`oberth.ci/repo`, `oberth.ci/trigger`,
  `oberth.ci/ref`, `oberth.ci/sha`) at submission time.
- Oberth forces the `serviceAccountName` via a trigger-and-path-gated switch:
  a pipeline that declares no approved secret-store paths binds to the
  pipeline ServiceAccount with no Vault/OpenBao access and
  `automountServiceAccountToken: false`; a release pipeline with approved
  paths binds to its repository's per-repo ServiceAccount when provisioned
  (the shared credentialed ServiceAccount otherwise), and a CI pipeline with
  approved paths binds to its repository's per-repo CI ServiceAccount when
  provisioned (the shared ci-secrets ServiceAccount otherwise) — each carrying
  a projected token that only its own identity's OpenBao role accepts, with
  the selected role injected as `OBERTH_VAULT_ROLE`. When other repos have
  per-repo identities and the triggering repo does not (either tier), the
  run is refused at admission with a message directing the operator to
  provision the identity via the installer — falling back to a shared SA
  when per-repo isolation exists for other repos would leak cross-repo or
  cross-org secrets. The CI system-path prohibition (branch pipelines may
  not declare system-namespace paths) and the approval-table grant check
  are enforced as defense in depth on top of the identity switch. Secret paths an
  `oberth secretstore exec --path` invocation would fetch are
  admission-checked against the declared annotation. The YAML must not declare
  `serviceAccountName`.
- **AI-CONTRACT:** Explicit `automountServiceAccountToken: false` remains false
  through forced identity assignment, including workflow/default opt-outs and
  inline descendants. Tokenless leaves receive no injected Vault environment,
  CA/server-binary or credential-chain mounts; a recognized credential command
  in a tokenless leaf or its defaults is refused. Argo's separate executor token
  remains confined to its init/wait containers.
- Template annotations `oberth.ci/workspace-mounts: none|cache|readonly` and
  `oberth.ci/workspace-env: none` reduce automatic workspace access. `none`
  omits automatic work/cache/artifact mounts and their injected environment,
  preserving explicit non-reserved mounts and the read-only source checkout.
  `cache` is `none` except that the per-repository, tier-scoped persistent
  cache stays mounted at `/work/cache`; `GOMODCACHE`, `GOCACHE` and
  `OBERTH_CACHE_DIR` are then set inside it even with `workspace-env: none`,
  and are absent when the tier has no cache root. It is a subset of the
  default mounts. CI-tier trust model: within one repository and tier any
  pusher's branch run can plant `GOCACHE` objects that the next run links in
  (pre-existing for default-mode leaves). So `cache` is refused on CI-tier
  leaves that run a credential chain, and admitted on release leaves,
  credentialed or not, because only admitted release runs write the release root. Any
  other value is refused by admission and `oberth validate`.
  `readonly` covers shared aliases and Argo wait mirrors. Authored read-only
  requests survive mount-path replacement and protect overlapping subpaths
  across containers; dynamic subpaths are conservatively overlapping. Fixed
  run/release identity environment remains server-owned. These controls belong
  on Pod leaves (container/script/containerSet, including inline leaves);
  workspace isolation with templateDefaults is refused. The exact interface
  and controller-proof limits are in [workspace isolation](docs/workspace-isolation.md).
  Helper mount mirroring is resolved once before credential injection, with
  explicit helper paths taking precedence; generated credentials are never
  mirrored into repository helpers. Argo's native `wait` credential mirrors are
  retained; source and explicit read-only workspace mirrors stay protected.
  Container-set common
  mounts are normalized before policy, and a server patch binds their
  referenced volumes to the admitted definitions and this Workflow's own
  generated claims.
- Oberth injects the source checkout into every container template as a
  read-only mount at `/work/src`. Templates set `workingDir: /work/src` but
  do not declare the source mount themselves.
- Oberth injects a persistent Go module and build cache into every container
  template as a writable mount at `/work/cache`, and sets `GOMODCACHE`,
  `GOCACHE` and `OBERTH_CACHE_DIR` to point inside it. The mount is node-local
  storage under a server-configured root (`--ci-cache-root` /
  `--release-cache-root`), scoped to one directory per repository, and the
  trigger selects the root — so a branch build and a release build of the same
  repository never share a cache directory. The YAML must not declare the cache
  volume, name its path, or set `GOMODCACHE`/`GOCACHE`: admission refuses a
  repository `hostPath` outright, and a repository value for either variable is
  overridden unless workspace environment injection is opted out as above
  (a `workspace-mounts: cache` leaf still receives the server values).
  Only these two caches are shared; tool binaries stay on the run's
  own ephemeral volume. When no root is configured for a tier the mount is
  absent and runs simply start cold.
- The server reads YAML statically at submission -- it never executes
  repository code in the server process.
- Argo owns Workflow concurrency, locks and the queue; Kubernetes owns Pod
  placement. Oberth submits each validated run without an application capacity,
  pressure or disk-floor gate, request defaults, automatic release lock or
  priority mapping. Repository YAML owns requests, limits, placement, native
  priority, retries, optional deadlines and synchronization. See
  [native scheduling](docs/native-scheduling.md).
- A named scheduled run durably records its resolved schedule entry in
  `runs.schedule_entry` (v31). Cron and manual
  schedule triggers share that identity. Retry, rerun,
  replacement and Workflow adoption retain it. Historical
  rows stay empty; no current file is used to guess their entry. Non-schedule
  runs cannot carry a nonempty entry. `oberth-schedule` is a reserved,
  server-owned Workflow parameter: any repository declaration in
  `spec.arguments.parameters` is refused. Named schedules always receive it;
  other runs receive empty only when their document references it, otherwise
  it is absent. Named entries have separate same-branch supersession domains.
  Due cron entries queue even when the repository has active runs; Argo and Kubernetes policy govern execution. Cron atomically checks the
  consumed cursor and all active runs of that entry/branch before enqueue:
  overlap records `coalesced` and advances the cursor without replacing the
  live run; `fired` commits its run/receive/audit/cursor together. Read failures
  fail closed and are logged; cursor-read failure reports `failed_state`
  without advancing. Manual schedule triggers
  retain their busy-repository guard unless forced. Run list/detail expose
  `ScheduleEntry`; status/wait expose
  `schedule_entry`. See [scheduled runs](docs/scheduled-runs.md).

- Source receipts bind the exact reviewed source to submission.
  They carry no aggregate resource authorization or packing budget.

- Native resources and placement are preserved as authored. Oberth has no
  compute admission, numeric grants or resource resizing. See
  [native scheduling](docs/native-scheduling.md).

- Every compiled Workflow receives server-owned parameters `oberth-repo`,
  `oberth-ref`, `oberth-sha` and `oberth-trigger` (`branch|promotion|schedule|tag`).
  Release-tier runs also receive `oberth-release-tag` and `oberth-release-sha`;
  these copy the exact ref and SHA. Missing ref/SHA facts are refused at Build,
  rather than submitted empty. They are absent on ordinary CI runs, including ordinary tags.
  The entire `oberth-` parameter prefix is reserved: authored parameter
  declarations are refused in workflow arguments, template inputs/outputs and
  invocation arguments. References are allowed without declaring defaults.
  For example, `when: "'{{workflow.parameters.oberth-trigger}}' != 'branch'"`
  selects work by trigger. The run name is `{{workflow.name}}`.
  Branches, refs and tags share the allowlist
  `^[A-Za-z0-9][A-Za-z0-9._/+@=,-]*$` at receive and Build; already-stored
  refs outside it fail Build with the same plain refusal. Version-tag
  classification bounds its suffix by this alphabet and anchors both ends.
  Ref/tag facts must go through env, never `when:`. Output `globalName` also
  reserves the `oberth-` prefix. Assisted step selection uses Argo node names.
  A run that must not overlap another declares a native Argo mutex or
  semaphore. Retired Workflow annotations are inert metadata. The
  `concurrency_groups` tool, `oberth admission groups` and
  `system_status.admission.groups` are gone; the `runs.concurrency_group`,
  `runs.concurrency_limit` historical columns remain unread; the orphaned declaration table is dropped. Concurrency-group membership no longer
  restricts cancellation.
- Workflow resources, placement, parallelism, synchronization, priority, deadlines and retries are authored in repository YAML. Oberth configures only Argo global parallelism; it does not inject release locks or priority, manage semaphore ConfigMaps, or apply CPU/pressure/disk/hold/startup gates. Requests and deadlines are optional.
- Assisted lifecycle settings (`assist.*`), cron entry settings (`schedules.*`), and security profile configuration remain operator-owned. Assisted holds retain their explicit deadline extension and keep their Argo slot and locks.
- Semaphore limits are operator-owned in `oberth-sync-limits` in the pipeline namespace. The fixed name and same-namespace restriction protect ConfigMap data from disclosure through Argo errors. The installer leaves existing semaphore objects and their ownership to operators. Named mutexes and native Argo priority pass unchanged; no release mutex is reserved or injected.
- Oberth does not inject run-fact environment variables (`OBERTH_REPO`,
  `OBERTH_REF`, `OBERTH_SHA`, `OBERTH_TRIGGER`, `OBERTH_RUN_ID`,
  `OBERTH_RELEASE_TAG`, `OBERTH_RELEASE_SHA` or `OBERTH_STEP`). Authors map
  native workflow parameters to env explicitly; authored env names and values
  pass through unchanged. Toolchain/workspace variables, `OBERTH_ARTIFACTS`,
  assist hold controls and secret-store coordinates remain with their live
  mechanisms. See [run facts](docs/run-facts.md).
  The SDK itself is configured through `OBERTH_SOURCE_DIR`,
  `OBERTH_STEP_TIMEOUT`, `OBERTH_SECRET_DIR`, `OBERTH_SECRETSTORE_DIR`, and
  (Apply only) `OBERTH_PLAN_DIGEST`/`OBERTH_PLAN_SIZE`.
- The SDK's declaration vocabulary is flat: `Steps(...)` assembles named
  burns built by `Test`/`Build`/`Release`/`Plan`/`Apply`, each step is one
  `Cmd(name, command, args...)` line, and modifiers chain onto the value
  they configure (`After`, `Timeout`, `Env`, `Size` on steps; `After` and
  `Backend` on burns). The earlier fluent builder
  (`New().Test(...).DependsOn(...).Done()`) and the `With*` step methods
  keep compiling and produce the identical `Pipeline` value. The dumped
  pipeline's JSON wire names (`Command`, `Env`, `Timeout`, `Size`,
  `Backend`) are pinned across SDK generations: `--dump-pipeline` output is
  decoded by CLIs and servers built at other versions.
- Pipeline admission rejects two steps whose command, argument list, and
  resolved environment are byte-identical within one trigger class (CI =
  test + build; release separately). Identical invocations can only
  produce duplicate evidence — the canonical defect is a cross-compile burn
  that never sets `GOOS`/`GOARCH` and greens a platform it never built.
  Cross-compiles pin their target explicitly (`Go.BuildFor` or
  `Env("GOOS"/"GOARCH", ...)`); a release burn may repeat a CI
  verification step because the two classes never run in the same Job.
- A repository may declare secret-store-sourced release credentials with one
  literal `var SecretStoreSecrets = map[string]string{"name": "path"}`
  beside `ReleaseSecrets`. The declaration is statically parsed, never
  evaluated. Two path namespaces exist:
  - **Hierarchical (preferred)** — the virtual `oberth/upstream/` prefix,
    authorized structurally at release admission against the declaring
    repository's identity, with no allowlist entry:
    `oberth/upstream/<org>/<secret>` is readable by every repository of that
    upstream org; `oberth/upstream/<org>/<repo>/<secret>` only by that exact
    repository. `<org>` is the registered upstream base URL's trailing path
    segment and `<repo>` the catalog repository name — matched byte-exactly,
    case-sensitively. The server fetches the value from
    `<secretstore.kvMount>/data/upstream/...` (KV v2; `kvMount` defaults to
    `oberth`, flag `--secretstore-kv-mount`), so with the default mount the
    declared path is exactly the `bao kv put` logical path. Exactly 4 (org)
    or 5 (repo) path segments; the raw `oberth/data/upstream/...` spelling is
    reserved and rejected in declarations and in the allowlist. Upstream
    registration fails closed when a new upstream's `<org>` (or an empty one)
    collides with an already-registered upstream's `<org>`, naming the
    conflict — two upstreams may not share an org, since that would alias this
    subtree across trust domains. Registration additionally validates the
    upstream NAME itself: it must match the repository segment charset, must
    not end in `.git`, and must not be one of the reserved names `release`,
    `data`, `upstream`, `sys`, `metadata`, `receive-outbox`
    (case-insensitive) — an upstream named `release` would make the release
    credential namespace structurally addressable from repository-controlled
    path spellings. Name and org namespaces are kept disjoint in both
    directions: a new upstream's name may not equal an existing upstream's
    org, and a new upstream's org may not equal an existing upstream's name
    (an upstream whose own name equals its own org remains legal), so a
    2-segment `<first>/<repo>` spelling can never be ambiguous between the
    two.
  - **System** — any other KV API path (for example
    `oberth/data/release/cosign-secret`), requiring an exact administrator
    `secretstore.allowedPaths` entry; used by Oberth's own release
    credentials and legacy flat paths.
  A declared path that is malformed, scoped to a different org or repository,
  non-allowlisted (system namespace), or unreachable fails the release run
  before its Job is created. Release steps read the delivered keys as `0400`
  files under
  `$OBERTH_SECRETSTORE_DIR/<name>/<key>` (`/run/secrets/secretstore`, tmpfs);
  the runner refuses to start burns until the delivery manifest verifies and
  fails closed on timeout. Branch burns receive zero secret-store secrets.
- Oberth's own repository Release DAG may publish and verify immutable release
  artifacts, but it receives no upstream-forge mutation credential and performs
  no forge release or ref mutation. Oberth alone publishes the exact admitted
  annotated tag object after every release burn is terminal green. A
  human-facing forge release, if desired, is a post-publication operation.
- The release publisher verifies its existing Cloudflare account token at the
  fixed HTTPS API, then derives temporary S3 credentials locally using the
  documented HS256 JWT protocol. The bucket-only parent has no REST object
  authority: no REST credential-mint fallback or broader grant is allowed.
  The token binds the selected release bucket, `oberth/` prefix,
  `object-read-write` scope and 1800-second lifetime. The token ID remains the
  access key; the parent signing key is ASCII lowercase SHA-256 hex of the
  API token. The temporary secret is SHA-256 hex of the compact JWT, and the
  session is standard base64 of `jwt/` followed by that JWT. Parent verification
  rejects redirects/proxies and uses TLS 1.3; derived curl configuration stays
  in the existing credential leaf's private tmpfs. No credential is printed.
- **AI-CONTRACT:** No unlisted objects survive in `oberth/latest/` after
  `release-finalize`. After alias convergence, the finalizer LISTs
  `oberth/latest/` and DELETEs every key not in the converged set ({VERSION,
  SHA256SUMS, SHA256SUMS.sig, SHA256SUMS.sigstore.json, cosign.pub,
  release.json, release.json.sigstore.json} union {binaries and chart from
  SHA256SUMS}); it fails closed on list failure, truncation, or any key
  outside the `oberth/latest/` prefix (#703/#719).

## Network and wire outputs

### Git SSH

- TCP NodePort `30022` serves authenticated Git smart protocol. The chart's
  steady-state Service is `NodePort` with client-facing SSH port `22`; both
  `NodePort` and `LoadBalancer` Service types and SSH ports `1..65535` may be
  rendered so Helm can adopt an existing Service without changing its live
  traffic path. NodePort `30022` remains fixed in every supported shape.
- A live `LoadBalancer` Service exposing SSH at `2222` is migrated through two
  ordinary client-side Helm revisions: first render that exact live type,
  port, and NodePort so Helm owns the existing ServicePort merge key; only then
  render the desired `NodePort` / `22` / `30022` shape. Hooks, API-side
  mutators, force/replace/delete, and availability bypasses are not part of
  the chart contract.
- Authentication maps the SHA-256 SSH public-key fingerprint to exactly one
  durable uplink identity.
- SSH identity and scope reads use a bounded, separate read-only WAL pool,
  with a fresh committed snapshot for each lookup. No authorization cache or
  immutable SQLite connection is used. The signed authentication request
  rechecks the key and scope after the initial public-key probe, so a committed
  revocation cannot be bypassed by the SSH library's probe cache. Lookup
  failures log the storage error, send a generic temporary-server-error banner
  and close the connection before an authentication denial can be sent.
- Only `git-upload-pack` and `git-receive-pack` are accepted. Receive-pack may
  update only `refs/heads/*` and `refs/tags/*`; shell, PTY, forwarding, helper
  protocols, replacement refs, and other namespaces are rejected.
- Three repository path spellings are accepted and validated segment-wise:
  `<repo>[.git]`, `<org>/<repo>[.git]`, and
  `<upstream-name>/<org>/<repo>[.git]`. All spellings of one repository
  resolve to the same identity — so one repository has exactly one cache
  directory, one lock, and one durable receive reservation regardless of
  spelling. The on-disk layout is org-qualified
  (`<root>/<upstream>/<org>/<repo>.git`, issue #264): the server derives it
  from the catalog's canonical identity via the cache's RepoQualifier, so
  same-named repositories under different upstreams hold strictly disjoint
  caches, and pre-#264 flat directories (`<root>/<repo>.git`) are moved by
  an idempotent, crash-safe startup migration before any listener exists.
  A supplied upstream name must name a registered upstream and a supplied
  org must equal that upstream's org identity; an org-qualified path always
  addresses that org's OWN namespace (never another org's same-named
  repository), and a bare name that exists under multiple upstreams is
  refused with disambiguation guidance — nothing guesses. Nested paths
  beyond three segments are rejected.
- Canonical persistence (#245 G3, schema v11/v12): `repositories` enforces
  compound `UNIQUE(upstream_id, name)`, so the same bare name may exist
  under different upstreams. Store lookups by name accept all three
  spellings; a bare name matching repositories under multiple upstreams
  returns an ambiguity error naming the candidates — nothing silently picks
  one. Cross-repo persisted state keys on the qualified
  `<upstream>/<org>/<repo>` form: `schedule_fires` rows (runtime reads and
  writes resolve the qualified key first and skip the tick when resolution
  fails — never a bare-key fallback write) and `secret_access` rows
  (admission loads grants by repository ID through the qualified key ONLY;
  a bare-keyed row is inert, never aliased onto same-name repositories).
  The access reconciler canonicalizes ConfigMap entry spellings at the
  converge boundary: resolvable entries key the diff by their qualified
  form, an ambiguous bare entry is skipped fail-closed with a loud log,
  and an entry for an unregistered repository stays verbatim and inert.
  The `AddGrantCanonical` and `RemoveGrantCanonical` modify callbacks
  apply the same resolution before comparing: a qualified revoke
  (`codeberg/acme/acme-example-service`) matches a bare-spelled
  ConfigMap entry (`acme-example-service`), removing all canonical
  duplicates; `AddGrantCanonical` deduplicates against canonical
  equivalents. Both preserve the original spelling of surviving entries.
  An ambiguous bare entry is never matched (fail-closed). A truly absent
  canonical tuple returns `ErrInvalidInput`, surfaced as a typed MCP
  error rather than an internal error (#689).
  Migration discipline: shipped migration bodies are append-only — v10 was
  released as a ledger-only no-op and is recorded on live databases, so
  the canonical-persistence rebuild lives at v11 (FK-safe rebuild +
  schedule-fire qualification) and v12 (grant qualification); replacing a
  recorded version's body silently never runs on the databases that need
  it (`TestMigrationLiveLineageAppliesRebuildAfterRecordedV10`).
- Feature branches may be force-updated. Tags are creation-only: deletion,
  movement, upstream conflict, and commits outside the fresh upstream default
  branch are rejected before the public cache ref changes. The same branch/tag
  name grammar filters upstream refs before they become clone-visible. Release
  replay uses the exact admission SHA durably bound before receive-pack, not a
  later default-branch lookup.
- An accepted ref update is durably recorded with its uplink identity before it
  can be forgotten. Recovery replays unacknowledged events idempotently. When
  the pre-finalize gate rejects after receive-pack has applied ref updates, the
  reservation persists in a gate-failed state preserving the original actor and
  admission; replay re-runs the gate for any not-yet-finalized reservation
  before finalization and delivers callbacks with the original attribution once
  the gate recovers.
- A live push receives `remote:` feedback on SSH stderr: the authenticated
  uplink identity, one line per durably processed ref (queued run ID, recorded
  deletion, release admission or rejection reason), and — when the
  `pushBannerURL` chart value is set — a `watch:` link. Display-only and
  additive; startup replay produces no client output.

### HTTPS and MCP

- TCP NodePort `30443` serves TLS 1.3 or newer.
- `/healthz` is a liveness endpoint and `/readyz` reports dependency readiness.
  Like a vault before initialization, a freshly installed deployment stays
  `Running` but not ready until its first upstream is registered in-pod;
  registration then also requires the upstream's durable SSH credentials for
  readiness to hold.
- Readiness sampling has its own read-only WAL connection, a thirty-second budget
  and five-second cadence. A successful sample expires after 90 seconds of
  elapsed time, independent of attempt count; reader deadlines and typed
  SQLite BUSY/LOCKED do not renew it. Confirmed writer failures refuse at the
  next sample without receiving another last-green grace window.
  Thirty-second probes overrun the ticker, which drops missed ticks. Hard errors
  and startup recovery refuse at once. The reader busy timeout is at least
  the probe budget. Configuration checks reuse the sampled upstream rows.
  Stale transitions and the first retained deadline after each success are
  logged; sample age and the retained-deadline count are administrator metrics.
  The chart requires six consecutive five-second HTTPS readiness failures.
- `/mcp` and `/api/*` require an active uplink authenticated by its bound
  client certificate or bearer credential. Static connection guidance may be
  served unauthenticated without exposing repository or run data.
- `POST /mcp/enroll` is a separate MCP boundary with only `uplink_redeem`.
  A random single-use credential, expiry, current server CA pin and URL bind
  enrollment to an invitation created by an unscoped admin. Consumption,
  certificate-only uplink registration, local SSH fingerprint, client SPKI
  and audit receipts commit together; no bearer token is minted. See
  [uplink invitations](docs/uplink-invites.md).
- The dashboard pages `/runs`, `/repos`, `/issues`, `/status`, and
  `/runs/{run}` serve a static shell (plus `/assets/*` stylesheet, script, and
  fonts) with zero repository or run data; the browser holds the bearer token
  in localStorage and fetches everything from the authenticated read-only
  views. Those views are `/api/runs`, `/api/runs/{run}` (run record, burn/step
  results, owning repository), `/api/runs/{run}/logs?burn=&step=` (one exactly
  recorded step's bounded retained log), `/api/runs/{run}/livelog?offset=`
  (polled live slice of a running Job's redacted log stream — bytes from the
  cursor, the next cursor, and the terminal flag; offset -1 starts tailing;
  responses are bounded to 256 KiB per poll), `/api/repos`, `/api/issues`,
  `/api/grants` (read-only promotion grant list; filters `repo`, `sha`,
  `status`; mirrors `promote_grant_list`; no mutation endpoints), and
  `/api/status`. The run views resolve by full run ID or an unambiguous
  hexadecimal prefix of at least 8 characters (the push-feedback ID); responses
  retain the full canonical ID. Prefix resolution searches only runs the caller
  is authorized to see. Ambiguous prefixes are rejected with the matching full
  IDs only; malformed or shorter prefixes are invalid input. These views never
  acquire or renew a CI issue lock; the MCP `status`, `wait`, `issue_get`, and `issue_get_many` tools are
  equally read-only and do not renew locks — renewal happens only through the
  explicit `issue_lock` tool, which is a mutating operation gated by the
  audit mutation gate. `/api/status`
  additionally reports the server version, per-upstream probe results, the
  upstream SSH public-key fingerprint, the secret-store connection summary
  (configuration and, when configured, a TTL-cached Kubernetes-auth login
  probe result — never a token or secret value), the audit mode
  (`audit_mode`: `"local"` or `"anchored"`), and the audit-chain head with
  `audit_chain.anchored` plus the latest external checkpoint when a timestamp
  authority is configured. Background diagnostics use configurable sixty-second
  defaults. Upstream SSH samples share endpoint/credential bindings, back off on
  failure and require three consecutive failures before `unavailable`; `last_error`
  and `checked_at` remain visible while the last healthy state is retained.
  These observations never authorize or refuse readiness, pushes, mirror refresh
  or promotions. See [status probe policy](docs/api-latency.md).
- A bearer credential maps to exactly one uplink public-key fingerprint and
  identity. Plaintext tokens are displayed once and are never persisted.
- MCP `wait` and `promote_status` negotiate POST SSE only for explicit
  acceptable `text/event-stream`; JSON-only/headerless clients and `q=0`
  exclusions keep JSON. Authentication, strict arguments and initial target
  resolution precede streaming. Fixed comments flush immediately and every
  20 seconds, followed by exactly one JSON-RPC `message` event with the request
  ID. The existing maximum wait and overall write deadline remain unchanged;
  each write/flush has a five-second bound. Heartbeat writes and the final
  response never overlap. Disconnect/write failure cancels only observation,
  never durable work, and joins the heartbeat writer before returning.
- During a server restart (#1172) the draining server answers new HTTP/MCP
  requests with HTTP 503 and `Retry-After: 1`, and ends in-flight `wait` and
  `promote_status` long polls with JSON-RPC error `-32001` (`server
  restarting`). Neither is a run or promotion result. Agents calling `/mcp`
  directly may re-issue only idempotent reads (`wait`, `promote_status`,
  `status`, `logs`, `run_get`, `run_logs`, `issue_get` and the other list/get
  tools) with the same arguments, honouring `Retry-After` and their own
  deadline; `oberth mcp call` does this itself (docs/restart-handover.md).
  Mutations (`promote`, `sync`, `run_cancel`, `run_rerun`, `issue_create`, …)
  must never be re-issued on 503, `-32001` or a lost reply: the request may
  already have committed. Inspect durable state instead (`run_get`,
  `promote_status`, `issue_get`, `promotion_list`) and act on what it records.
- Every MCP tool advertises an object `outputSchema`. Structured tool content,
  when present, is always an object; list fields are arrays including empty
  lists. `promote_grant_list` returns `{"grants":[]}`, and successful
  `issue_delete` returns `{"id":123,"deleted":true}`. The HTTP `/api/grants`
  list remains an array. Text retains existing log/diagnostic views; otherwise
  it encodes the same result object. Authenticated tool refusal text keeps its
  stable error class and may append only an explicitly public service reason.
  Raw dependency chains stay masked; scoped missing/hidden equivalence remains.
<!-- MCP count generated by internal/api/readme_tools_test.go -->
- MCP exposes 92 tools: audited administrator operations `uplink_add`, `uplink_scope`, `upstream_add`, `upstream_list`, `upstream_remove`, `upstream_provide_key`, `repo_add`, `repo_verify`, `admin_access_list`, `admin_public_identity`, `admin_readiness`, `admin_version`, `admin_idle`, `admin_audit_record`; `status`, bounded named-step `logs`, exact-run
  `run_get`/`run_logs`, audited `run_rerun` (#1098), audited `run_cancel`/`run_purge`,
  assisted-continuation `run_hold_list`, audited `run_hold_exec`, `run_hold_patch`, `run_continue` and `run_hold_abandon` (any uplink whose scope covers the run's repository, or an admin; see [assisted continuation](docs/assisted-continuation.md)), the read-only agent loop `run_wait`, `events_subscribe`, `events_since`, `events_wait` and `sync_status` (see [agent events](docs/agent-events.md)), `artifacts`/`artifact_get`, `wait`, `sync`, `promote`,
  `promotion_list`, `promote_status`, audited `promote_retry` (#1097), `publish_retry`, `publication_abandon`, issue
  `issue_create`, `issue_get`, `issue_get_many`, `issue_update`, `issue_append`,
  `issue_close`, `issue_reopen`, `issue_delete`, `issue_list`, `issue_lock`,
  `issue_unlock`, `issue_close_many`,
  secret-access `access_list`/`access_allow`/`access_revoke`, unscoped-admin-only
  `uplink_invite`, `uplink_cert`, `uplink_list`/`uplink_revoke`/`uplink_rotate`,
  certificate-authenticated `client_renew`,
  `access_dismiss`,
  admin-only `workflow_capability_grant` and
  `workflow_capability_revoke`, read-only `workflow_capability_get`, admin-only
  `release_wif_list`, `release_wif_grant`, `release_wif_revoke` and `release_wif_reset` (#1497), `repo_list`, `repo_remove`, `repo_archive`, `repo_unarchive`, unscoped-admin-only `branch_delete_many`, `branch_deletion_list` and `receive_outbox` (#1490), `branch_deletion_resolve` (#1500), `ref_delete_many` (#1504), `run_list`,
  admin-only `audit_list` (#1104; verified entries, never available to a scoped uplink;
  `GET /api/audit` shares its filters, response and restrictions),
  `schedule_list` (also `GET /api/schedules`, with the same repository scope) plus admin-only audited `schedule_pause`, `schedule_resume` and `schedule_trigger` (#1106; pause state is the audit chain, trigger goes through `EnqueueCI`),
  `system_status`, admin-only `secretstore_plan` (#611),
  `secretstore_sync_receipt` (#712), `secretstore_verify`, and
  admin-only `promote_grant`, `promote_grant_revoke`, plus read-only
  `promote_grant_list` (#826). The tools listing shows a non-admin token only the tools it may call; a call to an admin-only tool is still refused by the server. The `promote_grant` tool's maximum lifetime is
  configurable via the `--promotion-grant-max-lifetime` flag (chart value
  `promotion.grantMaxLifetime`, default `24h`, hard ceiling `168h` / 7 days).
  The installer pins this value explicitly so `helm upgrade --reuse-values`
  carries the default.
- Park refs default to `refs/heads/park/`; accepted park pushes mirror upstream
  without a branch run. Fresh upstream `.oberth/promotion.yaml` configures the
  namespace, non-admin opt-in and `require_branch_ci` targets. Admins or exact
  promotion grants may also authorize parked sources. Parked promotions always
  run full merged-tree CI, including fast-forwards and unborn targets, and
  retain immutable `BranchCISkipped` plus admission audit evidence (schema 54).
  The existing reason line says `promoted without branch CI`; no status changes.
  Target branch-CI requirements survive admin/grant authority and are rechecked
  before planning and publication. See [park promotion](docs/park-promotion.md).
  Push policy reads locally reuse the successful repository refresh's private
  upstream snapshot for two seconds, with a two-second deadline and no second
  upstream transport. Initial cache fills also supply this snapshot; a proven
  absent policy uses defaults. Failed refreshes invalidate prior authority.
  Missing/expired snapshots and invalid/unreadable policies fall back to branch
  CI with the cause in the existing run reason and receive audit, including
  after queue-cap drain. Accepted receive replay preserves its original decision.
  Promotion policy reads stay fresh and fail closed. Single-source and train
  admission reject red branch evidence
  and share per-source park eligibility. Parked sources require `repo`.
  Receive-event ledger outcome `parked` records a mirrored park push with no
  branch run; it is separate from the unchanged run status vocabulary.
- MCP `run_get` and `run_logs` accept the same full IDs and unique prefixes
  as the dashboard run views. ID prefix resolution is read-only; scheduler
  and mutation lookups still require exact durable IDs. `run_rerun` and
  `run_cancel` resolve a prefix read-only within the caller's scope first and
  mutate only by the resolved exact ID. `run_cancel` takes `id`, optional
  `reason` (printable, one line, at most 512 bytes) and `force`, and returns
  `run_id`, `status`, `phase`, `previous_state` (`queued`, `waiting`,
  `running`), `cancelled_by`, `reason`, `workflow` (`none`,
  `not_submitted`, `deleted`, `pending`), and optional `promotion_id` and
  `warning`. `run_rerun` resolves the same way and is described under
  Behavioral guarantees. `run_logs` and `logs`
  authorize the named step against the plan/progress/results before reading
  the bounded redacted retained log. While a run is active, absent retained
  bytes yield the typed `ErrStepLogPending` error (HTTP 409; actionable MCP
  text with `isError: true`), including the run and step and a retry instruction.
  Available partial bytes keep the existing filtering and bounds. Unknown
  steps stay not-found; terminal missing logs remain faults, not pending.
- MCP `run_logs` and CLI `oberth log` default to the last 200 lines only when
  no pattern, limit, tail, offset or byte offset is supplied. A `window` field
  explains this in JSON; MCP text and the CLI stderr summary explain it too.
  Explicit filters retain their behavior, including zero/false/empty values.
  Direct HTTP logs, `logs` and artifact reads keep their defaults.
- JSON `/api/*` and MCP responses of at least 1 KiB negotiate gzip via
  `Accept-Encoding` and set `Vary: Accept-Encoding`. SSE, raw downloads and
  already encoded bodies remain uncompressed.
- `wait` matches annotated releases by either the immutable tag object SHA or
  the peeled commit recorded in `TestedSHA`. Trigger aliases `release`/`tag`
  and `ci`/`branch` are normalized before filtering candidate runs; selection
  uses the latest run within that trigger, including on notification and
  timeout. Distinct matching SHA identities and repositories remain ambiguous.
  A selected terminal release returns `still_running: false`, even with a
  newer branch or promotion for its commit. Before a matching release exists,
  a known branch may remain the timeout view with `still_running: true`.
  Ordinary status/sync/promotion SHA resolution does not gain TestedSHA aliases.
- A `status` branch selector resolves the current local Oberth Git ref, then
  the newest run for that exact commit SHA across all source/promotion refs.
  Historical runs named after the branch cannot override its current HEAD.
  When that HEAD has no run, status returns its repository, branch and SHA
  with `no-runs`, even if the branch has older runs. Exact/abbreviated SHA
  precedence, explicit tag selectors and repository/ref ambiguity remain
  unchanged; unknown selectors keep not-found. This read-only selection does
  not fetch upstream or alter sync, promotion or trigger-filtered wait rules.
  This is the complete tool surface. Issues serve the CI repair queue and
  workspace coordination. Manual issues remain in Oberth, either workspace-global
  or scoped to an upstream; no external-tracker cutover is required. See
  [issue scope](docs/ci-issue-scope.md). `issue_create` takes a `title`, a `body`
  and an optional `upstream`; without one it creates a global manual issue,
  except for an uplink scoped to exactly one upstream, which files there (#073). Run selectors are resolved across the repositories the uplink
  may see without a repository input. `issue_list` accepts optional `repo`,
  `kind` (`manual` or `ci`), `state` (`open`, `closed`, or `all`), `query`
  (case-insensitive substring in title, body or branch), `limit`
  (1..50, default 50), and exclusive `before` issue-ID cursor. Omitted filters
  retain the all-issues default. Filters apply before pagination; records are
  ordered by descending creation ID, not update time. Follow `next_before`
  with the same filters until absent for a complete scan. Repository filtering
  uses stored association: global manual issues have no repository, regardless
  of component names in their titles. MCP `issue_list` and `/api/issues`
  return `total` for all matching issues regardless of the page cursor, plus
  `counts` for open/closed (retaining the kind filter) and manual/ci
  (retaining the state filter). Repository, query and uplink scope constrain
  every count; scoped uplinks never receive installation-wide totals (#1124).
- Issue `work_status` is `todo` (the default), `active`, `waiting` or `review`,
  independently of `state`. `issue_update` sets it through the existing scope,
  lock, audit and expected-revision checks; real status changes advance revision.
  `oberth issues update --id ID --work-status active` uses that same MCP path.
  `issue_list`, `/api/issues` and `oberth issues --work-status active` filter
  status before pagination, totals and every state/kind facet. CI projection,
  repeat-red updates and eligible-green closure retain their existing behavior.
- Issue reads return computed `stale`: an open issue whose last `updated_at`
  is older than the applicable window. Closed issues are never stale. Defaults
  are seven days, or one hour for `active`, configured with server flags
  `--issue-stale-after` and `--issue-active-stale-after`. `issue_get`,
  `issue_get_many`, `issue_list` and the matching HTTP views accept positive
  duration overrides `stale_after` and `active_stale_after`. The active window
  cannot exceed the general window; a shorter general override caps an inherited
  active window. The CLI list uses matching hyphenated flags. Observations never
  write issue timestamps, revisions, audit entries or locks. `fields` can select
  `work_status` and `stale`; the dashboard shows both. See
  [issue lifecycle](docs/issue-lifecycle.md).
- `issue_append` takes `id` and `text` and appends bounded text with a dated
  separator to the existing body under the issue lock rules. It does not
  replace the body or create a discussion thread. Its compact response is
  `id`, `state`, `updated_at`, `occurrences` and `appended_bytes` (UTF-8 bytes
  of text, excluding the separator); read content with `issue_get`.
- `issue_close` takes a single `id`, preserves its history, and enforces the
  same issue lock rules. Its compact response has the same fields as
  `issue_append`, with `appended_bytes: 0`. Unlike admin-only
  `issue_close_many`, it does not take a reason parameter.
- `issue_get_many` accepts 1..50 unique positive `ids`, validated before reads.
  Its `results` preserve request order, each containing `id` and either a full
  `issue` (the existing `issue_get` wire record) or `error`: `not_found` or
  `response_limit`. Encoded structured JSON is bounded to 256 KiB, reserving
  a status row for every ID. Bodies are never truncated; later smaller records
  can still fit after an oversized record. Use `issue_get` for any
  `response_limit` ID. Unexpected storage errors fail the tool rather than
  masquerading as missing records. Reads neither acquire nor renew locks.
  Existing MCP encoding duplicates the JSON as text and structured content;
  the encoded tool result is bounded by three times this payload budget plus
  128 bytes of envelope overhead (excluding the JSON-RPC request ID).
- `publication_abandon` requires an unscoped admin uplink, a full publication
  `id`, and a printable `reason` (at most 512 bytes). It shares the live
  scheduler's delivery permit, then atomically records `publication.abandon`
  as the acting administrator, marks the intent `abandoned`, its run
  `failed/abandoned` with the actor and reason, and its promotion failed.
  It never pushes or reads the remote; a previous ambiguous push may already
  have reached upstream. An in-flight delivery settles first; terminal intents
  are refused. An abandoned intent is immutable and cannot be reactivated by
  publication retry or restart recovery. Use
  `oberth publication abandon <publication-id> --reason "stop delivery"`.
  Find the publication ID in attempt logs or in `audit_list` with
  `action: "publication.pending"`, matching the entry details to the run ID.
- Idle publication backoff ticks preserve the queued retry without re-evaluating
  trusted policy or logging an attempt/outcome pair. Due attempts still check
  current policy and record their attempt/outcome evidence.
- `publish_retry` retries the upstream publication for a branch or tag run
  whose burns all passed but whose upstream push failed (#696). Input is
  exactly one durable run ID — never a ref, SHA, or tag name.
  Preconditions (all fail closed with `ErrInvalidInput`): the run is
  terminal; every burn/step passed; the failure is recorded in the publishing
  phase (`run.phase = "publishing"`) or as an exhausted upstream publication
  infrastructure failure; the recorded SHA still resolves in
  Oberth's git cache (re-read, not trusted from the row alone). For tag runs
  the exact tag object SHA recorded at admission is what gets pushed; for
  branch runs the exact SHA (force-sync semantics identical to the original
  publish). A promotion ID, or the ID of a promotion's CI run, is refused
  with `ErrInvalidInput` naming `promote_retry` (and the head the target moved
  to, when it moved) and changes nothing: a promotion row is immutable once
  terminal and its merge was made against a head that may have moved, so a
  promotion gets a new retry row (#1097). After a transient push-only failure,
  an unchanged target permits reuse of the exact tested merge SHA; a moved
  target requires a fresh merge and tests. `Store.RetryFailedPublication`
  refuses a promotion-owned publication the same way, before any state
  change. Zero repository code executes. Idempotent: if upstream already has
  the object, succeeds without pushing. Rate-limited by the delivery permit (one
  concurrent publication delivery per scheduler). An audit-chain entry is
  recorded BEFORE the push attempt. On success the run transitions to
  `passed`/`phase=passed` and the CI issue closes; on failure the error is
  stored verbatim (minus anything secret) and the run stays retryable.
  If finalization is uncertain and recovery still owns the publication, the
  response is `pending`, plainly saying publication is queued, with
  run/publication IDs; poll `run_get` or `wait` for
  the run. Only a durable terminal publication row justifies a terminal
  response. Delivery attempts and outcomes log both IDs (#1189).
  Retry acknowledgements include `action`, `message`, `run_status` and
  `run_phase`. `action=publication_retry` identifies retry of the existing run;
  `status` keeps its legacy publication outcome (`pending`/`delivered`/`failed`).
  Publication and owning run are observed in one committed WAL snapshot even
  after a successful delivery reply; a stale or lost delivery reply cannot
  override durable failure/completion. `run_status`/`run_phase` match `run_get`
  at that observation. When status cannot be read, report `unknown` rather
  than guessing `pending` or claiming a terminal outcome. Notifications begin
  at durable reactivation. No-work refusals say `nothing started`; errors after
  admission identify the admitted work, and an ambiguous admission is explicitly
  unconfirmed. Publication ownership remains with the outbox during backoff.

  Transient upstream server/transport failures retain the exact durable intent.
  Five attempts are allowed, with backoff of 2, 4, 8 and 16 minutes (about
  thirty minutes total, excluding I/O). Schema v38 persists the attempt count
  before external mutation, next-attempt time and owner progress message;
  canonical audit intents bind the retry state. Waiting holds no background task or
  delivery permit. Dispatch ticks and owner-startup recovery resume due work.
  A remote ref read precedes every mutation decision. After a lost
  finalization reply, recovery can confirm an already-delivered result during
  backoff without another push.
  Promotions/default targets use ordinary non-forced pushes; tags retain their
  admitted object identity. A hook refusal alone, permission/policy denial or
  non-fast-forward remains a permanent failure. Exhaustion marks the run
  failed/infrastructure, with an upstream-publication reason. `run_rerun` routes
  these failures to publication retry without repeating CI or release work. Explicit publication retry resets the bounded budget.
  Publishing-phase failures get their own CI issue title
  (`Publish red: <branch> — upstream publish failed`) so automation and humans
  can distinguish code red from forge red.
  **Audit-gate invariant** (v0.16.19+): `RetryFailedPublication` chains an
  informational `publication.retry` action followed by a canonical
  `publication.pending` action as the LATEST entry with standard
  `publicationAuditDetails`. The mutation gate
  (`verifyPendingPublicationSnapshot`) requires every pending publication's
  latest chained action to be `publication.pending` or
  `publication.predecessor` — `publication.retry` alone is not gate-valid.
  The service layer verifies the mutation gate before calling
  `RetryFailedPublication`; a gate failure leaves the run in
  `failed/publishing` (retryable) with a typed error.
  **Startup reconciliation** (v0.16.19+): `ReconcileInconsistentPublications`
  runs at store open, after `recoverInterruptedRuns` and before the first gate
  evaluation. For each pending publication whose latest chained audit action is
  not gate-valid, it appends a canonical `publication.pending` action through
  the normal chained-append path (hash chain stays gap-free). Publications with
  no actor are transitioned to failed. Idempotent: publications already
  gate-valid are untouched, and zero audit actions are appended on a consistent
  store.
  **Readiness**: the readiness probe continues to verify full audit state
  (chain + pending publication intents). The root cause of the v0.16.18 outage
  (retried publication with non-canonical latest action) is eliminated by
  construction (retry chains canonical pending) and by startup reconciliation
  (heals pre-existing inconsistent state before readiness is first evaluated).
  Reads do not degrade independently of mutations because audit integrity is a
  precondition for correct attribution on read paths too.
- `issue_reopen` accepts exactly one positive `id` and returns the same issue
  record as `issue_get`. Only manual issues may reopen; CI state remains owned
  by run projection. Reopening preserves ID, content, creation time and occurrence
  count, clears `closed_at`, and records `issue.reopen` with the acting uplink
  in the same transaction as the state change. The existing mutation gate and
  active-lock ownership check apply, including on already-open requests. A
  successful transition renews the caller's active lock; an already-open retry
  changes neither issue timestamps nor audit history. No additional Kubernetes
  permissions or chart configuration are required.

### Release images

- This repository's Release burns publish the server image as a multi-arch
  OCI index with exactly two children, `linux/amd64` and `linux/arm64`, each
  a single flattened layer binding the exact source-built executable for its
  platform. The published reference is always the index digest; the Helm
  chart pins the index digest, never a single-platform manifest.
- The server substrate is a reviewed, digest-pinned scratch index containing
  static Git built from a checksum-pinned source, static Oberth, its native
  SSH alias, and CA data. Repack replaces only the server executable and
  refuses extra runtime files or a changed SSH alias at assembly/readback.
  Local runtime qualification runs the complete CLI cache suite, host-key
  mismatch refusal and cancellation/reaping checks without a shell/OpenSSH.
- Releases also publish an optional helper index for `linux/amd64` and
  `linux/arm64`. Each child has one flattened layer containing only the exact
  static Oberth release binary and the CA bundle, with no Git executable,
  SSH alias or static-Git component labels. Steps select the helper digest
  from signed `release.json`; the five-file binary/chart checksum contract
  stays unchanged.
- There is no runner image artifact: Job substrates are public standard
  golang images selected per repository under the administrator prefix
  allowlist.

### Runner result

- The binding step results are a JSON array of step objects with `burn`,
  `step`, `status`, `exit_code`, `started_at`, and `finished_at` fields,
  emitted as one final `[runner/summary] oberth-summary <array>` marker line
  on the run's log stream after every burn has finished. The server reads the
  final line-start match from the verified authoritative Pod log; only the
  runner can start a physical log line, so subprocess output can never
  outrank the genuine record. Decoding is strict and fail-closed: an invalid
  or empty record fails the run, and a green run without a record is an
  error, never a silent pass.
- The Kubernetes termination message carries only a minimal JSON status
  object (`version`, `trigger`, `status`, optional `error`, timestamps,
  bounded to the platform's 4096-byte limit) for `kubectl describe`
  diagnostics. During transition the server still decodes a legacy
  array-shaped termination message from older runner images; the summary
  step array is therefore no longer budgeted at pipeline admission.
- Step status is one of `passed`, `failed`, `skipped`, or `timed_out`. These
  are the only values a *durable* step result can carry: `PutStepResult`
  refuses anything else and the `step_results` CHECK constraint does not admit
  it, so a half-finished run can never be recorded as a result.

### Planned steps

- A run's step list describes the pipeline it was admitted with, not only the
  part of it that has already executed. Engines that can enumerate their own
  pipeline format statically (`service.PipelinePlanner`; the Argo engine via
  `argoworkflow.PlannedSteps`) record the declared burn/step inventory once,
  immediately after the pipeline object is submitted and before its first Pod
  exists. The record lives beside the run's log and progress journal, never in
  the durable results table.
- MCP `status` (including ref fallback and `wait`) adds optional `scheduling`;
  MCP `run_get` and `GET /api/runs/{run}` add optional `Scheduling`, preserving
  the detail response's existing capitalized fields. It appears only for active
  runs with no running or terminal step results. This ephemeral observation is
  not persisted and never changes run state or constitutes execution evidence.
  `observed_at` and `age_seconds` describe snapshot freshness (including time
  spent in MCP long-poll); underlying controller state can lag reality.
- Scheduling reasons come from exact source-seed or Workflow/Pod observations:
  native Argo parallelism, locks and suspension, and curated Kubernetes Pending
  reasons. An observation is not an ETA or a resource reservation.

- `system_status`, `GET /api/status` and `oberth status` report pipeline
  NetworkPolicy configuration in the Kubernetes diagnostic (`cluster_detail`).
  The chart passes `networkPolicy.enabled` as `--pipeline-network-policy-enabled`:
  false produces a persistent warning; true reports configured enabled, with CNI
  enforcement unmeasured. An omitted flag reports configuration unavailable.
  This installation-wide diagnostic is also visible to scoped uplinks; it
  contains no repository or namespace details and never gates readiness or admission.
- Controller identity verification can block new dispatch. Status exposes its
  bounded reason and remedy; it is independent of resource quantities.

- Run list views (#1122): `/api/runs`, MCP `run_list`, `oberth runs` and the
  dashboard use one default order in three groups, top to bottom. The queue
  first: every not-yet-admitted run, capped at 500, in scheduler claim order:
  highest effective class first, then earliest `admission_sequence`, then
  `queue_sequence`, preserving restart FIFO order. High work, including aged
  normal runs, precedes normal work in every view.
  Then running runs, capped at 500, most recently started first. Then
  terminal runs, most recently finished first (`COALESCE(finished_at,
  started_at, queued_at)` descending, ties by ID). Only terminal rows count
  against the page limit, and only they page, by that fixed (end time, ID)
  key with the last run's sequence or ID as the external cursor; no limit or
  page lists a queued or running run below a finished one, and a run that
  finishes between page loads cannot land on an older page. `latest_per_ref=1`
  groups each ref's newest run the same way. The whole queue, including claimed
  but unadmitted runs, has one numbering computed before repository/ref filters.
  Each queued run carries its scheduling reason; filtered lists keep global positions;
  scoped uplinks see no global position (#073). `started_at` is recorded when
  workflow admission is granted.
  Workflow submission preserves the admission time. `view=history` retains its
  terminal-only sequence paging. Repository/ref filters and uplink scope apply
  to both views. `run_list.status` accepts one existing display status or a non-empty
  list (REST: repeated/comma-separated `status` query values). Filter before the
  page bound using the same display classifier as presentation; preserve queue
  positions, cursor formats, and latest-per-ref head selection. Never resurrect
  an older matching attempt when the latest head does not match the status.
  `run_logs {id, step}` infers a unique burn across recorded/active steps;
  ambiguity names candidate burns instead of choosing the failed burn. Both
  omitted still use the recorded failure. Promotion selectors accept unique
  8+ character hex prefixes within the caller's scope and pin the full ID for
  subsequent reads; ambiguous prefixes use the existing run ambiguity error.
  Detailed status/run reads can also expose an ephemeral Argo deadline and
  finalization-lag observation after steps have progressed. The clock is the
  submitted budget plus Argo's Workflow start, not workflow admission. Only
  explicit controller/current-attempt Pod deadline evidence puts
  `Execution deadline exceeded.` in `display_reason` (the run stays `running`
  until the store finalizes it, then `failed`); elapsed time or exit 143
  alone does not. Durable terminal state, admission release and replay
  evidence are unchanged. See [workflow-deadlines.md](docs/workflow-deadlines.md).
  Durable `Status` values are unchanged and `RunStatus.Valid` still refuses
  the read-only `waiting` projection.

  `display_status` is exactly one of five values: `queued`, `running`,
  `passed`, `failed`, `cancelled`
  (`model.DisplayStatuses`; `model.DisplayRunStatus` is the only mapping,
  and `TestEveryRawPhaseMapsToExactlyOneDisplayStatus` holds every raw
  status, phase and projection to it). A status says what to do with the
  run; anything finer is the reason line, never a second status. Every run
  that has not started is `queued`: durable queued, or claimed before
  admission (`Status` `waiting` on the wire, phase `waiting` or
  `preparing`). Admission records `started_at`, so an admitted run,
  is `running`.
  The dashboard reverses only the queued block: last-to-start at the top,
  next-in-line directly above running rows, also in the queued filter. API/MCP
  retain claim order. A queued run's display reason reports native Argo parallelism, lock or
  suspension state, or a curated Kubernetes Pending reason. Source-seed waits
  are observable before Workflow creation. No aggregate request figure,
  priority class, pressure blocker or resource reservation is projected.

  | Durable cause | `display_status` | `display_reason` |
  | --- | --- | --- |
  | queued, claimed before admission | `queued` | what the run waits for (above) |
  | admitted and started | `running` | empty |
  | uncertain Workflow create (phase `infrastructure-pending`) | `running` | `Workflow create outcome uncertain; cleanup pending` |
  | running past its execution deadline | `running` | `Execution deadline exceeded.` |
  | failed by its execution deadline | `failed` | `Execution deadline exceeded.` or the retained resource guidance |
  | passed CI / published release | `passed` | empty |
  | Argo Failed/Error or infrastructure failure | `failed` | failed phase and bounded cause; never empty |
  | upstream publication failure | `failed` | upstream rejection class, or upstream publication failure with transient cause |
  | queued publication retry | `running` | publication attempt/backoff message; from publication state |
  | Workflow confirmed absent after a server restart, including work never submitted | `failed` | `interrupted by server restart` |
  | active submitted cancellation | `running` | `cancelling by <actor>: <reason>` until Argo finishes |
  | unsuccessful Oberth stop or cancellation before submission | `cancelled` | cancelling actor or retained stop reason |
  | observed Workflow interrupted by server restart, requiring an operator retry | `cancelled` | `server restart, retry needed` |

  Raw status fields retain their values. A newer push leaves older runs active;
  publication checks the current mirror branch head before pushing. Clients
  accept the five display enum values above. The former `waiting`
  dashboard label and the `infra-pending` and `deadline-exceeded`
  `display_status` values are gone: they are `queued`, `running` and
  `failed`/`running` with the reason above. The display pair is additive
  on default API, MCP and CLI responses; retry
  and rerun eligibility still uses the durable cause. The `status`/`wait`
  tool's `status: "delivered"` remains the legacy alias of `passed` for every
  passed run, including releases; `display_status` is always `passed`.
  An interrupted result neither opens nor resolves a CI-red issue.

  `/api/runs/counts` retains `Total` and `ByRepo`, and adds `Queued`, `Running`,
  `QueuedByRepo` and `RunningByRepo`. Queued includes claimed/unadmitted runs;
  running means admitted and non-terminal. Counts respect uplink scope. The
  dashboard header uses these counts; its dated-page footer uses `Total - Queued`
  (or the corresponding per-repository counts). Status chips and the independent
  release toggle compose, so failed releases can be selected. A pruned cursor
  returns 404 with an instruction to restart from the first page.
- Execution scheduling observation has a two-second request budget, one exact
  Workflow read bound to run ID and effective tested SHA, and one namespaced
  Pod list (limit 16) whose entries must have the exact Workflow controller UID.
  Node and Pod summaries are capped at 16 each, with truncation explicit.
  Status uses fixed vocabulary; synchronization waits retain Argo's wording,
  with whitespace collapsed, non-printable runes removed and a 200-rune cap.
  Other reasons use curated summaries:
  arbitrary messages, node/container names, specs, env, commands, logs and raw
  transport errors are withheld. No resource mutation, logs, events, additional
  RBAC or credentials are needed. Observer failure preserves durable status and
  reports unavailable observation. Terminal/progressed runs perform no live
  scheduling reads.
- The projections that serve a run's steps (`/api/runs/{run}`, MCP `status`
  and its `burns` map) merge three records in increasing order of authority —
  plan, progress journal, durable results — and only ever forward, so a
  less-informed source can never walk a step backwards. The step count is
  therefore the planned count for the whole life of the run, including a run
  that was interrupted before recording any result.
- Argo retry attempts share the ordinal of their logical burn/step's first
  attempt. Dropping superseded attempts cannot renumber the final rows or
  move a retried step behind a parallel sibling. A resumed reporter retains
  journaled key/ordinal assignments and assigns newly observed keys only
  unused ordinals. Legacy DAG-attempt burns such as `address(0)/check` are
  ignored by the journal projection only when `address/check` is known from
  the plan or journal and the suffixed name is not explicitly planned.
  Without a known canonical key, the historical row is retained and its
  ordinal remains reserved on resume. Conflicting ordinals for genuine keys
  still fail journal validation; historical log bytes are not rewritten.
- Consequently a step status in an API projection may additionally be
  `pending` (declared, not reached) or the existing in-flight `queued` and
  `running`. `pending` is a projection state only; it is never persisted and
  can never make a run green. A run whose engine cannot enumerate its pipeline
  reports exactly what it always did, and says so in its own log: a plan is a
  visibility record, never a gate.
- The static enumeration and the runtime projection derive burn and step names
  through one shared rule (`argoworkflow.BurnAndStep`), so a document cannot
  plan one set of names and record another. Constructs whose step count is a
  runtime property (`withItems`, `withParam`, `withSequence`) are deliberately
  left out of the plan rather than given a fabricated count.
- Step measurement is absent in v0.22.0: commands and arguments stay authored,
  no measurement executable or mount or architecture selector is injected,
  and per-step usage figures and resource reports are absent. The forward
  migration deletes historical measurements, preserving outcomes, logs, timing
  and bound termination diagnostics. Static YAML resource checks remain.
- Retained output is prefixed by burn and step, redacted before disk for
  streams the oberth credential chain (exec or materialize) wraps, indexed by
  byte range, and served only through bounded reads. Retained logs are bounded
  per step (32 MiB) and per run (64 MiB), enforced at two points of one write
  path: the engine's step-log replay writer (step-attributed) and the
  scheduler's run-log writer. On breach the run fails with an error naming the
  offending step instead of filling the PVC unchecked. Progress-marker write
  failures are surfaced rather than swallowed: a run whose durable progress
  record is degraded is never reported as green.
- Job CPU, memory, and ephemeral storage are bounded. Private scratch uses a size-limited
  `emptyDir`; persistent source state and the split CI/release caches retain the
  fixed PVC and node-path topology.

## Behavioral guarantees

- Branch pushes, including pushes to the discovered default branch, enqueue one
  FIFO CI run for the exact accepted commit. A newer push leaves older runs
  running under Argo's parallelism. Before branch publication, Oberth verifies
  the current mirror head under the Git lock through the push. An older green
  result abandons publication if the branch has moved, even if the newer run
  fails. Submission tracking reserves no capacity.
- A branch deletion records cancellation of that ref's unfinished non-promotion
  runs, with phase `cancelled` and reason `ref deleted`. Work not submitted
  finishes cancelled; submitted Workflows receive native Stop and keep their
  observer until Argo finishes. Deferred receives retain a cancelled reason and
  cannot drain into new runs. Receive replay cannot cancel later recreations,
  including at the same SHA. Tags, promotion CI and pending publication owners
  are untouched.
- An explicit run cancel (`run_cancel`, `oberth run cancel`, `oberth runs
  cancel`) records durable intent and a `run.cancel` audit action atomically,
  including actor, reason, previous state, force and policy warning. Queued work
  finishes `interrupted` with phase `cancelled` and an actor reason. Claimed
  work retains its existing active status and a cancelling reason until native
  Argo completion. No status or label is added.
  Ordinary branch cancellation uses Argo Stop so exit handlers run. Repeating
  an active cancellation uses Terminate and skips exit handlers; release, tag,
  apply and promotion cancellations use Terminate immediately. The first Stop
  response warns about repeat cancellation in both MCP and CLI output.
  `run_cancellations` records native stop obligations with reason `cancelled`
  or `owner_restart`; its terminate flag never decreases. No successor columns
  remain. Ownership and UID checks refuse foreign or replaced Workflows, and a
  stale Stop cannot overwrite Terminate. Failed shutdown requests remain pending
  for recovery. Oberth never deletes Workflows; Argo TTL owns their deletion.
  Accepted Stop keeps token delivery, logs and step observation until Argo's
  final phase. An existing completed Workflow also passes through resumed final
  collection and executor identity retirement. Settlement starts an observer
  only when it acquires its own reservation. An unsuccessful Oberth stop ends
  with the existing interrupted/cancelled verdict and opens no CI issue; native
  Succeeded remains passed even when an author-tolerated step failed; the step
  evidence remains failed. Authored shutdown retains a Workflow-spec reason.
  The MCP request attempts the native shutdown on its request context; a client
  disconnect or timeout leaves the durable obligation for the next pass. A
  terminal run or a run owning pending publication refuses cancellation. A
  promotion cancellation prevents a new publication. Work already published
  stays published.
  Authorization is rechecked inside the cancel transaction. An admin uplink may
  cancel any run it can see, with explicit force for tag/release and apply runs
  and a partial-publication warning. A non-admin uplink may cancel only branch
  pushes within its repository scope; release, tag, plan, apply, promotion and
  scheduled runs are refused. Cancellation racing Workflow creation preserves
  the accepted native shutdown strategy and keeps observation/settlement of
  that exact Workflow until native completion or confirmed absence.
- An explicit rerun (`run_rerun`, `oberth run rerun <id> [--reason]
  [--force]`, #1098) queues a new run with the same repository, ref, SHA,
  trigger, tested and base SHA as one failed or interrupted run, including
  infrastructure failures, failed steps and tag/release runs. Branch sources
  must be the newest run for their ref and still match its head. The checks,
  the enqueue and a
  `run.rerun` audit action (`source_run_id`, actor, reason, force, warning)
  commit in one transaction; the new run's lineage is that action, shown as
  `RerunOf` by `run_get` and `/api/runs/{run}` and as `rerun of` by `oberth
  run`. Because `audit_actions` has no index on `resource_id`, the store reads
  every `run.rerun` link with one scan the first time a run view needs one
  and keeps them in memory, adding each new link after its rerun commits, so
  run views never scan the audit table per call. No schema change is made:
  an index would be a migration numbered after the other in-flight ones (the
  runner skips any version at or below the recorded one). It is an ordinary new run: SHA-keyed `status`, `wait` and `promote`
  select the newest run for the SHA, so they follow it. Refused without any
  change: an active or passed source; promotion CI (`promote_retry`), plan
  and apply runs; a ref that already has an active run; a branch whose
  current Oberth head is no longer the run's SHA (a green branch run
  force-syncs its SHA, so rerunning an older commit would roll the upstream
  branch back); and a tag that already has a pending, delivered or failed
  publication or a passed run. A failed tag publication, of this run or of an
  earlier rerun of the tag, means every release step already passed once:
  the refusal names that run and `publish_retry` for it, so `release.yaml` is
  never executed again for a tag that only failed to publish. A source that passed every step and failed
  only in publication (`phase = "publishing"`, or exhausted upstream publication
  infrastructure failure) is not re-executed: its
  publication is retried exactly as `publish_retry` does (response `mode:
  "publish_retry"`), so a release's publishing work never repeats. A tag
  rerun re-executes `release.yaml` with the original admission (the tag
  object and peeled commit are immutable and were proven reachable when the
  tag was admitted); its tag publication classifies the remote first, so the
  tag object is pushed at most once, and the recorded warning says anything
  the failed Workflow already published stays published. Authorization: a
  non-admin uplink may rerun only `branch`-trigger branch runs in
  repositories its scope can see; an admin may also rerun scheduled runs and,
  with an explicit `force`, tag/release runs. The response carries `mode`
  (`rerun` or `publish_retry`), `run_id`, `rerun_of`, `status`, `ref`, `sha`
  and optional `publication_id`, `warning` and `error`.
  Additive `action` and `message` name the actual work: `rerun` means a new
  run, `publication_retry` means the existing `rerun_of` run's publication.
  `run_status`/`run_phase` carry the current run observation independently of
  legacy `status` (which is publication status in `mode=publish_retry`). The
  CLI prints this action and run state. A recorded rerun remains recorded even
  when its dispatch acknowledgement is lost; such an error never says nothing
  started. These observations can advance after the response as work completes.

- Background scheduler storage deadlines and SQLite BUSY/LOCKED errors keep
  the exact operation pending with fresh 10-second attempt budgets and capped
  exponential backoff (250ms to 5s), until recovery or owner shutdown. They do
  not return a fatal scheduler completion, replay a Workflow create/Wait/Git
  push, or change an already observed run result. Lost commit acknowledgements
  resolve the exact claim/publication identity before retrying.
  Intentional retry backoff is excluded from local dispatch liveness; a DB
  call that ignores its deadline remains detectable as a local wedge. Warnings carry
  `operation`, `retry_pending`, `retries_total` and `retry_in`; unscoped
  `system_status`/`GET /api/status` exposes `database_retries_total` (process
  lifetime, omitted at zero). Client request contexts keep their own deadline
  and ordinary error behavior. Composite startup recovery and Git replay keep
  their caller's operation budget; the 10-second budget applies to storage
  calls, not Kubernetes cleanup or a whole replay. Bare external deadlines do
  not authorize database retries. Trusted startup cleanup logs pending failures
  and continues with its durable obligations intact. Owner shutdown stops
  retries between attempts and leaves in-flight detached cleanup/finalization
  to finish. The create-settlement background task keeps its own bounded attempts/backoff.
  Invalid immutable/admission bindings, missing required recovery capabilities,
  schema/corruption and other non-transient failures keep their fatal behavior;
  a joined invariant error cannot be hidden by a concurrent DB deadline.
- A server stop or restart adopts an existing Argo Workflow for an active
  branch, promotion, tag or release run (#1172). The
  Workflow's run ID, ref, tested SHA, trigger and
  namespace must match the run and configured engine. Its spec identity must
  be present; its UID, identity and live spec remain pinned during observation.
  A name collision or unreadable API stops recovery without deleting or
  re-firing that work. Startup logs and returns the run ID and the exact
  `<namespace>/<name>` of a refused Workflow (binding mismatch, invalid size
  declaration) with the `kubectl delete`
  command for it; once it is deleted, the next start takes the
  missing-Workflow fallback below. The adopted run's grant is restored before
  new claims. A Workflow a v0.19 server submitted with a concurrency-group
  annotation is adopted; the annotation no longer constrains new work.
  Adoption keeps the same run row, `queue_sequence` and `admission_sequence`;
  it does not enqueue a replacement or allocate a new admission order.
  Shutdown leaves a non-terminal Workflow and its run/workspace intact,
  including an active promotion workspace.
  The successor appends to the same run log, restores the progress journal,
  skips already-recorded step transitions/logs, and enforces the remaining
  aggregate log budget. Executor-token delivery resumes only for Pods still
  running the token waiter. A Workflow that finished while the owner was down
  uses the same observation, artifact collection, step persistence and normal
  publication/issue finalization as a newly submitted Workflow.
  Verified adoption distinguishes a missing process exit from the legacy
  synthetic exit-code-1 display fallback. New progress markers omit unknown
  exit codes rather than persisting the synthetic 1. A failed Pod attempt that
  started before this server process initialized, without durable exit evidence,
  becomes `interrupted` / phase `interrupted`, with no failed-step
  attribution only when a complete Pod snapshot confirms `pod-absent`, or an
  exact readable owned Pod lacks a main termination. LIST errors, incomplete
  snapshots and nodes that never got a Pod stay ambiguous/red; the existing
  amber/cancelled presentation and `run_rerun` apply.
  A continuity-gap line alone never overrides a known nonzero node or bound-Pod
  exit, or turns a successful Workflow into an interruption. Ambiguous attempts,
  malformed/foreign or conflicting exit evidence, an independent real test failure, hard cleanup
  failures and an explicit workflow deadline keep their existing verdicts.
  An ownership-bound Pod deletion already in progress when an Argo Error node's
  container receives SIGTERM/SIGKILL is also cut evidence; signal-derived 137/143
  alone, post-failure Pod GC and memory-limit OOM are not. If Wait ends in a log
  or observer error, its result carries an immutable adoption binding into the
  fresh terminal read after the in-process intent is forgotten. Retained exact
  journal failures and real exits already observed for that attempt survive a
  later incomplete snapshot; fallback still accepts a newly reported real failure.
  Only an explicitly terminal earlier Workflow verdict can override a fresh cut;
  a partial running observation cannot fabricate a red Workflow-running result.
  Restart cuts do not publish or create a new CI-red issue.
  Observation retries failed Workflow reads for a bounded grace (2 minutes).
  An adopted run whose observation still fails settles like a submitted one:
  a terminal Workflow keeps its real result and artifacts (also after a log
  budget breach or executor identity retirement error), and an unobservable
  Workflow is asked to stop through Argo's native shutdown (never deleted;
  Argo's TTL owns deletion); the scheduler keeps running. The run stays owned
  for settlement until the Workflow's completion is confirmed, and its result
  is then the Workflow's native verdict from the final observation, exactly
  as for a submitted run.
  A confirmed-absent Workflow, or a claimed run without a Workflow name,
  fails with `interrupted by server restart`. This applies to branch, tag,
  release, and promotion runs. Recovery never automatically requeues or
  re-fires them. Existing Workflows are adopted; ambiguous creation remains
  under bounded observation until adoption or confirmed absence. API read
  errors do not establish absence. An explicit retry or rerun creates a new
  run ID with its own queue and admission sequences.
  Argo tag pipelines require the durable release flag; a historical tag Job
  without that capability never gains release/WIF authority from its ref name.
  Publication ownership is durable and takes precedence over Workflow
  observation: recovery never submits or adopts that run again.
  A passing adopted promotion uses its immutable target/base/result through
  the same publication outbox as a new run. Delivery re-reads the target before
  pushing, refuses a moved main, and recognizes an already-applied result after
  a crash instead of pushing again. Tags publish the original tag object while
  adoption binds the reviewed peeled commit. Tag/release adoption does not
  re-run pod-side publication, rebuild credentials, or select new per-repo/WIF
  policy; it observes the exact admitted Workflow spec. Credential-bearing
  service accounts, projected WIF audiences and environment remain pinned by
  that spec hash. Promotion workspaces are cleaned only after their durable
  run/publication/promotion owners permit terminal cleanup. Explicit cancellation
  uses native Stop or Terminate; a newer push leaves older Workflows running.
  A completed Workflow with an active run is adopted for final collection.
  Terminal run rows are never automatically re-fired.
- Green branch runs force-sync that same branch to the upstream forge. Red
  default-branch and scheduled branch runs create or update one open CI issue
  per repository and branch with the new SHA, a bounded (16 KiB) failure excerpt,
  and full burn-log command hint; green closes it. Ordinary feature-branch runs,
  promotions, and tag/release runs do not register CI issue work. Projection
  never acquires, renews, or deletes issue locks; explicit coordination locks
  remain available. An explicitly cancelled run (or promotion) projects no CI
  issue and leaves an existing one unchanged. Transient kubelet evictions,
  including node memory/storage pressure and executor scratch eviction, also
  leave CI issues unchanged. Scheduler-side infrastructure failures (such as checkout
  failure) still open/update default-branch and
  scheduled-run issues. A container killed at its own memory limit remains a
  test failure; `OOMKilled` alone does not establish node pressure. A periodic
  reconciliation also
  closes a stale red issue when its accepted branch head is merged into the
  upstream default branch and that branch's latest run passed, or when a
  previously published or explicitly
  deleted branch is absent upstream. The closing reason is appended to the
  issue and audit history. Default-branch CI issues close only through a green
  run. When the run recorded a failed burn/step, the excerpt comes only
  from that step's own named log slice (what `logs <sha> <step>` returns): its
  tail, preceded by the newest earlier lines of the same slice matching
  `fatal error|panic|FAIL|Error:` when they scrolled out of the tail window.
  Sibling steps' output never substitutes for it; a failed step with no slice
  yields no excerpt. Only a run without a recorded failed step (a Job-level
  failure) excerpts the combined run-log tail.
- Promotion green-gates the candidate. It reuses candidate CI only when the
  fetched target fast-forwards to that exact candidate. Divergent merges and a
  fetched target that already contains the candidate receive target-tree CI.
  Both Git backends use a conservative tree merge: a path changed on both
  sides is refused with a `rebase onto <target>` hint unless its resulting
  object and mode are identical (including identical deletions). File/directory
  conflicts and ambiguous merge bases are refused; there is no content-merge
  or rename inference. A clean merge has the exact target/source parents and
  its immutable object is pinned in the cache before CI.
  The chosen target is pushed without force; a moved target fails the promotion.
  Such a failure (delivery found the target at a head other than the planned
  base) is retried by the server itself (#1097): after the delivery permit is
  released, a new promotion of the same repository, source SHA and target is
  admitted, merged onto the moved head and re-tested exactly like `promote`,
  at most twice in a row (a deleted target is not retried). The decision
  reads a typed field the delivery sets on its finalization
  (`TargetMovedTo`), never the stored error text. The original uplink's
  authority is re-resolved as it is now: the retry needs an uplink with that
  identity to still exist; a direct promotion keeps direct authority only
  while every uplink with that identity is still an admin; a granted one acts
  as a non-admin and needs a grant that is still active; its current scope
  must still cover the repository, and uplinks sharing the identity but
  disagreeing on scope stop it. Only admission (a store transaction) runs in
  the failing run's background task or request, so `promote_status` reports the retry
  at once; merge preparation and any fast-forward delivery run in the
  background and hold no admission reservation or delivery permit of the
  work that failed. `promote_retry {id}`
  (CLI `oberth promotion retry <id>`) does the same on request for any failed
  or interrupted promotion (push failure, CI or infrastructure failure,
  restart), with `promote`'s authorization. A transient push-only failure may
  reuse its exact tested result when a fresh remote read still finds the
  planned base (or the exact result). The source must still be green; current
  release policy, trusted-test receipts, actor scope and grants are rechecked.
  A moved target follows normal merge-and-test planning. A retry is refused while a
  promotion of the same tuple is pending or delivered, when the promotion was
  already retried, or when the source SHA's newest run is not green. Promotion
  rows stay immutable: the lineage is the `promotion.retry` audit action
  (`retry_of`, `mode` `manual`|`auto`, `auto_attempt`) recorded on the new
  promotion in its admission transaction, and `promote_status` on the failed
  promotion reports the successor as `retried_as`.
  Promotions to one target of one repository are prepared one at a time
  (#1021): a promotion admitted while an earlier one to the same target is
  pending stays pending with no plan or run, and once no earlier one is
  pending it is merged onto the target as that left it, so two promotions
  never race to "upstream moved". The wait is bounded (12 h, then the
  promotion fails). Server shutdown cancels the wait (it does not wait it
  out) and leaves the promotion pending with no plan, which owner-startup
  recovery interrupts like any promotion that never recorded its plan;
  nothing touches the store after the API's shutdown returns. The API shuts
  down after the scheduler's background tasks stop, and refuses background work
  (queued waits, automatic moved-promotion retries) from then on. A plan
  already under way when the bounded shutdown wait times out keeps running
  to its own bounded end and is logged. promote,
  promote_retry and the automatic moved-target retry (#1097) all queue.
  An unborn target (brand-new repository whose upstream lacks the branch —
  confirmed by a successful, empty ls-remote, never inferred from a failed
  fetch) is a fast-forward creation of the tested source; the promotion row
  records the zero OID as its planned base and delivery expects the ref to be
  absent, failing closed if the target appeared concurrently.
- A reachable release tag runs the release burn with the release-only cache
  and store-sourced credentials; an unreachable tag receives no release
  credentials and is not synced. Ordinary tags run CI under its own cache and
  credential policy, as described in [ordinary tag runs](docs/tag-runs.md).
  After every burn is terminal green, Oberth publishes the exact admitted tag
  object—not merely another tag that peels to the same commit—and fails closed
  if the upstream tag appeared first. Run success requires BOTH a Succeeded
  Workflow AND zero terminally failed configured step results. A step with
  retry attempts where the final attempt succeeded is a pass
  (deduplicateRetries keeps only the final attempt). A terminally failed
  configured step—regardless of whether the DAG's depends expressions handle
  it—makes the run red. Enhanced-depends cleanup or verification tasks may
  still execute after a failure but cannot convert the terminal run to green.
  An Argo-Failed Workflow always fails the run regardless of which step rows
  failed, because unreached tasks have no rows to inspect.
- Pipelines execute as Argo Workflows in a namespace that is never the server's.
  Oberth owns the envelope and the repository owns the work: the document
  supplies steps, DAG, images and commands, and Oberth forces namespace,
  ServiceAccount, the source mount, the
  per-repository build cache mount and its `GOMODCACHE`/`GOCACHE`, the
  server-owned run-fact workflow parameters, the active deadline and the TTL. A document's own `serviceAccountName` is never honoured — Vault validates
  identity, not intent, so a branch push that could name the release identity
  would defeat the tier gate before OpenBao ever saw it.
- Current per-step grants select secret identity and delivery. A recognized
  main store consumer must declare every path and hold the corresponding
  grant for its stable template name or wildcard in the repository/upstream.
  Admission and Build share `argojob.GrantedSecretSteps`; only that main
  Container/Script receives store address/role/CA, helper binary, login token
  and secret-root mounts. Oberth injects no store settings into other containers
  or steps; Argo wait retains its native store mirrors. Every container of a
  credentialed step shares its trust; output-read refusals guard Argo reads.
  Run records carry no durable
  credential classification. Retry/rerun reload current grants, and granted
  consumers require healthy access reconciliation. Step-level credential
  evidence remains part of replay safety.
- Admission refuses everything that would reach the node or the cluster around
  the envelope: `podSpecPatch`, `hostNetwork`, `hostAliases`, `hostPath`,
  `nodeSelector`, `affinity`, `hostPort`/`hostIP`, `workflowTemplateRef`,
  Secret or ConfigMap references in `env`/`envFrom`, `ARGO_*` environment names,
  repository-chosen object names, repository-set Pod or container security
  contexts, and any `claimName`. Documents are decoded strictly, and exactly one
  YAML document per file is accepted.
- Kubernetes namespace ResourceQuota and admission policies govern resource
  quantities. Oberth grants only API-kind/action authority for resource templates.

- Synchronization mutex names are server-scoped at Build time to
  `<trigger>/<repo>/<name>`. A CI workflow's mutex `chart-index` becomes
  `ci/repo/chart-index`; a release workflow's becomes `release/repo/chart-index`.
  ConfigMap-backed semaphores and namespace overrides are rejected at admission.
- volumeClaimTemplate names `src` and other server-reserved volume names are
  rejected at admission to prevent collision with the server's own source claim.
  volumeClaimTemplate specs must not declare `dataSource`, `dataSourceRef`,
  `selector`, `finalizers`, or `ownerReferences`.
- Run artifact collection (`$OBERTH_ARTIFACTS`) is CI-tier only: credentialed
  triggers (release, plan, apply) never have their artifact directory collected
  or persisted, preserving the memory-only contract for secret material. Every
  collected member is scanned against the structural secret set
  (`internal/artifacts.DefaultScanPatterns`, PEM private-key headers) during
  the whole-archive judgment pass; one match refuses the entire collection
  fail-closed with the member named and no content echoed.
- Run evidence retention (#1101) runs independently of artifact collection every
  `--retention-sweep-interval` (default `10m`). `--log-retention` and
  `--artifact-retention` are maximum ages since run completion (Go durations,
  default `0s`, disabled). `--logs-budget-bytes` defaults to `0` (disabled);
  `--artifacts-budget-bytes` preserves the existing 4 GiB budget, and
  `--artifacts-limit-bytes` preserves the 256 MiB collection limit.
  `--retention-keep-newest` keeps the newest terminal runs per repository,
  ref kind and ref (default 1). The chart exposes these as `retention.interval`,
  `retention.keepNewestPerRef`, `retention.logs.{maxAge,budgetBytes}` and
  `retention.artifacts.{maxAge,budgetBytes,limitBytes}`. Explicit zero budgets
  and keep counts remain zero on upgrade. Age expiration and exceeding a size
  budget independently select oldest terminal evidence for removal.
- `run_purge {id,what,reason}` / `oberth run purge <id> --what
  logs|artifacts|both --reason <text>` requires an unscoped admin uplink.
  A reason is required (printable, one line, at most 512 bytes). Manual purges
  may override the newest-run policy, but neither manual nor automatic purges
  remove active or release/tag/plan/apply evidence, unfinished cancellation or
  publication evidence, runs bound to promotions or trusted plans/applies,
  pending promotion source/result evidence, open CI issue branch evidence, or
  evidence covered by an active promotion grant. Protected evidence may exceed
  a budget; configure retention to keep sufficient working space.
- Purges retain immutable run/step rows and receipts. A committed
  `run.purge.pending` audit intent precedes file removal; local scheduler
  writers are excluded (including cancelled runs still closing their logs).
  Protection checks repeat under a write transaction during removal, and
  `run.purge` records bytes and outcome.
  Audit records project durable per-run `Purges` markers in `run_get` and the
  dashboard detail, avoiding a numbered schema migration. Reads of purged
  evidence identify the actor and date (HTTP 410); unfinished/failed attempts
  explicitly report unconfirmed completion and can be retried. Logs include
  indexes, progress journals and plans. Installation status (`system_status`
  and `/api/status`) includes retained log/artifact bytes, available bytes,
  the live database size (`database_bytes`, database plus
  WAL) and last sweep/error under `storage`; scoped status omits
  installation-wide storage information. The same sweep keeps Workflow and step
  timing records for the newest 2000 terminal runs and the runs evidence
  retention protects, pruning older ones in bounded passes (`timing_pruned`).
- The administrator-only `/metrics` endpoint exports database/free bytes,
  backup/audit-anchor timestamps and admin-token expiry. An unavailable source
  is omitted, never reported as zero.
- One bearer credential holds at most 64 blocking calls (`wait`,
  `promote_status`, `run_wait`, `events_wait`) at once; a further call is
  refused immediately with a tool error naming the limit. `events_wait` with
  `run_ids` waits for several runs in one call.
- A raw artifact download (`GET /api/runs/{id}/artifacts/{name}`) streams the
  whole stored object, never the bounded MCP text view, with `Content-Length`
  and `X-Oberth-Artifact-SHA256` describing exactly the bytes sent; a body that
  cannot be completed aborts the connection short of `Content-Length`. The CLI
  and `mcp call --output` save paths verify size and digest and rename into
  place only on a match. MCP `artifact_get` refuses binary content inline
  rather than returning a lossy body (#1011).
- Each run mounts one PersistentVolumeClaim: a per-run source claim the server
  creates in the pipeline namespace and fills with the exact pushed revision
  before submission, mounted read-only. A Pod may only mount claims in its own
  namespace, so the server's own workspace volume is never the pipeline's. The
  claim is owned by its Workflow and collected with it; unowned claims past a
  grace window are swept.
- Secret-store SDK construction and every per-fetch clone replace inherited
  request headers with the deliberate `X-Vault-Request: true`, clear ambient
  token/namespace identity, and disable response wrapping. `VAULT_TOKEN`,
  `VAULT_NAMESPACE`, `VAULT_HEADERS` and `VAULT_WRAP_TTL` cannot authorize or
  alter transmitted requests. Seal-status remains unauthenticated; only the
  explicit ServiceAccount login's issued token authenticates read/revoke.
  This isolates request metadata without changing process environment; the SDK
  can still reject malformed environment configuration during construction.
  Constructor/clone failures return fixed errors without reflected SDK input.
- Secret-store fetch auditing spans two layers. Fetch INTENT is recorded
  server-side in the audit chain as a `<trigger>.argo.submit.binding` action
  attributed to the uplink that pushed the ref, with the declared secret paths
  included in the binding details; no audit chain, no attributable actor, or a
  failed intent write means no submission. Fetch OUTCOME is observed through
  three signals: the step exit code (`oberth secretstore exec` exits non-zero
  on any store error), a structured `oberth-secret-fetch` JSON marker emitted
  by `secretstore exec` to stderr (captured in the step log), and — when
  enabled — the OpenBao file audit device. Both `setup-secretstore.sh` copies
  require Python 3 and verify the existing device through semantic CLI JSON
  before mutating the store. Missing, discard-only, malformed or unreadable
  audit configuration refuses setup; the scripts never enable audit devices
  through the API. The store operator configures a writable persistent sink
  (for example `/openbao/data/audit.log`) or collected stdout declaratively;
  this metadata check does not prove collection or retention. Fetched values
  are redacted in-Pod by the oberth
  credential chain (`oberth secretstore exec` or `materialize`), which wraps
  each credentialed step's stdout and stderr with redact.NewWriter.
  This wrapper is the value-based masking boundary. The server does not hold
  pipeline secret values and does not mask arbitrary issue bodies, comments,
  or manually supplied log text. CI issue excerpts inherit masking only from
  the wrapped source stream; callers must keep secrets out of issue text.
  Empty optional values are allowed. Nonempty values shorter than three bytes
  (including after trailing-newline trimming) refuse child execution, avoiding
  both an output-matching oracle and unmasked short credentials. Exact values
  of three bytes or more remain masked; the eight-byte threshold applies only
  to extracted multiline fragments. Artifact collection scans every byte of
  every bounded member for configured patterns and structural private-key
  markers before writing any member; there is no line-length or 64 MiB scan
  cutoff. It refuses a matching collection without altering its contents.
- Every push, sync, promotion, and issue mutation is attributed to the acting
  uplink in the same durable transaction as the state change where applicable.
- The inactive pilot implementation is retired. Schema v46 preserves the
  schema-v14 `vm_suite_executions` and `vm_suite_resources` journals as read-only
  history for export and investigation; no execution path consults them.
  Existing capacity reservations remain intact because retiring code does not
  prove external resource cleanup. Operators with historical reservations must
  verify cleanup before clearing them.
- Admin and installer actions use authenticated APIs and require an unscoped
  admin identity. Every action passes the live audit mutation gate and records
  an intent and terminal outcome. Named public reads never accept paths. The
  sole server exec creates the first admin at installation; its client private
  key remains on the operator machine. No bootstrap token enters a Kubernetes
  Secret. OpenBao exec and restore utility operations remain Kubernetes actions.
- The local gap-free SHA-256 audit chain always runs and is verified at
  startup, on every cycle, and before every mutation. External anchoring is
  opt-in and off by default: `--audit-tsa-url` (RFC 3161 checkpoints) and
  `--audit-rekor-url` (Rekor witnesses with rollback-external continuity) are
  each empty unless configured, and a default install contacts no external
  service. Dependent flags (`--audit-tsa-roots`, `--audit-tsa-ca`,
  `--audit-rekor-ca`, `--accept-witness-chain-reset`) require their URL; the
  chart fails the render on the same combinations.
- Existing SQLite state and its WAL are inspected through a private read-only
  snapshot at the exact current schema, leaving the source database/WAL/index
  byte-exact, and checked against complete external continuity before writable
  open. The live daemon does not migrate older schemas. Fresh genesis requires
  empty immutable intent/completion histories (proved even in local mode) and,
  when the Rekor witness is configured, is witnessed before any listener
  starts.
- A fresh genesis whose host-key-derived witness identity already has published
  Rekor history fails closed while the witness is configured. The one-shot
  `--accept-witness-chain-reset` acknowledgment
  (`auditAnchor.acceptWitnessChainReset`, valid only with
  `auditAnchor.rekorURL`) unblocks exactly that
  state: it must name the exact UUID of the latest published witness, the
  abandonment is logged loudly, and the acknowledgment is recorded permanently
  as audit action 1 of the new chain, whose first witness commits it. The flag
  never overrides existing local audit history, rollback-external ConfigMap
  continuity, or signed checkpoints, and becomes a no-op after the reset
  completes.
- An existing deployment that has never witnessed can adopt a Rekor witness via
  the one-shot `--accept-witness-genesis` acknowledgment
  (`auditAnchor.acceptWitnessGenesis`, valid only with `auditAnchor.rekorURL`,
  mutually exclusive with `acceptWitnessChainReset`). The operator names the
  exact current audit chain head `<auditID>:<sha256hex>` (read via
  `oberth audit head`); startup appends a permanent `witness-genesis.adopted`
  audit action at `baseline+1` and creates the immutable sequence-1 witness
  intent binding it. The first cycle publishes witness 1, committing the
  operator decision and the entire trusted prefix hash. Security invariants:
  (I1) exact acknowledgment of the verified head; (I2) never over existing
  rollback-external evidence; (I3) never when a public Rekor history exists
  for this identity; (I4) inert everywhere else (zero value, fresh DB, genesis
  chain, TSA-anchored history in phase 1); (I5) permanent committed record;
  (I6) no retroactive witness claims; (I7) no verification-logic changes;
  (I8) no degraded adoption (Rekor must be reachable); (I9) crash-safe,
  exactly-once. The flag becomes a no-op after success. The installer guard
  `guardWitnessGenesisRetrofit` probes the head and refuses `--install-rekor`
  on an existing deployment without the explicit acknowledgment.
- The in-pod administrative surface is `upstream add|list|remove`,
  `repo add`, `uplink add|list|remove`, `secretstore verify`, and `run
  cancel` / `runs cancel` with `--database`. Every
  mutation passes the live audit mutation gate and appends an audit action;
  listings are read-only. The private admin socket is `/run/oberth/admin.sock`,
  in a daemon-owned 0700 directory on a dedicated writable runtime volume.
  Linux peer credentials must match the daemon UID; mutation-gate requests
  must name a recognized operation in `X-Oberth-Admin-Operation`.
  An in-pod cancel acts as the administrative actor
  (admin rules, `force` included) and then asks the daemon, through the
  admin socket's body-less `POST /v1/cancellation-pass`, to run its
  cancellation pass; the wake carries no input and mutates nothing itself,
  and without it the obligation runs on the next queue event or restart. Push-time repository discovery auto-maps a new name
  only while exactly one upstream is configured — with several upstreams an
  unmapped push fails closed and `repo add <name> <upstream>` declares the
  mapping explicitly. `upstream remove` deletes only mapping state and fails
  closed when a mapped repository already holds immutable CI history;
  `uplink remove` deletes the uplink and revokes its bearer token in the same
  transaction. `uplink add --admin` mints an admin uplink; the admin flag is
  persisted in sqlite and propagated through bearer-token authentication into
  the MCP Actor. Existing uplinks default to non-admin after migration
  (fail-closed). Only admin uplinks may call `access_allow`,
  `access_revoke`, and `secretstore_verify`; non-admin callers receive a clear `ErrForbidden` error
  before any state read or write.
- `uplink add --scope UPSTREAM[,UPSTREAM]` and `uplink scope --set|--clear`
  limit an uplink to the repositories of named upstreams (#073; schema v22).
  The scope travels with bearer-token and SSH authentication. For a scoped
  Actor every MCP tool and `/api/*` view runs on scope-wrapped dependencies:
  out-of-scope runs, issues, promotions, grants and repositories answer
  exactly like missing ones, lists, counts and pages cover the scope only
  (manual issues count only when filed under an in-scope upstream), queue
  positions, capacity and its use are withheld, run and promotion sequence
  numbers are zeroed and `run_list`/`promotion_list`/`/api/runs` page with a
  record-ID cursor (`next_cursor`, `X-Oberth-Next-Before`; numeric cursors are
  refused), re-adding a key keeps its scope, `system_status` reports only
  in-scope upstreams, and the `secretstore_*` tools are refused even to an
  admin. One deliberate exception (#1094): an admin `issue_update` that
  carries `upstream` runs unscoped, so a scoped admin can move any manual
  issue between global and an upstream, including any title/body change in
  the same call, and the response shows the full issue. A non-admin scoped
  caller can still only move issues it can see, within its scope. SSH fetch
  and push resolve the path's upstream first and refuse anything else with one
  fixed message. A scope that cannot be read fails authentication instead of
  degrading to unscoped.
- Secret-access grants are ConfigMap-driven: the `oberth-secret-access`
  ConfigMap in the server namespace is the declarative source of truth for
  the approval table. The server reconciles it into sqlite at startup and
  from a watch with periodic resync (the resync ticker is hoisted to the
  outer Watch loop so ConfigMap convergence survives a broken watch),
  failing closed to zero grants when the ConfigMap is absent or
  unparseable; `access allow|revoke` (CLI and MCP) mutate the ConfigMap
  with resourceVersion-checked read-modify-write and never write the
  approval table directly — the reconciler is the approval table's only
  writer. Every grant and revocation mutation in sqlite is wrapped in a
  single transaction with an `appendAuditAction` call
  (`secret_access.grant` / `secret_access.revoke`), following the house
  pattern from `RegisterUplink`. CLI `access allow|revoke` pass the live
  daemon's fail-closed audit mutation gate (the same gate sibling admin
  mutations use); no gate reachable means fail closed. Grant attribution
  format: when an authenticated caller triggers UpdateConfigMap, the
  reconciler stamps `<actor>+configmap@rv=N` in `approved_by`/`revoked_by`;
  watcher-driven reconciles keep plain `configmap@rv=N`. Grant is
  duplicate-tolerant via `INSERT ... ON CONFLICT(repo, workflow, step, secret) WHERE
  revoked_at IS NULL DO NOTHING`; a concurrent race returns the existing
  active row without error. Grant step values are `*` (wildcard: all
  templates) or a DNS-1123 label (specific Argo template name, e.g.
  `release-publish-images`); `*` and glob characters (`?`, `[`, `]`) in repo
  or secret are rejected at parse. The
  namespace Role carries collection `watch` on ConfigMaps for the reconciler
  and `update` scoped by `resourceNames` to exactly
  `oberth-secret-access`; unnamed ConfigMap update, patch, or delete stay
  forbidden so audit-anchor continuity ConfigMaps remain unwritable by the
  server's own identity.
- Secret paths in `access_allow` and `access_revoke` are literal identities,
  copied exactly from pipeline declarations or `access_list`; no mount prefix,
  KV `data/` segment, slash cleanup, or short-path expansion is inferred.
  For example, `oberth/data/release/cosign-secret` and
  `release/cosign-secret` are different paths. Revocation selects the exact
  canonical upstream repository, exact workflow file, step and secret tuple; `step="*"` selects only that
  wildcard grant, not its named-step neighbors. A missing tuple is refused
  without mutation. Repository-name canonicalization is unchanged.
- Grant revocation is immediately effective for Oberth admission (the sqlite
  approval table is updated atomically with ConfigMap reconciliation), but the
  Vault credentialed policy retains the exact-path read entry until a policy
  re-sync. Per-step grants (issue #623) scope a secret path to a named Argo
  template: `access_allow repo=X workflow=.oberth/release.yaml step=release-publish-images
  secret=oberth/data/release/gar-image-key` restricts the path to templates
  named `release-publish-images`; `step="*"` is the explicit
  wildcard granting access to templates only within that workflow file. Enforcement is at admission:
  `admitSecretstoreExecPaths` checks each template's `--path` arguments
  against that workflow file's step grants before any Workflow is submitted. A path with only
  named-step grants is refused when consumed by an inline or unnamed template (fail-closed: the name is not stable or
  authoritative).
- **Per-step Vault identities:** every (upstream repository, workflow file,
  named or wildcard step) with a grant gets a dedicated ServiceAccount,
  Kubernetes-auth role, and `oberth-step-<readable>-<hash12>` policy. Names
  hash the complete tuple. Named steps inherit wildcard paths only from the
  same file. Repository identities carry no explicit secret approvals.
  `applyPerStepIdentities` assigns the admitted main's scoped account and
  role. Sidecars, init containers, helpers and ungranted steps receive none
  of its store settings or login token. Plan and sync digests include the
  exact workflow file. The chart creates the scoped accounts from
  `argo.perStepIdentities`.
  **Prune safety (finding 1):** `ProducePerStepIdentities` returns a hard
  error when the authenticated active grant catalog call fails — it never
  returns `nil, nil` on API failure. `secretstore sync` aborts before any
  Vault write when the per-step producer fails, preventing orphan removal
  from running against an empty desired set. Fresh installation skips catalog
  production until the server and its first admin exist. The installer's `PerStepIdentities` in the chart
  args are only set when the per-repo producer AND the per-step producer
  both succeeded (prune-safety: an empty list from a producer failure never
  reaches the chart or the sync).
  **Fail-closed admission:** every granted release main requires its exact
  workflow-file and step identity, including wildcard consumers. A missing
  or empty account is refused with the workflow and step plus the store sync
  and identity installation commands. An empty identity map supplies no authority.
  At controller.Create, each per-step SA assigned to a template is verified
  to exist in the pipeline namespace using the same mechanism as the
  workflow-level `verifyServiceAccount`; a missing per-step SA produces the
  same denial class with the same remediation instructions.
  **Chart SA creation (finding 3):** `argoOberthHelmValues` sets
  `argo.perStepIdentities[i]` from `PerStepIdentityNames(cfg.PerStepIdentities)`
  alongside the existing per-repo and per-repo-CI identity lists. The chart
  creates these ServiceAccounts in the pipeline namespace. The values are
  only set when the per-step producer succeeded (prune-safety).
  **Orphan removal (finding 5):** `secretstore sync` enumerates existing
  `oberth-step-*` policies and removes any that are not in the desired set
  (derived from current step identities). Both the policy and the
  Kubernetes-auth role are deleted; each removal is printed before it is
  performed. Tokens outstanding under a removed role
  expire by TTL (<=30m `token_max_ttl`). This handles revoked grants: the
  per-step identity stays in OpenBao until the next sync removes it.
  **Pending declarations (finding 4):** `access_list --pending` returns
  (repo, workflow file, step, path) declarations observed at Workflow admission that have
  no matching active grant. Declarations are recorded at admission time
  by `ArgoJobs.recordDeclarations` via the `DeclarationRecorder` interface
  (wired to `store.Store.RecordGrantDeclarations` in serve.go)
  (schema migration 18: `grant_declarations` table) and never auto-approve.
  Recording failures are logged and never block admission.
  A wildcard grant (`step="*"`) covers any named-step declaration for the
  same (repo, path). Revoking a grant re-pends its declaration.
  The server's own identity has no Vault policy-write capability, so
  `access_revoke` includes an advisory in its response. To complete the
  revocation at the Vault layer, re-sync policies from the current approval
  table: `oberth secretstore sync` (targeted: re-derives per-repo release and
  CI identities plus the shared credentialed/ci-secrets policies, nothing
  else), or the full `oberth install --install-secretstore --upgrade`, or the
  equivalent `setup-secretstore.sh` with `--force`. All three run under the
  ADMINISTRATOR's own store session (`BAO_TOKEN`/`VAULT_TOKEN` in the
  operator's environment, delivered to the pod over the exec stdin stream) —
  the sync command exists precisely so the server never needs policy-write
  capability; grant-triggered auto-sync and admission-time policy composition
  remain design-rejected. `oberth access allow` returns the mirror-image
  advisory (`grantPolicySyncAdvisory`) naming the same commands. Until
  re-synced, an already-running credentialed step whose token was obtained
  before the revocation can still read the path; new runs are blocked by
  Oberth's own admission gate.
- Fresh installs enable pipeline `networkPolicy.enabled`, including on k3s,
  which provides a NetworkPolicy controller. Reruns preserve the stored choice.
  An explicit or stored disabled policy prints a prominent installer warning;
  `--network-policy=true` restores the chart policy. The operator must supply
  an enforcing CNI; the installer never silently disables the policy based on
  the Kubernetes distribution.
- Chart optional cluster-scoped objects have explicit enabling values:
  `secretstore.createAuthDelegatorBinding` (with `secretstore.enabled`) creates
  the TokenReview ClusterRoleBinding. The control-plane PriorityClass belongs
  only to the server; Argo and OpenBao have no injected class. The external
  usage sampler and its node metrics permissions are absent in v0.22.0.
  The remaining access Roles are namespaced.
- The server-namespace Role grants only ConfigMap access: collection
  create/get/list/watch and update of `oberth-secret-access` by exact name.
  It grants no Pod, `pods/log` or `pods/exec` access. The separate pipeline
  namespace Role grants `pods/exec` create only for source seeding into
  run-scoped volumes. Pipeline secrets arrive through projected identity
  tokens and secretstore exec, not through the exec stream. The optional
  `secretstore.createAuthDelegatorBinding` ClusterRoleBinding lets OpenBao
  validate login tokens through TokenReview without a stored reviewer JWT.
  Authored Pod and container security contexts pass through unchanged. Go still
  refuses AppArmor, SELinux, procMount, sysctls, Windows security options and
  beta AppArmor Pod annotations; Kubernetes enforces the remaining Pod floor.
  The removed `oberth.ci/nonroot-templates` annotation is refused, with no
  preparation container, security rewrite or injected private `/tmp`. Authors
  choose users/groups and declare scratch explicitly. Root and writable-root
  execution are allowed; unsafe contexts reach Kubernetes admission unchanged.
  The executor-token waiter keeps its existing security context.

- Security-backported runner tools are rebuilt from immutable upstream release
  source, identify themselves as Oberth derivatives, and are bound to their
  patched module versions and exact binary digests by the image contract.
- Secret-store setup and verification split by authority. The server binary
  has no code path that accepts a store admin token: store-side configuration
  is `scripts/setup-secretstore.sh`, run where the administrator's own
  bao/vault CLI session holds that authority (the identical bytes are embedded
  and extractable via `oberth secretstore setup --print-script`; the script is
  idempotent, refuses drifted roles/policies without `--force` and
  cross-cluster auth configs without `--force-auth-config`, and never touches
  a token or secret value). The script configures Kubernetes authentication
  and KV read policies. It does not provision Transit. Existing trusted-plan
  mounts and keys remain operator-owned. `oberth secretstore verify` runs in the pod with
  only the pod's ServiceAccount identity, reads the live serve process's
  `--secretstore-*` flags from `/proc/1/cmdline`, exercises the production
  fetch path end to end (login, TokenReview, read policy, TLS), reports key
  counts only, and zeroes every fetched value. Renaming a `--secretstore-*`
  serve flag is a breaking change for this discovery
  (`TestSecretStoreCmdlineMatchesServeParsing` pins it).

- **AI-CONTRACT:** `secretstore_verify` reuses the CLI's real login/read verifier
  with the running server's configured identity and TLS trust. Only admin
  uplinks may invoke it, before any configuration read or network operation.
  Inputs select `paths` (at most 32), `release_tier`, optional exact
  `upstream/org/repo` `repo`, per-repo `tier` (`release` or `ci`), server-tier
  `keys`/`expect`, and an overall `timeout` (default 45, maximum 120 seconds).
  Callers cannot override endpoints, roles, tokens, token files, CA paths, or
  insecure transport settings. Release-tier verification uses the same
  deterministic per-repo ServiceAccount and Vault role as runtime execution.
  TokenRequest JWTs stay in memory; the deadline includes TokenRequest and
  Vault login/read. Every owned token/secret byte buffer is cleared on errors;
  successful reads transfer ownership to the verifier for clearing. Duplicate
  paths are read once. Output contains paths/key counts (optional server-tier
  field names), never values or token/error response bodies; diagnostics are
  capped at 64 KiB. Response `verified: false` also sets MCP `isError: true`;
  exceeding the output bound fails verification rather than claiming a
  truncated success. The tool changes no infrastructure configuration, grants,
  roles, or policies. The Go/Kubernetes SDK's immutable token strings retain
  the existing runtime/GC limitation; dropping references is not byte zeroing.

### Promotion grants (#826)

Non-admin uplinks may promote only when a matching unexpired, unrevoked,
unconsumed promotion grant exists for the exact (repository, SHA, target)
that no pending promotion holds. Admin uplinks retain direct authority; their
promotions record an empty `grant_id` (distinguishable as "direct" in the
audit chain).

The `promotion_grants` table is append-only (delete trigger refuses, update
trigger permits only revocation and consumption transitions). Every creation,
revocation, reservation and consumption appends to the gap-free audit chain.
Grants are single-use against delivery (#1097): a promotion admitted under a
grant holds it (its `grant_id`, audited `promotion_grant.reserve` in the
admission transaction, which also refuses a grant that another pending or
delivered promotion holds), and the grant is consumed (`consumed_by` = that
promotion, audited `promotion_grant.consume`) in the same transaction as the
promotion's delivered publication. A promotion that fails or is interrupted
before delivery releases it, so `promote` or `promote_retry` of the same
tuple can use it again while it is unexpired and unrevoked; at most one
promotion holds it at a time and it backs at most one delivery. A grant held
by a pending promotion may be revoked, which stops later retries; the pending
promotion still completes. A grant an older server consumed at admission
stays consumed.

MCP tools: `promote_grant` (admin, creates a time-bounded grant),
`promote_grant_revoke` (admin, revokes an active grant),
`promote_grant_list` (read-only, optional repo/sha/status filters).

Maximum grant lifetime is a documented knob
(`PromotionGrantMaxLifetime`, default 24 h, hard ceiling 168 h / 7 days).

### secretstore_plan MCP tool (#611)

Admin-only read-only MCP tool `secretstore_plan`: given the approved grants
(`access_list`), compute the per-repo policy/role structure that `oberth
secretstore sync` would apply and report the exact diff (repo, path, policy
name). Returns typed output without secret values. Denied for non-admin uplinks.
The server never receives an admin Bao token and never writes policies.
The plan also inspects each registered repository’s default-branch release
document and reports every release leaf’s effective ServiceAccount, WIF role
and subject, alongside capability gaps. This inspection does not record grant
declarations or alter the policy digest; an unavailable planner is explicit.

### secretstore_sync_receipt MCP tool (#712)

Admin-only mutating MCP tool `secretstore_sync_receipt`: records a
`secretstore.sync` audit action after a successful `oberth secretstore sync`.
The CLI posts a typed receipt (plan digest, per-policy changed status, counts)
through the already-authenticated uplink. The server recomputes the plan digest
from the current approval table and records status `current` when digests
match, `stale` when they differ. It never refuses to record a stale receipt
(the audit record is still valuable evidence). No secret values appear in the
receipt or audit details. Denied for non-admin uplinks. The CLI degrades
gracefully when the server is unreachable: it prints the receipt digest and the
exact tool call to replay.

The `secretstore_plan` tool response includes `digest` (the SHA-256 plan digest)
and `last_materialized` (always present: time, uplink, plan_digest, status from
the latest `secretstore.sync` audit action, or status `never` with empty fields
when no sync has been recorded). The `never` state is rendered explicitly in
both text (`last_materialized: never`) and JSON (`"status":"never"`) so an
operator can distinguish "never synced" from "server predates #712".

Audit action schema: action `secretstore.sync`, resource_type `secretstore`,
resource_id is the plan digest, details JSON carries plan_digest, policies
(map of name to changed boolean), changed_count, total_count, and status.

Repositories configure module access in their own Workflow environment. Oberth
does not provide a module proxy or inject module resolver settings or proxy CA
files.

## Compatibility matrix

| Change | Compatibility |
|---|---|
| Add or remove an MCP tool, input, or required response field | Breaking |
| Change the `SecretStoreSecrets` declaration shape, delivery path, manifest name, or exec payload schema | Breaking |
| Change the `oberth/upstream/` scoping rule, its org/repo identity derivation, or the `<kvMount>/data/upstream/` fetch mapping | Breaking security change |
| Deliver an upstream-scoped secret to a repository outside its declared org/repo scope, or allowlist the upstream subtree | Forbidden |
| Store a secret-store value or value-derived digest in etcd, a Kubernetes object, or on disk | Forbidden |
| Add an optional MCP response field | Review required |
| Reduce an advertised maximum while retaining bounded pagination | Review required |
| Change a NodePort, route, MCP tool name/input, environment variable, mount path, or binding step-result JSON field | Breaking |
| Change the Argo Workflow YAML contract — expected filenames (`.oberth/build.yaml`, `.oberth/release.yaml`), required annotations, admission-set fields, the Workflow submission path, the source mount path, the cache mount path, or the injected environment | Breaking |
| Honour a repository-supplied `serviceAccountName`, namespace, security context, or `claimName` | Forbidden |
| Let a repository annotation change a run's identity or credentials | Forbidden |
| Refuse a document for carrying the retired `oberth.ci/concurrency-group` or `oberth.ci/concurrency-limit` annotation | Breaking |
| Decide a run's admission from its workflow document (size, weight, declared demand, a class annotation), rewrite or refuse native `synchronization`, or add a run status or label for a lock wait | Forbidden |
| Count an assisted run (any exec, patch or continue on a retry hold) as promotion, publication or CI-issue evidence; hold a credentialed, release, tag, promotion, schedule, plan or apply step; let a repository set `oberth.ci/assist-hold` or `podSpecPatch` | Forbidden |
| Change the ConfigMap name `oberth-sync-limits`, the mutex name `oberth-release`, or the trigger that `releases.serialize` applies to | Breaking |
| Charge Pod start-up to a step's declared deadline budget again by default, or report a step whose `main` exited 0 as a deadline failure | Breaking |
| Change a scheduling or placement default (`admission.stepStartupAllowance`, `admission.stallTimeout`, `argo.defaultWorkflowTimeout`, `schedules.*`), or add such a setting without a Helm value | Breaking |
| Let a repository choose the step start-up allowance or write `oberth.ci/step-start-allowance-seconds` | Forbidden |
| Admit a `volumeClaimTemplate` with `dataSource`, `dataSourceRef`, or `selector` | Forbidden |
| Admit a construct that reaches the node (`hostPort`, `hostPath`, `hostNetwork`, `nodeSelector`, `affinity`) or the cluster (`workflowTemplateRef`) | Forbidden |
| Bind an OpenBao role to a wildcard ServiceAccount name or namespace, or grant the CI tier a ServiceAccount token | Forbidden |
| Change the RunnerImage declaration shape or bypass an operator-configured image allowlist | Breaking security change |
| Change token-to-uplink cardinality or accepted Git ref namespaces | Breaking security change |
| Change the accepted repository path grammar, its resolution to the registered identity, or the upstream-name reserved list / disjointness guards | Breaking security change |
| Mount any Secret in a branch Job or share/nest CI and release cache roots | Forbidden |
| Execute or evaluate repository code in the server process | Forbidden |
| Force-push a promotion target | Forbidden |

Any breaking or security-relevant change must update this file, the Helm chart,
the relevant tests, and the operator-facing documentation in the same change.

### Upgrade flags: --values, --set, and pinned-key protection (#704)

`oberth upgrade` accepts `--values`/`-f` (repeatable values file) and `--set`
(repeatable `key=value`) flags, forwarded to `helm upgrade` after the
installer's own pinned arguments. Protected keys are `image.ref`,
the three `compatibility.*` identity tokens.
They cannot be overridden via `--set` or `--values`. Setting a pinned key
(or a parent path like `image`) returns `ErrPinnedKeyOverride` before any cluster mutation. Values files are
parsed and their leaf keys checked for the same conflicts.

`--set` key paths are validated against the chart's default values hierarchy
(`helm show values`); a key that does not exist in the chart is rejected with
`ErrSchemaValidation`. This is a minimal typo gate, not deep schema
validation — Helm's own `values.schema.json` validation still runs at upgrade
time and fails closed on type mismatches or constraint violations.

The `--dry-run` plan renders from the same `upgradeHelmArgs` builder the real
path uses, so the rendered `helm upgrade` command is always truthful.
`oberth upgrade` uses Helm's `--reset-then-reuse-values`: new chart defaults are
merged below stored release configuration, then user values and pinned arguments
apply. Explicit false, zero and empty configured values are not replaced merely
because they are empty. Direct `--reuse-values` upgrades also need template
fallbacks for newly introduced keys: `utilityImage` falls back to the shared
utility pin. Native Git is the only backend. The historical v0.22.3 chart gate proves the
old Transit defaults are refused raw and render after exact installer-key
cleanup plus omission of the empty retired profile defaults. Real Helm upgrade, install and restore tests retain live settings.

Helm >= 3.14 is checked before a real upgrade. The three compatibility identity
values are read from the installed release with `helm get values --all`,
validated as protocol tokens and pinned explicitly with `--set-string`;
new chart defaults and upgrade overrides must never redefine them. Missing or
unreadable identities refuse the upgrade. Preflight renders the target chart
with stored user values (not the old defaults), the same pins and requested
overrides, so target schema errors and unknown keys refuse before either the
Argo controller or Oberth mutation. Temporary stored-value files are private
and removed on all exits; rendered manifests are not printed by this preflight.
Only the retired installer-written secretstore.transit.enabled, .mount and
.key leaves are removed from stored snapshots, with one output note. The actual
preview and apply use the cleaned snapshot with --reset-values; live settings
and operator override precedence remain intact. Unknown user keys are reported,
not automatically pruned. Manual reconciliation
and downgrades require `--reset-values` with a cleaned explicit values file
matching the target schema and preserving existing compatibility identities;
forward-only database migration constraints still apply.

Install re-runs and restore deliberately retain `--reuse-values`, preserving
old chart defaults. Restore overrides only its database compatibility values.
When retired installer keys are present in stored values or installed defaults,
install and restore to a chart without Transit instead apply a cleaned snapshot
of stored USER values with --reset-values. Installed compatibility identities
are preserved inside the initial stored-user snapshot, below operator files and
the restore overlay; restored archive identities win. Other chart defaults never
become operator settings.
Restore to a chart that still declares secretstore.transit retains its values.
The v0.22.3 fixture and real Helm tests assert the exact stored user key set and
cover both target-chart paths.

## Retired execution compatibility

Technology-specific execution and its five MCP tools are removed. Generic
workflow capability grants, resource kinds and extended-resource allowlists
remain. Legacy Workflow declarations fail with a workflow capability migration
message; saved chart values are warned about and ignored. Execution journals
are inert, historical runs remain readable, and shipped migrations are unchanged.
See [workflow capabilities](docs/workflow-capabilities.md#migration-from-retired-execution).

## Schema migrations

Database schema migrations are forward-only. `oberth upgrade` announces
pending migrations before applying; rollback requires a manual database
restore from a pre-upgrade backup of `/data/oberth.sqlite`. The running
server's schema version is reported via `oberth version --schema` (prints
just the integer) and the `/api/status` response (`schema_version`).
The default `oberth version` output (no flags) is a 4-field line consumed by
`.oberth/release.sh` and must not be extended; see the AI-CONTRACT comment
in `cmd/oberth/main.go`.

Each entry below is a shipped migration body recorded on every live database
that has passed through it. Shipped migration bodies are append-only — a
version that is recorded on live databases must never be rewritten.

Integer migrations v1..v53 are frozen. New migrations are independent SQL files
in `internal/store/migrations/`, named `YYYYMMDDHHMMSS-<slug>-<random>.sql`
(UTC, generated by `python3 -I hack/new-migration.py <slug>`). The full filename
without `.sql` is the immutable ID. Run integer migrations first, then pending
timestamp IDs in lexical order; never renumber on merge. Independent migrations
must tolerate either deployment order. Each file carries its own `Purpose` and
`Rollback` contract, so adding a migration does not require a shared table edit.

The single `schema_migrations` ledger retains historical integer receipts and
timestamps and adds a unique `id`. New `version` values are local receipt
ordinals, not migration identity. Numeric schema reporting remains a migration
count for installer/backup compatibility. Startup and backup verification check
individual IDs: an unknown applied ID refuses as newer schema, even when counts
match. Missing receipt ordinals remain corruption; a missing earlier timestamp
ID is pending work and can run after a later one. Historical integer-only
binaries refuse the larger receipt ordinal. Downgrade/restore, v1 continuity,
and v2 backup-and-replace semantics are unchanged. See
[Adding schema migrations](docs/schema-migrations.md) for the contributor workflow.

| Version | Introduced | What changes | Rollback |
|---------|-----------|--------------|----------|
| v1 | v0.1.0 | Initial schema: upstreams, repositories, runs, step_results, promotions, issues, issue_locks, audit_actions, token_credentials, uplinks, run_cancellations, receive_events, pending_token_credentials, publications, ci_issue_work, ci_issue_projections, plus immutability triggers | Fresh reinstall |
| v2 | v0.3.0 | Audit chain bootstrap (Go migration) | Database restore |
| v3 | v0.9.0 | step_results: add declared_size, max_rss_bytes, user_cpu_ns, system_cpu_ns columns | Database restore |
| v4 | v0.10.0 | Trusted plans and applies tables, immutability triggers | Database restore |
| v5 | v0.11.0 | secret_access table for grant tracking | Database restore |
| v6 | v0.12.0 | uplinks: add admin flag | Database restore |
| v7 | v0.13.0 | upstreams: add key_name for per-upstream deploy keys | Database restore |
| v8 | v0.13.5 | Historical run credential flag, dropped by the v0.22.5 forward migration | Database restore |
| v9 | v0.13.10 | schedule_fires table for scheduled entry deduplication | Database restore |
| v10 | v0.13.25 | Ledger-only no-op (org-qualified identity phase 1); body is immutable and must not be rewritten | Database restore |
| v11 | v0.13.26 | Canonical persistence rebuild: compound UNIQUE(upstream_id, name) on repositories, schedule-fire qualification (raw connection-level migration) | Database restore |
| v12 | v0.13.26 | Secret access qualified names: re-keys grant rows by qualified repo identity (raw connection-level migration) | Database restore |
| v13 | v0.14.0 | vm_executions table for single-VM execution tracking | Database restore |
| v14 | v0.15.0 | vm_capacity_slots, vm_suite_executions, vm_suite_resources tables for suite-based VM orchestration | Database restore |
| v15 | v0.15.5 | vm_suite_executions: add attach_json column | Database restore |
| v16 | v0.16.0 | runs: add concurrency_group column with DNS-label grammar constraint | Database restore |
| v17 | v0.16.18 | Publish retry (#696): replace publications_guard_update and ci_issue_projection_monotonic triggers to permit failed-to-pending publication retry and same-sequence outcome transition | Database restore |
| v18 | -- | Grant declarations (#623): grant_declarations table for declared-but-not-granted path tracking; `access_list --pending` query; recorded at Workflow admission | Database restore |
| v19 | -- | Promotion grants (#826): promotion_grants table with append-only guards; promotions gains grant_id column; replaces promotions_guard_update trigger to guard grant_id immutability | Database restore |
| v20 | Unreleased (#278) | vm_executions: execution kind, immutable KVM preflight intent/Workflow UID/receipt; preserve legacy VM lifecycle checks and shared capacity | Database restore |
| v21 | Unreleased (#894) | Nullable bounded step termination diagnostics; old rows remain explicitly unavailable, with no inferred OOM or backfill | Database restore |
| v22 | Unreleased (#073) | Upstream-scoped uplinks: uplink_scopes table (one upstream name per row; none = unscoped); issues gain nullable scope_upstream_id for manual issues | Database restore |
| v23 | Unreleased (#917) | Audited repository-scoped KubeVirt grants keyed by durable repo ID | Database restore |
| v24 | Unreleased (#954) | Partial index for passed runs by repository and tested SHA, used to validate SHA-pinned fragments | Database restore |
| v25 | Unreleased (#1017) | kubevirt_grants: add max_vms, max_cores, max_memory_bytes (per-grant VMI run budget; existing grants get 2 VMs, 6 cores, 12Gi) | Database restore |
| v26 | Unreleased (#1021) | size_grants: audited per-repository maximum admission size (L or XL; no active row means L); `CREATE TABLE IF NOT EXISTS` | Database restore |
| v27 | Unreleased (#1085) | admission_head_waits: when a commit's run first waited at the admission queue head, so bounded backfill keeps its clock across a restart; `CREATE TABLE IF NOT EXISTS` | Database restore |
| v28 | Unreleased (#1092) | Backfilled run admission_sequence retains FIFO order across restart re-fires; queue_sequence remains the history cursor | Database restore |
| v29 | Unreleased (#1102) | Partial index on audit_actions (repository_archive_actions_idx) for the repository archive/unarchive projection | Database restore |
| v30 | Unreleased (#1128) | Versioned immutable run milestones, admission episodes, expanded task identity and bounded step timing | Database restore |
| v31 | Unreleased (schedule entry identity) | runs: immutable schedule_entry, empty for legacy rows; named schedule queue isolation | Database restore |
| v32 | Unreleased (#1190) | schedule_fires: durable successful-fire cursor, pending first-fire catch-up; latest outcome/time retained | Database restore |
| v33 | Unreleased (step resource usage) | Nullable RSS/CPU, container memory peak; ambiguous legacy zero counters become NULL | Database restore |
| v34 | Unreleased (queue priority) | Administrator RC branch patterns and audited per-run priority bumps; no pipeline-controlled priority | Database restore |
| v35 | v0.17.73 candidate | Covering latest-ref index on repository/ref-kind/ref/queue sequence; exact indexed head/count aggregation | Database restore |
| v36 | v0.17.74 / #1216 stage 1 | Server-observed, UID/container-bound sampled step resource usage and bounded repository/time/step indexes; no ingestion API or admission change | Database restore |
| v37 | Unreleased (#1216 measurement) | Guard version-2 container telemetry and add the global observation-age retention index; preserve version-1 observations | Database restore |
| v38 | Upstream publication retry | Persist bounded attempts, retry deadlines and audited owner progress without changing the pushed object or promotion plan | Database restore |
| v39 | Submission priority | Persist expiring identity grants, audited priority requests and supersession protection; existing priority ordering remains compatible | Database restore |
| v40 | v0.17.83 run list order | Covering latest-ref index for grouped run lists; no bearer-expiry or issue schema change | Database restore |
| v41 | Bearer token expiry | Nullable absolute expiry; existing credentials retain their lifetime | Database restore |
| v42 | Issue metadata revision | Positive revision; advances only for real title, state or scope changes | Database restore |
| v43 | Publication abandon (#1320) | Immutable abandoned outbox verdict and audited owner termination | Database restore |
| v44 | Restart timing history | Preserve immutable Workflow and step timing per restart episode while retaining the run ID, queue position and original timing records | Database restore |
| v45 | Tag publication identity | Guard one publication owner per repository, tag and result SHA; preserve existing duplicate history | Database restore |
| v46 | Pilot retirement | Preserve inactive pilot journals as read-only history; remove them from execution decisions | Database restore |
| v47 | Operator capabilities | Empty repository-bound authority for operator ServiceAccounts, resource kinds and extended resources; no conversion of historical grants | Database restore |
| v48 | Deferred execution retirement | Preserve writable execution journals, interlocks and grant authority so owner recovery can settle work in flight | Database restore |
| v49 | #1216 stage 2 | Independent per-repository numeric request/limit ceilings, migrated exactly from size grants with grant and revocation provenance | Database restore |
| v50 | Publication policy availability | Persist the first unavailable policy read independently of publication age; clear on success or terminal status | Database restore |
| v51 | Issue workspace coordination | Add constrained todo/active/waiting/review work status and include real status changes in metadata revisions; staleness remains computed on reads | Database restore |
| v52 | Run list order | Rebuild the v40 covering index idempotently; widen `runs_latest_ref_idx` to cover status, start, finish, queue time and ID, so the grouped latest-per-ref list ranks every head from one covering scan | Database restore |
| v53 | Promotion trains | Persist ordered sources, conflict decisions and merged tree SHA, with retry identity and per-source grant reservations; legacy promotions remain unchanged | Database restore |
| v54 | Park promotion evidence | Add immutable `promotions.branch_ci_skipped`, defaulting to false for historical rows; parked sources must use a tested promotion run | Database restore |
| v55 | Green commit trailers | Persist default-branch green boundaries and atomic issue-trailer processing receipts; seed historical deliveries without replay | Database restore |
| `20261104120123-concurrency-limits-971dc11f4a5530e149fd22d0d6847b46` | YAML concurrency limits | Persist each run's limit and accepted pipeline definitions; seed legacy groups at limit 1 | Database restore |
| `20261104022000-migration-ids` | Migration IDs | Add/backfill unique IDs in the existing ledger, preserving integer receipts and timestamps; future definitions load from separate SQL files | Database restore |
| `20261104130000-queue-cap` | Optional queue cap | Persist queueCap.maxNotStarted (default 0/off) and FIFO deferred branch receives; preserve the frozen integer catalog | Database restore |
| `20261111190000-client-certificates` | Client certificate bindings | Public SPKI pins and certificate-only uplink lifecycle records | Database restore |
| `20261111201000-run-ref-prefix-index` | Ref prefix index | Bounded literal-prefix run lookup | Drop index |
| `20261111200000-canonical-priorities` | Canonical priorities | Rewrite stored priority classes and constrain new values | Database restore |
| `YYYYMMDDHHMMSS-<slug>-<random>` | Future migrations | Per-file `Purpose` and `Rollback` contract in `internal/store/migrations/`; no shared number allocation | Per-file contract (database restore by default) |

Timestamp migration `20261113213557-remove-fragment-run-index-8c4f9bbfa3c091b95cecdb689775370b`: remove the obsolete fragment passed-run lookup index; retain runs and audit history. Rollback restores the pre-upgrade database backup.

Timestamp migration `20261104190000-priority-four-classes`: extend execution-priority vocabulary while preserving legacy receipts; rollback restores the pre-upgrade backup.

**Contract tests:** `TestEveryMigrationDocumentedInAgentContract` checks all
legacy table entries and each timestamped file's contract comments.
`TestMigrationIDsNoNewIntegerMigrations` runs in the required store CI suite and
refuses new integer migrations; catalog validation rejects duplicate or invalid
IDs. Merge tests apply independent IDs in both orders and preserve legacy receipts.

Repository-owned `.oberth/issues.yaml` may opt in with
`close_from_trailers: true`; omission leaves trailer automation disabled.
Delivered green default-branch pushes and promotions process real Git `Fixes:`,
`Closes:` and `Refs:` trailers since the preceding green head. Issue evidence,
closure and the replay receipt commit together through the issue audit path
with actor `oberth` and the run ID. Upstream scope and active locks constrain
every mutation. Permanently invalid policy or bounded history is completed with
an atomic `issue.trailer.skipped` audit; retryable Git failures remain pending.
Recovery batches interleave repositories. No run status or additional authority
is introduced. See
[issue lifecycle](docs/issue-lifecycle.md#commit-trailers-on-green-default-branches)
for policy, evidence, recovery and multi-repository behavior.

### Bounded promotion recovery (#752)

`promotion_list` reads existing append-only admission records without mutation,
Git resolution, publication retry or issue-lock renewal. Exact optional repo,
full lowercase source SHA and current status filters are applied before the
page bound (default 50, max 200). The exclusive admission-sequence cursor
`next_before`/`before` supports older records. Pending admissions remain visible
without a run/result SHA. Typed output carries only public identity, scope,
status and timestamps; it omits internal error text and actor fields. Listing
is not inferred from Git ref equality. The committed promotion row is the
status authority for both `promotion_list` and `promote_status`. Pending page
entries are refreshed before rendering and the requested status filter is
reapplied. Long polls re-read the row at timeout before returning pending.
Both reads remain pending while finalization is unconfirmed, even if the
remote already contains the result, and report passed after atomic recovery.
Terminal rows are immutable; independent reads may observe a real intervening
commit, reflected by `updated_at`/`UpdatedAt`. Reads never finalize a publication.

## Operator-owned external ingress

The chart and installer manage Oberth and its CI dependencies. Operators own
external ingress and connectors separately. The `watchTunnel` value is
removed; an upgrade refuses stored values that still set it. The push banner remains an optional, display-only dashboard URL.

## Keyless GAR publication and verification (#557)

The repository-owned credential helper (`scripts/oberth-release-wif-credential.py`)
serves all three WIF roles via an exact audience allowlist and
administrator-configured first-hop accounts. The helper derives the active
role from the `audience` field of the Oberth-projected ADC at
`/run/oberth-gar/adc.json` and requires `service_account_impersonation_url`
to match the exact Google IAM endpoint and service-account syntax. The first-hop
URL comes directly from the existing server-owned, read-only ADC mount.
Unknown audiences and missing or malformed source URLs refuse before exchange.
Google's WIF and IAM bindings enforce first-hop role permissions; the helper
does not carry an installation-specific audience-to-source-account allowlist.

### Role allowlist

| Role | Pool | Service Account |
|------|------|-----------------|
| `image-writer` | `oberth-release-images` | `<release-publisher>` |
| `image-reader` | `oberth-release-reader` | `<release-reader>` |
| `chart-writer` | `oberth-release-charts` | `<chart-publisher>` |

All pools use provider `<provider-name>` under project `<project-number>`.

Oberth's helper additionally performs the existing matching delegation (#875):
image-writer to `oberth-release-publisher@<project-id>.iam.gserviceaccount.com`,
image-reader to `oberth-release-reader@<project-id>.iam.gserviceaccount.com`, and
chart-writer to `oberth-chart-publisher@<project-id>.iam.gserviceaccount.com`.
The validated audience fixes the final hop. ADC names the configured
first-hop principal; its URL must pass strict syntax validation and cannot
override the Oberth target. Both IAM requests
use 600-second tokens over the same verified TLS/no-proxy/no-redirect transport.
Only the final Oberth token reaches the credential pipe; intermediate tokens
stay in memory, and second-hop refusal never falls back to the first token or
static keys. This consumes existing matching TokenCreator and repository IAM,
without widening either. The helper uses the configured source exchange for
Oberth's repositories.
Real source account emails live only in operator installation configuration.
Before the next release, configure/retain `argo.releaseWIF.roles.<role>.service_account`
for all three roles. The current server already supplies the required ADC;
this revision requires no new environment variable or preliminary server rollout.
See [publication prerequisites](.oberth/bootstrap.md#publication-prerequisites)
for the exact administrator handoff and placeholders.

### Layered trust enforcement

- **Google WIF** enforces namespace-bound ServiceAccount subject, exact
  per-repo release-SA identity, and role-specific pool audience. A token
  projected for one pool cannot be exchanged against another.
- **Oberth** enforces release-trigger-only projection and reachable-tag
  admission. Branch pipelines never receive a WIF-scoped token. The WIF
  role annotation is validated against the administrator's per-repo
  capability grant at admission -- an unapproved template/role pair is
  rejected before the Job exists.
- **No custom JWT claims** exist or are needed. The Kubernetes projected
  token carries the standard issuer, subject, and audience; Google's
  provider conditions and Oberth's admission gate are the enforcement
  layers.

### Active WIF templates

- `release-publish-images`: annotation `oberth.ci/release-wif-role: image-writer`.
  Fetches cosign-secret via secretstore exec for signing. Registry auth via
  the credential helper using the image-writer role.
- `release-verify`: annotation `oberth.ci/release-wif-role: image-reader`.
  Uses the committed public signing key without invoking secretstore.
  Registry auth via the credential helper using the image-reader role.

- `release-publish-chart`: annotation `oberth.ci/release-wif-role: chart-writer`.
  Fetches only cosign-secret; Helm OCI uses the helper config without login.
### Security controls (all roles)

OBERTH_RELEASE_WIF=1 required, ADC path fixed at `/run/oberth-gar/adc.json`,
`external_account` type exact, projected token regex enforced, STS exchange
followed by `generateAccessToken` with 600s lifetime, TLS 1.3 minimum,
redirects refused, proxies disabled, stdout must be a FIFO, no credential
in logs/args/files, response sizes bounded at 64 KiB. Only the Docker
credential-protocol pipe receives the access token.

### Two-edit reviewed rollback (per role)

The legacy static-key authentication remains for an explicit reviewed source
rollback. For each role, the rollback is exactly two edits in `release.yaml`:

**image-writer rollback** (release-publish-images):
1. Remove annotation `oberth.ci/release-wif-role: image-writer`
2. Add `--path=oberth/data/release/gar-image-key` to secretstore exec args
   and `oberth/data/release/gar-image-key` to workflow-level secret-paths

**chart-writer rollback** (release-publish-chart):
1. Remove annotation `oberth.ci/release-wif-role: chart-writer`
2. Add `--path=oberth/data/release/gar-chart-key` to secretstore exec args
   and `oberth/data/release/gar-chart-key` to workflow-level secret-paths

**image-reader rollback** (release-verify):
1. Remove annotation `oberth.ci/release-wif-role: image-reader`
2. Add `--path=oberth/data/release/gar-reader-key` to secretstore exec args
   and `oberth/data/release/gar-reader-key` to workflow-level secret-paths

Admission then supplies the ordinary Secretstore projection and the template
uses its existing legacy auth path. An unconfigured server rejects an
annotated WIF leaf before execution; there is no automatic fallback on an
exchange failure.

Oberth also annotates `release-publish-chart` as `chart-writer`. All three leaves explicitly
preserve the server-injected WIF marker and ADC path across `env -i`.
`release-verify` uses the committed public signing key without secretstore.
Its rollback's second edit restores the structured `oberth secretstore exec
--dir=/run/oberth-secrets/release --path=oberth/data/release/gar-reader-key --`
wrapper and forwards `OBERTH_SECRETSTORE_DIR` to the existing clean shell,
as well as restoring that workflow secret path. The other two leaves keep
secretstore solely for signing material; their rollback restores the matching
GAR key path to that existing wrapper and the workflow declaration.

The image and chart Go consumers invoke the same fixed Python helper through
an OS stdout pipe on each authentication request, bound output to 64 KiB,
and discard helper stderr. They never consult ambient Docker credentials.
Helm, Cosign and Trivy use a private public-only `credHelpers` config; WIF
never runs Helm registry login or writes an access-token file. The legacy
key route is selected only when `OBERTH_RELEASE_WIF` is empty; `1` selects
WIF and every other nonempty value fails closed.

Both Go publisher binaries are rebuilt from admitted source and re-pinned in
`.oberth/pins/bootstrap-tools.sha256`. Publisher source verification includes
`scripts/oberth-release-wif-credential.py` before any credential is consumed.

Upgrade override grammar (#888): each `--set` accepts one canonical dotted key
and scalar string; repeat the flag for multiple assignments. Escaped literal
commas in values retain Helm semantics. Unescaped separators, list syntax and
escaped/noncanonical keys fail before any mutation. Pin and schema checks use the same grammar. This is a documented subset
of Helm strvals, not a replacement parser.

### Fixed-field MCP wait diagnostics (#863)

Only authenticated `wait`/`promote_status` handling emits the private fixed-schema
start/terminal diagnostic pair and fresh public `X-Oberth-Wait-ID` nonce. JSON
preparation refusals stay JSON; diagnostic time begins after streaming
preparation. No argument, actor, target, RPC ID, payload, header or raw error is
logged. The older internal-error logger is bypassed only for these two tools,
while public error classification is retained. Counters saturate at 65535.
Heartbeat ownership, bounded writes and positive join remain unchanged; final
flush is a server-side observation, never client acknowledgement. Correlation
adds no authority, retry or durable-work cancellation, and cannot retrospectively
attribute uncorrelated transport failures.

## Native step termination observations (#894/#895)

Schema 21 adds optional bounded termination JSON to durable step results. The Argo
engine retains the exact selected retry node identity and reads its owned Pod
before normal cleanup under a five-second aggregate observation budget. Current
`main` termination, or the current failed init container when main never started,
is reported only with matching Workflow/node/Pod/container identity. Init evidence
adds `container_kind: init`; absent/`main` means the existing main observation.
Retry ambiguity, unsupported container sets, unavailable Pods and contradictory
status produce a fixed unavailable category. Unknown reasons stay normalized;
no environment or event dump is retained. An observed termination message is
now retained as authorized run output (`message`, at most 1536 UTF-8 bytes,
`message_truncated` when bounded further to keep the diagnostic within 4096
bytes). Log summaries quote control characters. Message contents do not establish
infrastructure authority. Container OOM reason and Pod deadline reason are
separate observations; exit 137 alone means neither.

**Native deadlines:** Workflow and template deadlines are authored unchanged.
No default, ceiling, startup allowance, no-progress expiry or server watchdog is applied.
Deadlines are optional. A never-schedulable Pod without an authored deadline holds its Argo global slot until cancellation.
Only explicitly enabled assisted holds extend deadlines for their own lifecycle.
Old scheduling annotations on adopted running Workflows are historical evidence and ignored as policy inputs.

**Agent-assisted continuation (retry hold).** Off by default (`assist.enabled`).
A branch-tier CI run that asked (`git push -o oberth.assist=1`, `run_rerun`
`assist`, `oberth run rerun --assist`; immutable `runs.assist_requested`) and
declares no secret-store paths gets, on every single-main container/script leaf
without its own `retryStrategy` (and no `templateDefaults.retryStrategy` in
the document), credentials, step identity or token mount, a
server-owned `retryStrategy` (`limit 1`, `OnFailure`,
`expression: asInt(lastRetry.exitCode) > 0 && asInt(lastRetry.exitCode) != 137
&& asInt(lastRetry.exitCode) != 143 && asFloat(lastRetry.duration) < <floored
step budget>`, the budget being the leaf's own deadline, else the inherited
`templateDefaults.activeDeadlineSeconds`, else the workflow budget) and a
server-owned conditional `podSpecPatch` (`{{= asInt(retries) > 0 ? held :
base }}`; a retry attempt exists only after the retry expression passed, and
the patch is re-substituted for a deferred retry Pod with zeroed `lastRetry`
values, so it must not repeat the discriminator). Attempt 1 is unchanged. The held retry parks before its command
(`ARGO_DEBUG_PAUSE_BEFORE`), has a Pod deadline of the floored step budget plus
two `assist.holdTTL` (the server abandons an idle unreleased hold once `holdTTL` has
passed; a hold with an agent exec, patch or continue ages from its last action,
at most two TTLs after `held_since`; the Pod deadline is the backstop), a writable `/work/src`, a read-only `/work/cache` with its
cache variables moved to private scratch, and the server executable mounted.
The Workflow deadline grows by two hold TTLs. Server-owned annotations
`oberth.ci/assist` (Workflow) and `oberth.ci/assist-hold` (template, refused
from repositories) mark the mode. Release, tag, promotion, schedule, plan and
apply runs never hold; a deadline-killed (signal exit code 137/143, or an
attempt that ran at least its floored budget: Argo records the kubelet kill
with the forwarded signal's exit code, not `-1`), evicted or executor-failed
attempt is never retried in the Workflow, so #1499 classification and the
run-level infrastructure retry are unchanged, a timed-out step never gets a
second budget, and a hold that expires is the step's own failure. The tools
`run_hold_list` (read), `run_hold_exec`, `run_hold_patch`, `run_continue` and
`run_hold_abandon` (any uplink whose scope covers the run's repository, or an admin, active run only, serialized per
run, capped by `assist.maxConcurrentHolds`, bounded by `assist.execTimeout`,
`execOutputBytes`, `patchMaxBytes`, `patchMaxFiles`; argv-only exec; exact-
context unified diffs applied through the Pod helper `oberth internal-hold-fs`
with no symlink following, no `.git`, no renames or binaries) append to the
audited `runs.assist_record` (`run.assist.<kind>`) before they execute; a
failed release abandons the Pod. Admission refuses every `ARGO_*` environment
name from repositories (template defaults included) and a hold is
recognised only on a retry node (`(N)`, N >= 1). **A run with any exec, patch or
continue on a held step is assisted** (`runs.assisted`, monotonic), also when a
continue follows nothing: it is never a promotion source, never published upstream,
never resolves a CI issue. No status,
label or layout is added; the existing status carries the reason line
`assisted: step <name> continued after patch sha256:<12 hex> by <actor>; not
evidence`. Held runs retain Argo's native parallelism and template locks.
Cancellation uses native Stop or Terminate; Argo TTL owns Workflow deletion.
No RBAC is added for assisted continuation: the
Argo Role's existing `pods/exec` and `pods` delete verbs are used in the
pipeline namespace only. See
[assisted continuation](docs/assisted-continuation.md).

Step log collection selects the failed init instead of requesting nonexistent
main logs. Received complete records and inclusive timestamp/count/digest cursors
have a bounded write-ahead journal, repaired before adoption resumes. Finished
cursors do not replay old Pods. Final requests have a 60-second lease, at most
three attempts, and a shared 120-second drain budget including follower overflow.
Submitted Workflows retain owned Pods through their completion TTL (minimum
180 seconds, no immediate PodGC or indefinite finalizer). Missing logs and
termination details remain explicit unknowns. Journal errors mark continuity lost
for that run, fall back to retained output without restart cursors, and never
escape as a fatal dispatcher error. A fixed retained marker and best-effort
`run.log-continuity.lost` audit observation record the degradation. Commits capture
intervening prefixes and use one WAL sync, with one final projection sync; active journals count
toward retention, and synced/indexed terminal runs prune them immediately. See
[log continuity](docs/log-continuity.md).

`run_get` exposes this record through the existing typed step JSON. Old NULL
records explicitly mean `not-recorded`; no historical cause is backfilled.
Malformed optional evidence cannot replace the original step verdict. Separately,
failed selected Pod nodes with kubelet eviction or node-pressure messages
are classified as infrastructure (#1138); an owned failed Pod's live status can
also establish the cause before main terminates. The bounded kubelet message is
retained as the run error, separate from the typed termination JSON. A container
`OOMKilled` without node-pressure evidence remains a test failure, including
when it reaches its own declared memory limit. Application termination-log
suffixes never establish node pressure. Only a run whose failed
steps all have infrastructure causes is reclassified. It retains step rows,
clears run-level test attribution, permits eligible branch `run_rerun`, and does
not open/update/resolve repository CI-red issues. Executor scratch follows native Argo configuration; the removed nonroot
preparation patch no longer sets its volume bound. These
records trust the Kubernetes API/kubelet and bind observation time separately
from container event times; they are not independent execution attestations.

The OpenBao CI leaf fixes compiler parallelism at 1, GOMAXPROCS 2 and a
256 MiB per-process Go memory target within the retained #1287 R1 4096Mi container limit.
It uses the same verified module and build caches as the other Go test leaves,
with `/cache/gomod` and `/cache/gobuild` work-claim subPaths as the fallback and
server-owned repository cache paths when configured. Setup tools remain read-only;
the leaf receives no service-account token or real Vault credentials.
Download, compilation and execution have separate 45s/600s/120s bounds;
the 791s outer deadline leaves 26s cleanup margin. Download uses 40s plus 5s
TERM-to-KILL, compilation 595s plus 5s TERM-to-KILL; checksum verification and
extraction remain inside the download phase. The integration binary omits race
instrumentation; test-store/test-service/test-rest keep their existing race runs.
All `TestPolicyIsolation` cases, the widened-policy negative control and
setup-helper TLS tests remain required. Compilation keeps `-mod=readonly` and
direct execution retains the installer package working directory. OpenBao is a
pinned, checksum-verified release download, not compiled here. Warm local builds
are under a minute; resource requests are provisional pending CI remeasurement.

## Release capability preflight (#1139)

Changed `.oberth/release.yaml` documents are checked statically against live
repository grants, named-step identities and the public releaseWIF capability
map. Branch CI reports advisory `[release-preflight/check]` log entries;
promotion planning refuses gaps before both fast-forward publication and merged
CI enqueue. Comparison uses the fresh default for branches and the exact fetched
target base for promotions. Declarations observed before authorization are
recorded under the canonical repository and tested SHA for pending access lists.
Only public source, grants and ServiceAccount GETs are used; no credentials,
TokenRequests, Workflows or source claims are created. Existing chart RBAC
already provides ServiceAccount GET; no additional permissions are required.
Release admission remains authoritative and forced tag reruns recheck live grants.

## Remote credential and grant lifecycle (#1105)

`uplink_list` returns public identity, fingerprint, admin flag, scope, creation,
last-seen, expiry and revocation times, plus pending activation status. Pending
credentials have no reported revocation time. All five new lifecycle tools
require an unscoped admin; ordinary and scoped uplinks are refused. `uplink_revoke` and
`uplink_rotate` require an unscoped admin and the mutation gate, atomically revoke
SSH/bearer authority and audit the action, retaining the public registration.
The last active unscoped admin credential cannot be revoked. Rotation is a
handoff to an already-issued, active, distinct replacement with the same
identity, admin flag and scope; key/token issuance remains in-pod.

New uplink bearer tokens default to 90 days. `serve --uplink-token-default-ttl`
and `--uplink-token-max-ttl` configure issuance: `0 < default <= maximum <= 8760h`
(365 days), with a default maximum of 365 days. Chart values are
`uplinkTokens.defaultTTL` (2160h) and `uplinkTokens.maxTTL` (8760h).
The in-pod CLI reads the running daemon's policy over its private admin socket;
policy read failures refuse issuance. `uplink add --ttl 24h` may select another
positive lifetime within that maximum. Expiry is enforced at the exact boundary,
including during last-used telemetry coalescing. Rotation without `--ttl` uses
the shorter of the existing remaining lifetime and the current default.

Existing credentials retain their stored expiry; missing expiry is never
backfilled. `uplink list` marks active legacy credentials `NO EXPIRY (rotate)`;
`uplink_list` and the unscoped-admin-only dashboard `/api/uplinks` view expose
`no_expiry`. The dashboard Status page lists those public identities with a
rotation warning. The server warns at startup and hourly while any active,
unrevoked uplink token has no expiry. Counts and warnings use read-only queries
and log no credential material.
Expiry affects the bearer token; SSH-key revocation remains a separate action.
Expired replacements cannot satisfy rotation or last-active-admin checks.

Admission replaces pending declaration observations from both immutable pipeline
documents at the tested commit, including an empty snapshot. Malformed or
unreadable documents preserve previous observations. `access_dismiss` is an
audited unscoped-admin-only exact-tuple deletion; a
later declaration can reappear. These additions require no schema or Kubernetes permission changes.

### Repository archive (#1102)

`repo_archive` and `repo_unarchive` require an unscoped admin. Archive preserves
repository identity, runs, logs, artifacts, receipts, grants, issues and audit
history. It refuses queued/running work, pending promotions/publications,
unfinished cancellation cleanup and pending Git receive recovery. New runs and
promotions are refused with `repository archived`, as are new `workflow_capability_grant`,
`promote_grant` and `access_allow` grants; existing grants stay
readable and revocable. Schedules skip archived
repositories before reading Git. Git refreshes/probes fail closed, and the
qualified cache is removed under receive/refresh locks. Retry archive if cache
removal failed; the catalog stays archived. Unarchive allows lazy cache recreation.
Dashboard repository lists (runs sidebar, repositories page and the issue
repository selector) default to active repositories; the `archived N` filter,
remembered per browser, shows archived ones last with their counts and historical
links, and a selected archived repository stays listed (#1498). `repo_list`
hides archived repositories by default and reports them in `archived_hidden`;
`archived=include` returns the whole catalog with archive metadata and
`archived=only` the archived subset. Run history, logs and audit stay readable.
Archived repositories are never deleted: their runs, publications, promotions
and custody records are append-only evidence, so `repo_remove` still refuses
once runs exist. Upstream-level health probes remain shared by active
repositories; an upstream used only by archived repositories is not contacted.

Migration **v29** follows the admission-sequence migration v28 (#1092). It
indexes immutable `repository.archive/unarchive` audit transitions, which are
the archive state source; repository table shape and historical rows stay
unchanged. Store transactions guard admission. Chart `archivedRepositories` contains
fully qualified identities: releaseWIF entries for these names fail rendering.
Keep that operator-owned list aligned with the live archive catalog; Helm cannot
query the server database during offline rendering.

## Issue lock and bulk lifecycle (#1103)

CI projection never takes or renews issue locks (#1170). These lifecycle tools
apply to explicitly acquired coordination locks, including retained legacy locks.

Issue reads (`issue_get`, `issue_get_many`, list and HTTP) include `lock` with
owner, acquisition and expiry times, or null when free/expired. Reads do not
renew locks. Lock conflicts retain their error class and include the owner and
expiry. Admin-only `issue_unlock` is scope checked and atomically deletes the
lock with `issue.lock.break`, recording the previous owner. The former owner's
explicit renewal fails with the existing not-owned error.

Admin-only `issue_close_many` accepts 1..50 unique positive IDs and a nonempty
reason (at most 4096 bytes). Validation precedes mutation. Results retain input
order and report closed, not_found, or lock_held with lock metadata for each ID;
hidden issues are missing. Closures append the reason to the body and commit
one `issue.close` audit action each. Existing locks still apply, including for
CI issues; admins must explicitly unlock first. Already closed retries are
idempotent. These changes use existing tables and require no schema or RBAC
migration. Existing single-issue mutation authorization is unchanged.

## Retained run and step timing (#1128)

MCP `run_get {summary: false}` (the default is the compact summary), default
`GET /api/runs/{id}`, and `oberth run <id> --json`
expose `Timing`, version 1. Dashboard polling selects `?view=dashboard`,
which omits timing and bound termination diagnostics. Per-step resource
measurements and sizing reports are absent in v0.22.0.
These are observations, independent of the verdict and the older `Run.StartedAt` field.
Missing timestamps are unknown; they never mean a zero duration. Legacy rows
have `unavailable=not-recorded`; no timestamp is backfilled from `StartedAt`.
Timing detail is retained for the newest 2000 terminal runs (and every run the
evidence retention protects); the retention sweep removes the Workflow and step
records of older runs, keeps their ordinary run milestones, and their
timing reads `unavailable=pruned`. `system_status.storage.database_bytes` reports
the live database size the backup ceiling is measured against.

Owner resume retains the durable run ID and verified Workflow binding. Stranded
runs fail after restart without resubmission; explicit retries have new IDs.
Known milestones remain write-once. Restart timing epochs are removed while
retaining current Workflow and step evidence. Queue time is `queued_at` to
`claimed_at`, not queued to the historically overloaded StartedAt. Checkout
completion, source seed start and readiness, Workflow creation, and durable
terminalization have separate timestamps. Source seeding is once per Workflow,
not once per step. Admission episodes, requested/granted/closed fields, and their
table and triggers are removed by a forward migration; there is no alias.

Before TTL, the server retains the admitted expanded task/template identities,
simple dependency names, Workflow UID and spec identity. No commands, arguments,
conditions, environment, token contents or object dumps are retained. Graphs are
bounded to 2048 tasks / 256 KiB; unsupported graphs remain explicitly unavailable.
Per node/Pod incarnation, timing contains Pod creation, PodScheduled transition,
current init-container start/end (including executor identity initialization),
current main start/end, and node completion. A failed init never supplies a
main timing: its observed init timing remains, and main is unavailable.
Dependency readiness is logical
readiness: the latest successful simple DAG predecessor finish, or DAG start
for a task without predecessors. Expressions, guards, loops, retry ambiguity,
and unsupported shapes leave readiness unknown. This is not a measured Argo
controller dispatch timestamp. Uninstrumented script execution remains `mixed`;
image-pull timing is `not-observed`. Neither Pending nor Pod startup measures
image download or proves a tool-cache hit. Verified pin-addressed tool caching
remains separate work in the consolidated overhead backlog.

Pod evidence uses a Workflow-scoped LIST with 128 objects per page, at most 16
pages / 2048 objects, and one total five-second API deadline. A separate one-second bounded database
write retains explicit unknowns even when that API deadline expires. Continuation loops,
resource-version changes, duplicates, bounds or page failures refuse a partial
snapshot (`snapshot-incomplete`; first-page failures are
`observation-unavailable`). Every projected record retains the exact existing
run, Workflow UID/owner, node, Pod and current-container checks. A missing Pod is
unavailable, not evidence it never ran. Completed leaves are retained during
Wait and final evidence is collected before replaying potentially large logs
and before cleanup. Per-record projections are at most 16 KiB. Unknown evidence
may be upgraded to a validated available observation of the same attempt; an
available observation cannot be replaced by a different attempt or rewritten.
The timing reader requires only the durable database, including after TTL or a
server restart; normal run authorization and retention/purge rules apply.

An adopted branch Workflow (#1172) is observed under its original run ID and
Workflow UID, so resumed observation re-collects the same node/Pod attempts:
identical observations are idempotent, unknowns may still be upgraded, and a
different attempt for a retained node is refused. No milestone is re-recorded
by adoption. Admission wait history is not recorded.

Migration v30 stacks after #1102's repository-archive migration v29. Shipped
migration bodies and existing ledger rows remain unchanged; older backups
replay v29 and v30 in order on restore.

Live-log SSE streams revalidate their original bearer credential every 30 seconds
with a five-second authentication deadline. Revocation, rotation, lookup failure
or changed actor authority emits a fixed `auth_error` event and closes the
stream. A new connection must authenticate normally. Idle streams stat the
append-only log before full reads and refresh the authorized view roughly once
per second to observe terminal state and purges.

## Upgrade queue observation (#1200)

`upgrade --wait-idle` / `--wait-for=all` observe all nonterminal runs through the
selected server's private admin socket; `--wait-for=critical` excludes ordinary
branch/schedule CI and retains tag/release, promotion, plan, apply and unknown
triggers. Classification is server-owned and installation-wide, from one read
snapshot. The admin endpoint reads only an in-memory sample, refreshed in the
background on a dedicated read-only WAL pool, with a five-second read budget
and a one-second interval (no Argo scan or wait for the primary writer connection).
Missing, invalid, failed or ten-second-old samples return unavailable, never
idle; age is measured from read start so a delayed result cannot look fresh.
The versioned private response preserves old all-run status codes;
critical selection refuses unsupported observation. No schema, grants or RBAC
changes are required. This is advisory, never an admission freeze or proof that
all other operations have stopped.

Positive `--wait-timeout` defaults to 2h and is independent of the existing
Helm/rollout `--timeout`. Progress and elapsed heartbeats stream while waiting.
Per-probe transport timeouts, interrupted response reads and HTTP 5xx are retried
within that original wait deadline, with exponential backoff from one second up
to thirty seconds. Transient diagnostics print at most once per minute; deadline
or cancellation errors retain the last consecutive probe failure. Auth/other
4xx errors (except the busy 409), unsupported critical observation and malformed
snapshots refuse immediately. Successful busy observations reset the backoff.
The local wait child has that explicit context deadline and a bounded output-
pipe join; cancellation, lookup errors and child failure refuse before upgrade
mutation, preserving context causality. CLI refusals and failures are nonzero.
A same-version no-op exits zero with `Already running vX; no upgrade performed`;
explicit dry-run/help also succeed.
Existing Workflow adoption and publication ownership rules
are unchanged. See [restart handover](docs/restart-handover.md).

A currently observed nonzero init-container termination always refuses restart-cut
classification, including when main is still Waiting/PodInitializing. This guard
is local to restart-cut and does not depend on the log-continuity input. The
shared partial-execution evidence contract and its RCA/fallback regression are
compiled in this topic and included in its bundle.


**Infrastructure recovery / run-list UX:** no automatic infrastructure retry remains.
Authors use native Argo `retryStrategy`; manual `run_rerun` retain
existing safety checks. Source preparation precedes Workflow creation and needs
manual retry after a failure. Historical audit receipts remain immutable.
Raw failed/interrupted statuses remain; public display states are queued,
running, passed, failed and cancelled. No retry-link status is emitted.

Readiness uses a 30s background reader budget, retaining green on deadline or
typed SQLite reader contention without renewing the 90s last-success age.
Confirmed writer failures receive no second 90s grace. Hard errors,
startup recovery, liveness, write integrity and admission gates remain enforced.
See [recovery, history and readiness semantics](docs/rca-run-recovery.md).

### Native run waiting

Argo and Kubernetes own waiting. Pause the controller at zero replicas or use
`argo suspend`; parallelism zero is unlimited. See
[native scheduling](docs/native-scheduling.md).

### Operator-owned workflow capabilities (#1294)

Resource templates require an audited API-kind/action grant
for the durable repository ID and execution namespace. The operator provides
the ServiceAccount, namespaced RBAC, namespace ResourceQuota and any device
capacity. Oberth does not create infrastructure for a particular workload.
Resource admission checks a static, single-object envelope and explicit API
version/kind/action allowlist. Kubernetes discovery must confirm namespaced
scope before execution. Grants cannot combine operator authority with
secret-store or WIF identities. The server fixes the resource executor identity
and rechecks current grant authority before submission, token delivery and
restart adoption. Repository YAML cannot choose or widen it.

GPUs and other extended resources use vanilla Kubernetes requests and limits,
with no Oberth grant or amount bound. Operators enforce namespace ResourceQuota
and admission policies, including limits on device-plugin resources. The source
receipt pins exact source and run binding across submission.

Retired execution tables are inert; existing migration SQL remains unchanged.
The five legacy execution tools and technology-specific runtime are removed.
Historical run rendering remains available. Legacy annotations fail with a
workflow capability migration message, and legacy Helm values warn and are
ignored. Pipelines own resource creation, test gates and cleanup through the
operator identity. See [workflow capabilities](docs/workflow-capabilities.md).

## HTTP/MCP wire names

Public typed records use snake_case names only, after redaction. Every tool
that selects one run takes it as `id`. HTTP collections are objects
(`{"runs": [...]}`, `{"repositories": [...]}`, `{"grants": [...]}`), the shape
MCP returns. Oberth is pre-launch: removed names are not kept as aliases, and
each removal is a breaking change in the release notes. See
[the wire names](docs/api-compatibility.md).

### Ordered promotion trains

`promote` accepts either `sha` or `sources: [{sha, ref?}, ...]` (1..32 unique
full commit SHAs), plus one target `branch` and optional `repo`. Each source
passes the same exactly tested green branch eligibility function as single-source
promotion. A supplied ref must match that tested branch. A train waits in the
existing repository/target queue, then fetches the current target once, merges
sources in order in one temporary checkout and runs one promotion CI job on the
final commit. Native Git must merge cleanly; failed CI never publishes the target.
Trains always test, including one-source, already-contained and unborn targets.

Conflicts identify the 1-based source index, SHA and paths before any run starts.
`skip_conflicting: true` aborts that source's merge and continues; all-conflicting
trains fail without a run. Operational Git errors are never skipped. The existing
promotion error/reason line retains skipped source evidence even after success.
The durable `train` object and admission/plan audit actions retain requested source
order, skipped flags/paths and the final `tree_sha` (Git tree object); `result_sha`
continues to identify the tested commit. `source_count` on `promote_status` and
`promotion_list` counts requested sources, including skipped ones; legacy rows
report 1. Automatic retries preserve every original grant, and both retry modes
recheck all sources, merge afresh and run CI. Every included grant
must remain valid through delivery; skipped grants are released and not consumed.

Authenticated `POST /api/promotions` accepts the same body and returns `{id}`.
`oberth promotion create --sources-file sources.json --branch main --repo <repo>`
calls the same MCP operation. See [promotion trains](docs/promotion-trains.md).

### Pinned repository source trees

Workflow `sources` declarations use exact commit objects, explicit canonical
consumer/producer grants and read-only `/work/sources/name` mounts. The server
fetches and verifies bounded trees through its existing registered Git access;
Pods receive no producer credential. Immutable `source_bindings`, submission
audits and run logs retain the selected identities. Missing or denied sources
refuse execution without fallback. The complete limits, migration and hosted
acceptance boundary are in [pinned sources](docs/pinned-sources.md).

## TRIZ contract

- **Main useful function (MUF)**: the one thing the system is for. A build service preserves the integrity of source, checks and released artifacts.
- **Harmful function**: the system does something it should not (unchecked
  source mutation, expired build receipt, corrupt artifact bytes). **Insufficient**: it does the right
  thing weakly (source validation only in one build lane). **Excessive**: it does more than
  the function needs (repeated caveats, redundant compatibility code).
- **Contradiction**: improving A worsens B (technical), or X must be A and
  not-A (physical). Naming the contradiction is the first step of any change
  that adds a mode, flag, alias, profile or escape valve.
- **Separation**: how a physical contradiction is resolved without compromise:
  in space (different places), in time (different moments), by condition
  (different inputs), or by system level (move it up or down). A compromise
  ("enforce by default, but allow off") is not a resolution; it is the
  contradiction left open with a flag on it.
- **Ideality**: useful functions divided by harmful functions plus cost. A
  change that adds a part must say which function it adds or which harm it
  removes.
- **Resource**: something already in the system that can carry a function for
  free (the build receipt already carries the source digest and check results).
- **Trimming**: remove a part and name what carries its function afterwards.
- **Guard**: the test, lint, generator or schema that makes a rule mechanical.


The contract guard runs in `.oberth/build.yaml` as `contract-guard`.
Canonical owner: Oberth.
The emitted guard is a build artifact. Regenerate it from the owner when
changing rules; review repository baselines separately.

| Law or rule | Guard and registry |
| --- | --- |
| 1. One owner per fact | `.oberth/contracts/check.py`: confession wording, generator exception |
| 2. Claimed equals measured | registered build outputs; `.oberth/contracts/generated-files.json` runs registered generators in check mode |
| 3. A mode needs a contradiction and separation | env, annotation and flag registry: `surface-registry.json`; chart settings are registered as values |
| 4. Every deprecation has a sunset | `deprecations.yaml`: required fields and date/version expiry; unregistered compatibility wording ratchet |
| 5. Scratch is not record | path and content residue ratchet across tracked source and documentation |
| 6. Composition roots | 200-line function span ratchet; parser/AST imports in Go tests prohibited |
| 7. Memory hygiene | unsafe and zeroing patterns outside memory hygiene packages ratcheted |
| 8. Test seams | test/skip/insecure/development env reads ratcheted |
| 9. Lifecycle on the default profile | review requires the qualified test cell; no live qualification is implied by this lint |
| 10. Gates have setters | literal Go test gates require a pipeline setter and a lane in skip diagnostics |
| 11. Specific, explained suppression | suppression and broad security-exclusion ratchets |

Existing findings are recorded as exact content fingerprints and per-rule
counts in `.oberth/contracts/baseline.json`. A removed finding cannot be
replaced by a different violation. New surfaces require a documentation anchor
and a qualified test cell. Empty registries record no approved exceptions;
existing debt remains visible in the baseline, not silently grandfathered by
file or directory. Reduce the baseline when removing debt.

The review checklist is `.gitea/pull_request_template.md`; issues use
`.gitea/ISSUE_TEMPLATE/contract.md`. Incorrect answers about copied facts,
unmeasured claims or missing sunsets block promotion.

## Plugin client endpoint and SSH binding

The plugin probes the selected plan's client endpoints before install review,
without using cluster type as an admission gate. Repository setup verifies the
selected install CA pin and reads SSH host identity and canonical git_path over
mTLS. Repo-local core.sshCommand uses a per-install SSH config and pinned
known_hosts; the global oberth Host alias never chooses a plugin push target.
See [plugin install and repository connections](docs/plugin-install.md).

# Bootstrap contract cell runtime and capture boundary (#1533 review follow-ups)

The branch/release rest wrapper records one existing race run and requires all
18 required (Go package, top-level test) terminal pass events. Its Go memory/timeout/cache/interactive
Git controls survive child-environment isolation. The race command has a wall
budget including compilation, followed by a bounded added cell/scan budget
inside the existing Pod deadline. CLI construction and the generated-key adverse
control use the same race compilation flags/cache. Failed stages/test identities
are public; captured output, argv and exception diagnostics remain private.
The originating stage/check survives final scanning. Failure-path scanning has
an independent bounded alarm after a cell timeout; a scan failure stays red.
Installer advisory warnings do not invalidate an aggregate green plan; every
non-warning check and the aggregate result must pass.

Installer fixture helpers export credential inventories into the standard Go
test output directory before fixture cleanup. Raw/base64 values and PEM body
lines are scanned against captures and decoded Go event Output, with overlap
across writes. A fresh Go-generated CA leak must be rejected by the parent even
when its fixture passes. This is not a complete assertion over all credentials,
all encodings or nested buffers in every rest-suite fixture.

Loopback planning contexts have distinct endpoints, CAs and bearer credentials.
Every HTTP method is recorded before body reading and non-GET API calls refuse;
TLS acceptance, request handling and server teardown have bounded waits. The
out-of-CI machine harness refuses pre-existing k3s configuration/drop-ins/data
and owned units/enable paths, uses private explicit k3s paths, and exclusively
creates units/binaries. Disposable-VM provisioning/reboot acceptance remains
unqualified; the contract cell grants no publisher privileges or tool-pin bypass.

IP client choices are preserved by the port probe and added to the leaf IP
SANs by the installer, including in plan, Review and summary. Connect checks
the pinned CA and the leaf's exact IP SAN before saving trust; invitation
clients perform chain, identity and exact pin verification before sending HTTP.
IPv6 URL authorities are bracketed. CA generation and constraints are unchanged.
Optional DNS naming produces the exact hosts line or A/AAAA record; an endpoint
hostname can use CNAME. For that DNS choice, the plugin waits for the user's
naming change and re-probes; it never edits hosts or DNS automatically.
NodePort reachability depends on client-to-node routing (NAT-isolated guests
cannot reach each other); offer port-forward or a routable name/IP. TCP checks
remain advisory.

Installer NetworkPolicy preflight reports measured TCP egress behavior against
its own server Pod, with explicit node/namespace scope. Six randomized selection
rounds include both sources; each requires two consecutive paired denials per
node. The unselected source must complete a fresh connection before and after
the selected attempt while a token-matched held response spans the observation.
Scratch namespaces copy no pipeline labels. Only the generated ownership label
and restricted enforce/audit/warn Pod Security labels (version latest) are set;
the owned policy uses the probe Pod label. Namespace-name/label-specific
exemptions and installed allow rules are outside this measurement's scope.
Creation-response Pod UIDs are fixed for the run and checked at readiness,
before/after explicitly container-selected execs and at traffic stage completion.
A changed identity invalidates the measurement. Exec has no atomic UID condition;
these bracketing observations reject results from a detected replacement.
Both sources connect twice at baseline and after each policy removal. Failed
fresh connections, overlap or recovery remain unverified. The `enforced` wire
status means the measurement completed: traffic cannot prove causality, and
independent coincident fault windows can still pass. Public evidence and human
output state this residual. Work stays bounded to 120s plus 30s cleanup, with
20s traffic phases, 12s paired attempts and the existing command/hold caps.
Each round deletes its created NetworkPolicy with a UID precondition. The
random ownership-labelled namespace is also cleaned with a UID precondition,
under a separate bounded cleanup context even after cancellation. The pinned
utility image and restricted, non-root, tokenless Pods are shared across CPU
architectures. Install (after approval) and explicit `install --verify --probe`
create these scratch resources and never mutate the operator's kubeconfig.
Default verification creates no cluster objects and reports that enforcement
was measured at install; this verification did not re-test it. Plan makes no
cluster mutations and reports that enforcement will run first at install.

Mac onboarding #1549: selected kubeconfig files are immutable installer inputs.
No step changes the current context or merges/replaces operator files. The Go
REST client disables auth-provider persistence. Printed
kind creation remedies target a separate new file with an existence/symlink
guard. Context loss in the reported rehearsal remains unestablished.

Mac onboarding review: capability evidence does not alter a stored policy choice.
The live check measures egress independently, with controls and explicit
namespace/node coverage, and reports bounded convergence as unverified. Probe
expiry reconciliation protects active/foreign namespaces. All native source
loading is migration-free; child kubectl/Helm use a private merged copy through
flags and environment, including setup and the add-repo proxy. Exec credential
plugins may maintain their own caches. Existing-release policy state propagates
to JSON maintenance verification. These extend the invariants above; normal
cleanup includes cancellation, while hard-kill leftovers reconcile next run.

Mac onboarding #1553: Kubernetes clusters and host prerequisites are supplied by
the operator. The CLI and setup wizard do not create clusters, load kind images
or run package managers. Missing prerequisites print commands/links and stop;
printed kind remedies are instructions for the operator, with a separate new
kubeconfig target. Validation and address waiting do not change settings.

Mac onboarding R3 #1548: existing-release maintenance reads stored values before
capability probing and preserves true, false or unspecified policy configuration.
Unverified capability is information during maintenance; fresh installs and
explicit `install --verify --probe` remain fail-closed. Cleanup carries the observed UID,
revalidates expiry and binds deletion to resource version; unknown or replaced
objects stay untouched. The live scratch-resource probe runs during install and
only when explicitly requested for verification.

Mac onboarding R3 #1549: transient kubeconfig copies carry a complete ownership
marker and inherited kernel lease in an owner-only user-cache directory. Handled
signals include SIGQUIT. Startup reclaims recognized, unlocked copies whose age
is known; active, unmarked, malformed and future-dated objects are preserved.
Cleanup errors are surfaced. A surviving child or absent next startup extends
the residual window; this is neither a hard wall-clock bound nor secure erasure.

Combined bootstrap contract: the loopback API refuses every mutation, including
scratch namespace creation. Fresh and maintenance plans pass their read-only
checks with an explicit unverified warning that enforcement runs first at
install after approval; maintenance preserves its stored policy choice. Plans
provide no enforcement evidence. Fresh install apply and explicit verification
retain their enforcement gates.

Recovery requires the current helper-storage policy before reseeding or
adopting a Workflow. Admitted commands are preserved without interpreting
server command wrappers.

The v0.22.5 workflow-scoped approval migration clears all existing secret
approvals with no backfill. Unscoped ConfigMap entries are rejected with an
error. After a committed reset, install and upgrade output says: all secret approvals are cleared; re-approve.
After re-approving, run `oberth secretstore sync` to prune old OpenBao roles and policies.
`access_allow`, `access_revoke`, and `access_dismiss` require `workflow`; lists,
pending observations, policy digests and generated step identities include it.
No approval-file default or alias is accepted.

- Argo security defaults: install and upgrade read the live controller ConfigMap.
  Existing security defaults are filled only with interactive approval of the
  displayed review or `--argo-security-defaults`; only absent leaves are filled.
  Compliant external controllers, including `--skip-argo-workflows`, pass without
  a release rewrite. `controller.configMap.create=false`
  passes when the live defaults comply; missing defaults require direct ConfigMap
  edits, with the v0.22.5 migration named for old-profile releases. Pod-level Unconfined
  seccomp is refused even when every container supplies a safe override.
