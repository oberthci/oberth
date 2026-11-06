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
  it never runs a shell in the server image. Legacy TLS certificates are read
  from the mounted Kubernetes Secret. Runtime public certificate/key reads
  use `oberth admin read-file` over authorized Kubernetes exec, restricted to
  the exact public identity paths and their in-directory projected-Secret
  targets. Pre-scratch releases lacking `admin` use a version-gated public
  `cat` read. Failed upgrade identity checks refuse the upgrade.
  A server predating native backup
  support is copied through a temporary read-only utility PVC mount on its
  existing node, preserving the before/after hash checks without shell exec
  in the server. No maintenance helper mounts a live database into a pipeline.


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
  wait protections remain intact. Startup copies freeze the capability maps;
  their public digest joins the immutable spec/audit binding. No provider, IAM,
  release-consumer cutover or key retirement is activated by shipping this
  feature. See [release WIF](docs/release-wif.md) for separate acceptance gates.
- Administrator-only `argo.releaseTagRoutes` / `--argo-release-tag-routes-config`
  maps canonical repository and tag globs to clean `.oberth/<DNS-label>.yaml`
  release documents. Defaults remain release.yaml for tags and build.yaml for
  branch/promotion/schedule. Ambiguous matches, invalid/reserved paths and missing
  selected files refuse. The selected file supplies its own normal size and
  resource contract consistently for sizing, planning, inspection and execution;
  the audit binds its path and commit. Default-branch tag admission and all
  identity gates still apply. Release preflight/identity plans/declarations
  include routed files with their source-file provenance. Dispatch requires
  `Run.RefKind == tag` and the stored bare name. Reserved build/fragment/schedule/
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
    itself isolate secrets from a same-UID Argo `wait` container. A separate
    server-owned patch removes `wait` mirrors of both the secret volume and
    main release token after Argo adds them (#621); the effective admitted Pod
    must still be checked for those absent mounts.
  - **Transitional:** `oberth secretstore materialize` absorbs the old
    `oberth-secret-materialize` shim, reading from envconsul-set env vars.
    `envconsul` remains the legacy path. Both chains produce the same
    `$OBERTH_SECRETSTORE_DIR` file layout and the same env stripping.
  Values and value-derived digests never appear in etcd, Job annotations,
  node disk (on swapless nodes or verified RAM-only zram), or server-side
  logs; only names and paths are annotated. `secretstore exec` exempts active
  swap only for canonical `/dev/zramN` partition entries whose kernel
  `/sys/block/zramN/backing_dev` reads `none`. Disk-backed zram, disk/file
  swap, mixed devices, and unknown/unreadable/malformed device metadata retain
  the warning or `OBERTH_REQUIRE_SWAPLESS` refusal. A device name alone never
  establishes memory-only storage; the existing absent `/proc/swaps`
  portability behavior is unchanged.
- Trusted-plan artifacts use the same short-lived Kubernetes-auth client and
  exactly one non-exportable OpenBao Transit key. The role has `update` only
  on the exact managed encrypt and decrypt endpoints; it has no key-management,
  export, backup, rotate, list, or wildcard Transit capability. Transit hard
  rejects the development HTTP override before login or network I/O.
- Installer-managed production OpenBao starts with verified TLS from its first
  listener: a credential-free bootstrap Pod writes the CA and server keys
  directly to the retained OpenBao PVC before Helm creates the StatefulSet.
  Private keys never enter Helm values, a Kubernetes Secret, etcd, or pod
  logs. Only the public CA certificate is copied into Oberth's ConfigMap trust
  mount. Existing HTTP installs stage the same bundle on their current PVC,
  publish the TLS config, then roll the chart's OnDelete pod before Transit is
  enabled.

### Isolated execution placement

Administrator-only `--argo-execution-placement-config` selects an immutable
closed run/repository/source/intent/cluster/node binding shared by Build and
direct source seeding. Empty configuration preserves existing behavior. The
selected profile requires authenticated controller owner-chain and immutable
admission-profile readbacks, an exact transport/binary/config identity, scoped
1Gi/64Mi executor scratch and explicit provisional helper resources. Whole
protected-Pod checks and exact native expected-leaf selectors remain mandatory.
Final tolerations compare the complete role-specific canonical set, separately
from native templates; unknown profiles and changed identities refuse. Local
controller/admission fixtures confer no managed-cluster authority. See
[execution placement](docs/execution-placement.md) for schema and runtime inputs.

The opt-in primary `protected-v2` contract binds one canonical parent and two
exact phase Workflows to a dynamically journaled generic protected intent.
Admission/config/capacity/audit are atomic before allocation; source-volume UID
custody is chained before seed/Workflow creation. Current authority includes
live authenticated target/environment checks at scheduler delivery/publication,
even when current source removes the protected contract. Recovery uses retained
configuration only for observation and original-UID cleanup; group removal,
revocation or mode changes grant no new create authority. The normal phase records
its full intended Workflow before POST and binds its original observed UID in
the audit chain. Uncertain submission retains its late-commit boundary until
Workflow, owned Pods and the exact source claim are absent; retries cannot
reseed or recreate a phase with recorded source custody. Expired placements and
removed recipe approvals retain cleanup ownership only. An absent recipe flag
or explicit empty recipe array starts a recovery-only executor, never a grant.
Direct scope remains credential-free branch execution. Imported templates require
exact fragment keys, commits, byte digests, resolved graph and lock digests in
placement. Version 2 placement admissions retain the original root and fragment
bytes atomically with runtime/config/capacity/audit, within the existing 512 KiB
admission bound; fragment-free version 1 custody remains compatible. Current
allocation, receipt and publication recheck the existing fragment trust policy
and exact source graph. Protected tag lookup uses private authenticated upstream
refs after a successful fresh producer fetch; deleted/moved tags refuse even
when public cached tags still resolve to reachable commits. These checks do not
materialize public tags or change ordinary fragment/replay lookup. Cleanup
reconstructs original bytes after resolver or
policy removal and cannot authorize new work. External bootstrap, installed
containing CI and real workload qualification remain separately required.

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
  are enforced as defense in depth on top of the identity switch. Secret paths a repository-authored envconsul
  configuration (`secret {}` stanzas in `-config` files, `-secret` flags) or
  an `oberth secretstore exec --path` invocation would fetch are
  admission-checked against the declared annotation, with envconsul
  configuration read from the immutable run workspace; a config file outside
  the source checkout is refused. The YAML must not declare
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
  credentialed or not, because only tag runs write the release root. It is
  also refused on nonroot leaves, which take no host cache mount. Any
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
  mirrored into repository helpers. On credentialed leaves, a server-owned
  Pod patch also removes Argo `wait` mirrors of the secret root and the main
  release token after Argo adds them; `wait` retains its own executor token and
  ordinary output mounts. This boundary still depends on actual controller
  behavior and must be checked in the admitted Pod. Container-set common
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
- Serving admission bounds concurrent workflows. Kubernetes places step Pods by
  their requests. Historical `oberth.ci/size`, `oberth.ci/size-by-trigger`, size
  weights, declared host CPU/memory budgets and memory headroom settings remain
  compatibility inputs; they do not reserve serving capacity. Explicit requests
  and limits remain in git. Missing requests use the documented per-container
  defaults. See [workflow admission](docs/declared-admission.md).
- Execution priority has four canonical classes: `low < standard < promotion < high`.
  Ordinary builds default to standard. Server-classified promotions and the oldest
  eligible waiting tag/release per repository use promotion, as do admin RC globs.
  Standard work ages into promotion after 60 minutes at its original FIFO place;
  neither a ref, tag, promotion, pattern nor aging can create high urgency.
  High requires an explicit unscoped-admin or unexpired priority-grant request.
  A promotion-only grant cannot authorize high. SSH checks the exact authenticated
  key fingerprint, never an identity alias; scoped admins need a grant.
  Low is deliberate background work and never ages. Every explicit admin override
  pins its class. `run_priority {run_id, priority, reason}` sets any waiting class,
  including lowering high to promotion/standard/low; it cannot preempt admitted work.
  Audit records retain actor, prior/selected class and reason. Admission FIFO,
  pressure, per-container resource, group and hold checks, and bounded dispatch
  capacity remain. Local changes refresh waiters immediately; polling is 15s.
  Tag eligibility still honors oldest-waiter, exact-checkout size and size-revoke
  rules; losing an automatic boost retains authorized explicit fallback, otherwise
  standard. Release execution itself is not refused by that advisory lookup.
  Submission transports and schedules accept the four names. Promotion/high need
  authenticated authority; low/standard need no urgency grant. A standard request
  does not lower natural delivery priority. Sync only raises an existing waiting
  build; explicit lowering uses admin run_priority. Accepted priority and automatic
  protection survive replay, restart and eligible infrastructure
  refire, retaining original FIFO and reasons. A new branch-head push replaces
  queued, not-started runs at older SHAs in the same repo/ref/schedule lane, at
  every priority class. The latest explicit admin override carries to the new
  head, with its actor and reason but a fresh FIFO place. Running work retains
  requester/admin supersession protection. Publication/cleanup guards remain.
  Legacy input normal maps to standard; authorized high/highest requests, overrides
  and grants map to high. Automatic legacy high/highest protection rows with an
  empty requested field confer no high authority. The timestamp migration
  `20261104190000-priority-four-classes` extends CHECK vocabularies and preserves
  raw historical rows and audit chains. Shipped migration 39 and admin-lowering SQL
  remain unchanged. Canonical API/CLI/MCP/dashboard output uses only the four names;
  MCP accepts deprecated normal/highest input aliases for current clients.
  No run status or layout changes. See [release-priority.md](docs/release-priority.md)
  for the exact compatibility, resource and fairness rules.
- Waiting admission uses workflow slots, the enabled PSI hard-limit valve and
  the configured data-root disk floor. There is no measured backfill, resource
  head reservation, drain forecast or starvation bypass of hard pressure.
  Historical head-wait records remain evidence and are not rewritten to explain
  current queue waits. Priority changes retain the existing audited transaction
  and admission mutex boundary. Current queue reasons identify the real blocker.
- A named scheduled run durably records its resolved schedule entry in
  `runs.schedule_entry` (v31). Cron and manual
  schedule triggers share that identity. Retry, rerun,
  replacement, stranded-run requeue and Workflow adoption retain it. Legacy
  rows stay empty; no current file is used to guess their entry. Non-schedule
  runs cannot carry a nonempty entry. `oberth-schedule` is a reserved,
  server-owned Workflow parameter: any repository declaration in
  `spec.arguments.parameters` is refused. Named schedules always receive it;
  other runs receive empty only when their document references it, otherwise
  it is absent. Named entries have separate same-branch supersession domains.
  Due cron entries queue even when the repository has active runs; resource
  admission and explicit groups bound execution. Cron atomically checks the
  consumed cursor and all active runs of that entry/branch before enqueue:
  overlap records `coalesced` and advances the cursor without replacing the
  live run; `fired` commits its run/receive/audit/cursor together. Read failures
  fail closed and are logged; cursor-read failure reports `failed_state`
  without advancing. Manual schedule triggers
  retain their busy-repository guard unless forced. Run list/detail expose
  `ScheduleEntry`; status/wait expose
  `schedule_entry`. See [scheduled runs](docs/scheduled-runs.md).
- Size grants/revocations remain historical and automatic tag-priority policy;
  they no longer gate workflow execution or make XL exclusive. Automatic tag
  priority still uses its existing eligibility checks and never supplies high
  urgency. Kubernetes decides Pod placement, with operator-owned namespace
  quotas bounding resources created through generic workflow capabilities.
- Workflow graph analysis and declared Pod demand remain available for validation,
  provenance and diagnostic metadata. They do not create a serving CPU/memory
  packing budget. Existing demand receipts remain evidence; read paths do not
  recompute historical records from newer git declarations.
- Numeric per-container resource ceilings are administered independently using
  `oberth resource grant/get/revoke` or `resource_grant`, `resource_grant_get`,
  `resource_revoke`. Grants contain separate requests/limits quantity maps.
  Migration v49 copies historical size-grant ceilings and provenance, including
  revocations; later size changes do not overwrite a numeric resource grant.
  Every fresh submission and release preflight enforces that ceiling; lookup
  failure refuses submission. Resource history never rewrites authored values.
- The server-owned workflow parameter `oberth-trigger`
  (`branch|promotion|schedule|tag`, the same durable run trigger) is set on
  every document that declares or mentions it, so `when:` guards can skip
  work per trigger, e.g.
  `when: "'{{workflow.parameters.oberth-trigger}}' != 'branch'"`. A
  document may declare it in `spec.arguments.parameters` only as a plain
  vocabulary `value` (the default an older server, which never sets it,
  evaluates); this server replaces that value. `valueFrom`, `default`,
  `enum`, a duplicate, or an out-of-vocabulary value is refused. A document
  that never mentions the parameter is submitted byte-identical to before.
  `oberth.ci/concurrency-group: <name>` names a group shared across repositories
  and all run kinds. `oberth.ci/concurrency-limit: <N>` is a positive integer,
  default 1. YAML is the only definition surface: no chart/admin definitions.
  Names remain 1–63 characters of `[a-z0-9-]`. The scheduler persists each
  immutable run's group and limit before granting it, and remembers the latest
  accepted build/release YAML declarations per repository. Conflicting limits
  are refused before execution, including after restart or while a group is idle.
  Declaration keys are `(repository, release)`, not branch/ref/filename: accepted
  YAML on one branch can clear or replace another branch's remembered declaration;
  conflicting limits still refuse without replacing it. A live/retained gate
  conflict fails only the incoming run as infrastructure, never the scheduler.
  An ungrouped declaration removes that pipeline's remembered definition.
  Live grants still constrain a changed definition until they finish.
  Members share at most N grants in priority/FIFO order, across branch, tag,
  release and promotion runs. A waiting group member owns no workflow slot or
  resource reservation. Granted work remains subject to the global workflow
  ceiling; Kubernetes decides pod placement. Cancellation, retained grants,
  VM parking and adoption preserve the same ownership boundaries. On upgrade,
  already-running legacy members are adopted even if their combined count is
  above the newly shared limit; new work waits for them to drain.
  Migration seeds only the latest active grouped run per repository/release slot;
  terminal history cannot create an idle definition. VM readmission retains the
  immutable YAML limit when reacquiring its group slot.
  The existing queued reason names the group, limit and current holders.
  There are no new run statuses, labels or layouts. `concurrency_groups` and
  `oberth admission groups` are read-only unscoped views; system status includes
  `admission.groups`. Scoped run/status views hide other repositories' holder
  and waiter identities. See [concurrency groups](docs/concurrency-groups.md).
- Server-owned helper operations (secret-store delivery, trusted plan
  capture/acknowledge/deliver/load) are exec'd through the same
  `go -C /work/src/.oberth run .` invocation with explicit flags; the SDK's
  `Main` dispatches them before any pipeline evaluation.
- Pipeline steps receive `OBERTH_REPO`, `OBERTH_REF`, `OBERTH_SHA`,
  `OBERTH_TRIGGER`, and `OBERTH_STEP` (the Argo template name), plus the
  documented Go cache variables. `OBERTH_STEP` is set on every template for
  observability and diagnostic correlation with per-step grant denials; it is
  NOT a security control (the value is self-declared by the workflow document).
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
- A repository may declare its per-trigger Job resource tier with one literal
  `var JobSizes = map[string]string{"ci": "M", "release": "L"}` — keys are the
  trigger names (`ci`, `release`, `plan`, `apply`), values the public tiers
  (`S`, `M`, `L`, `XL`). Like the secret declarations, it is statically
  parsed, never evaluated; computed keys or values are rejected. An absent
  declaration or trigger key selects `M`. Per-step `WithSize` remains runtime
  step metadata inside the Job (usage summaries, per-step telemetry) and does
  not set the Job's resources.
- A repository that declares trusted infrastructure work must provide one
  `Plan` burn and one `Apply` burn with the same literal `.Backend("...")`, plus
  a nonempty literal `var PlanLockFiles = []string{"..."}`. The server scopes
  the backend identity to the registered repository, digests bounded regular
  lock files without following symlinks, and admits Plan only through an
  explicit actor-attributed `plan` call for one exact already-green SHA and the
  target's exact current base.
- Trusted credentials are phase-explicit literal maps:
  `PlanSecretStoreSecrets` may contain only
  `oberth/upstream/<org>/<repo>/plan/<name>` paths, while
  `ApplySecretStoreSecrets` may contain only the corresponding `/apply/`
  paths. Legacy, org-wide, system-allowlist, raw KV, and cross-phase paths fail
  before store login, fetch, or Job creation. Plan paths are operator-owned
  read-only capabilities and Apply paths are operator-owned mutation
  capabilities; Oberth enforces namespace separation, not the external
  credential's privileges. A present empty value remains a valid zero-byte
  delivered file and is never installed as an empty masking pattern.
- Plan and Apply expose the same `ctx.Plan.Path` interface
  (`/run/oberth-plan/terraform.plan`) but share no writable volume or host
  cache: their Go module/build caches use separate `plan` and `apply` trust
  tiers. Plan writes at most 16 MiB at the fixed path;
  server-owned `save-plan` verifies and encrypts it through OpenBao Transit
  before persistence on the Oberth `/data` PVC. Apply uses server-owned
  `load-plan` and `verify-plan` in an init container, then mounts the admitted
  tmpfs file read-only into repository steps. `WithSecretEnv(env, delivery,
  key)` is valid only in its statically declared Plan or Apply phase and
  resolves an already-admitted phase secret immediately before exec.
- A ready plan binds repository/upstream, source and result SHA, target and
  exact base, green/Plan run, backend, Periapsis/config/tool/lock identities,
  artifact digest/size, actor, creation/expiry, and single consumption. An
  Apply-capable promotion must name that exact `plan_id`, remains
  fast-forward-only, publishes Git first, and then durably enqueues one Apply.
  Promotion publication evidence stays distinct from Apply status: Apply may
  fail after publication and never implies rollback. Authorization expiry is
  checked before attachment; an attached/applying plan is a committed
  obligation and is not replayable or replaced by a fresh plan.
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
  SHA256SUMS, SHA256SUMS.sigstore.json, cosign.pub} union {binaries from
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
- `/mcp` and `/api/*` require a bearer token. Static connection guidance may be
  served unauthenticated without exposing repository or run data.
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
  authority is configured.
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
  Mutations (`promote`, `sync`, `run_cancel`, `run_retry`, `issue_create`, …)
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
- MCP exposes 80 tools: `status`, bounded named-step `logs`, exact-run
  `run_get`/`run_logs`, read-only `run_resources` (captured history and reviewed git resource suggestions), `run_retry`, audited `run_rerun` (#1098), audited `run_cancel`/`run_purge`, `run_kvm_preflight`, `artifacts`/`artifact_get`, `wait`, `sync`, `promote`,
  `promotion_list`, `promote_status`, audited `promote_retry` (#1097), `publish_retry`, `publication_abandon`, issue
  `issue_create`, `issue_get`, `issue_get_many`, `issue_update`, `issue_append`,
  `issue_close`, `issue_reopen`, `issue_delete`, `issue_list`, `issue_lock`,
  `issue_unlock`, `issue_close_many`,
  secret-access list/allow/revoke, unscoped-admin-only uplink list/revoke/rotate,
  `access_dismiss`, deprecated admin-only `kubevirt_grant` / `kubevirt_revoke`,
  unscoped-admin-only `kubevirt_grant_list`, read-only `kubevirt_grant_get`,
  admin-only `workflow_capability_grant` and
  `workflow_capability_revoke`, read-only `workflow_capability_get`, admin-only
  `resource_grant` / `resource_revoke`, read-only `resource_grant_get`, admin-only `size_grant`
  and `size_revoke`, read-only `size_grant_get` (#1021), admin-only
  unscoped `admission_release` (#1099), `queue_cap_config`, `queue_priority_config`, `queue_priority_get`, `run_priority`, `priority_grant`, `priority_grant_list`, `priority_grant_revoke`, `repo_usage`, `repo_list`, `repo_remove`, `repo_archive`, `repo_unarchive`, `run_list`,
  admin-only `audit_list` (#1104; verified entries, never available to a scoped uplink;
  `GET /api/audit` shares its filters, response and restrictions),
  `schedule_list` (also `GET /api/schedules`, with the same repository scope) plus admin-only audited `schedule_pause`, `schedule_resume` and `schedule_trigger` (#1106; pause state is the audit chain, trigger goes through `EnqueueCI`),
  `system_status`, admin-only `secretstore_plan` (#611),
  `secretstore_sync_receipt` (#712), `secretstore_verify`, and
  admin-only `promote_grant`, `promote_grant_revoke`, plus read-only
  `promote_grant_list` (#826). The `promote_grant` tool's maximum lifetime is
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
  and mutation lookups still require exact durable IDs. `run_retry` and
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
- The retained legacy VM tools remain available during the preparatory release:
  `kubevirt_grant` and `kubevirt_revoke` are audited administrator mutations;
  `kubevirt_grant_get` reads a repository's current grant and shared VMI budget;
  `kubevirt_grant_list` is a read-only list of active grants for unscoped admins.
  Legacy KVM/VMI admission fails closed without an active repository grant.
  Grants bound VMI concurrency, cores and memory and do not become generic
  workflow capabilities. Archived repositories refuse new `kubevirt_grant`
  mutations; existing grants remain readable and revocable. Revocation blocks
  new submissions while existing workflows retain their cleanup authority.
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
  failed/infrastructure, with an upstream-publication reason. `run_retry` and
  `run_rerun` route these failures to publication retry without repeating CI
  or release work. Explicit publication retry resets the bounded budget.
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
- Scheduling queue position is one-based effective priority then FIFO order among all durable
  queued runs, not an ETA or execution order: publication-gated refs can be
  bypassed. Admission position is separately one-based among live workflow
  reservations, with `preparing`, `waiting`, and `admitted` states. Missing
  reservations or Workflows never imply a cause or a scheduler failure. A
  grouped reservation adds optional `group`; while it waits on its group it
  also names either `group_holder` (the run of the same repository holding
  the group) or `group_ahead` (the earlier member queued ahead of it), both
  server run IDs, and the scheduling message names the group and that run.
  An unparked reservation behind the queue head (the first claimed,
  prepared, unparked run that does not fit) carries `blocked_by` (`run_id`,
  `repo`, `ref`, `size`, `weight`, `full_drain` for XL, `run_ceiling`,
  `disk_floor`, `used`, `capacity`, `waiting_since`), and its message, like
  every unclaimed queued run's, ends "blocked behind <repo>/<ref> (XL, needs a
  full drain)." The head itself sets `head_of_queue`. Unscoped `/api/status`
  reports the same as `admission_blocker` (#1021). A scoped actor sees the
  blocker only when its repository is in scope, without the budget's use.
- `system_status`, `GET /api/status` and `oberth status` report pipeline
  NetworkPolicy configuration in the Kubernetes diagnostic (`cluster_detail`).
  The chart passes `networkPolicy.enabled` as `--pipeline-network-policy-enabled`:
  false produces a persistent warning; true reports configured enabled, with CNI
  enforcement unmeasured. An omitted flag reports configuration unavailable.
  This installation-wide diagnostic is also visible to scoped uplinks; it
  contains no repository or namespace details and never gates readiness or admission.
- `system_status` / `GET /api/status` carry `admission` (#1099): `capacity`,
  `used`, `runs`, `max_runs`, `blocker`, `reservations[]` (`run_id`, `repo`,
  `ref`, `workflow`, `size`, `weight`, `state`, `group`, `position`, `since`)
  and `groups[]` (`name`, `repo`, `holder_run_id`, `waiting[]`). `state` is
  one of `preparing`, `waiting`, `group`, `vm`, `granted`, `retained`. It is
  the live process's in-memory gate, never durable evidence. A scoped uplink
  sees only its repositories' reservations and groups, without the budget,
  its use, positions or the blocker. Admin-only, unscoped MCP
  `admission_release {run_id, force}` (CLI `oberth admission release`,
  remote only; `oberth admission` prints the view) releases a reservation no
  owner will release. The practical case is a granted reservation still held
  for a run that is already terminal because its owner hung or was lost; an
  ungranted reservation of a terminal run qualifies too. A `retained`
  reservation belongs to a stopping scheduler or an uncertain Workflow create.
  Cleanup must be confirmed before its owner releases it. A reservation of an active owned run is
  refused (use `run_cancel`), and so is one without a durable run unless
  `force`. Before a granted reservation is released the run's recorded
  Workflow is deleted through the engine and an accepted deletion is the
  proof; a granted run with no recorded Workflow cannot be proven gone, and
  it, like a failed deletion, needs `force`, which releases unverified. The
  outcome is audited as `admission.release` (`run_id`, `state`, `weight`,
  `group`, `workflow`, `verified`, `force`) under the gate lock once the
  reservation is known to be still held and before its weight and group are
  freed; a reservation its owner released meanwhile is reported as not found
  and not audited. The same grant pass admits the runs behind it.
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
  terminal-only sequence paging; `view=active` remains an unpaged, bounded
  deprecated compatibility view. Repository/ref filters and uplink scope apply
  to both. `run_list.status` accepts one existing display status or a non-empty
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

  `display_status` is exactly one of seven values: `queued`, `running`,
  `passed`, `failed`, `infra-retried`, `superseded`, `cancelled`
  (`model.DisplayStatuses`; `model.DisplayRunStatus` is the only mapping,
  and `TestEveryRawPhaseMapsToExactlyOneDisplayStatus` holds every raw
  status, phase and projection to it). A status says what to do with the
  run; anything finer is the reason line, never a second status. Every run
  that has not started is `queued`: durable queued, or claimed before
  admission (`Status` `waiting` on the wire, phase `waiting` or
  `preparing`). Admission records `started_at`, so an admitted run,
  including a trusted preflight before Workflow submission, is `running`.
  A queued run's `display_reason` names what it waits for, in plain
  words, computed once by the server for the list, detail, status, live-log
  and active views, the CLI and the dashboard: `next in line`,
  `position 3 in line, behind org/app/main (needs the whole host)`,
  `preparing to run`, `waiting for CPU budget (12.5 of 40 CPU free)`,
  `waiting for memory budget (8 GiB of 16 GiB free)`, `host IO pressure
  (42%)`, `10 runs already running (limit 10)`, `concurrency group deploy
  busy (held by run <run12>)`, `waiting for KubeVirt capacity (repository
  VM grant)`, `waiting for trusted VM capacity`, `waiting for disk space
  (... free, floor ...)`, `waiting for capacity (size M, 6 of 8 weight
  used)`, `waiting for admission` (claimed, no reservation known yet) and,
  briefly at the grant before admission is recorded, `admitted, starting
  the workflow` or `KVM preflight running`; a head-of-queue run adds `; smaller runs run meanwhile` or `; host
  draining for it`. A scoped viewer sees no numbers, positions or other
  repositories' runs (#073). `Scheduling.state`, `Scheduling.message` and
  `Scheduling.admission` keep the raw observation for machines.

  | Durable cause | `display_status` | `display_reason` |
  | --- | --- | --- |
  | queued, claimed before admission | `queued` | what the run waits for (above) |
  | admitted (including a trusted preflight before Workflow submission) and started | `running` | empty |
  | uncertain Workflow create (phase `infrastructure-pending`) | `running` | `Workflow create outcome uncertain; cleanup pending` |
  | running past its execution deadline | `running` | `Execution deadline exceeded.` (plus priority prefix) |
  | failed by its execution deadline | `failed` | `Execution deadline exceeded.` or the retained resource guidance |
  | passed CI / published release | `passed` | empty |
  | failed step or infrastructure | `failed` | failed phase and bounded cause; never empty |
  | upstream publication failure | `failed` | upstream rejection class, or upstream publication failure with transient cause |
  | queued publication retry | `running` | publication attempt/backoff message; not replaced by queue priority |
  | infrastructure failure automatically retried | `infra-retried` | `infra error, retried`; linked successor |
  | superseded (`reason='superseded by newer run'`) | `superseded` | `superseded by <short SHA>`; successor run remains linked |
  | restart re-fire (`reason='re-queued after restart'`; fallback when an adoptable branch Workflow is missing, #1172) | `superseded` | `server restart, re-queued as <run12>` |
  | explicit cancel | `cancelled` | `cancelled by <actor>: <reason>` |
  | restart requiring an operator retry | `cancelled` | `server restart, retry needed` |

  Raw status fields retain their values, but existing `display_status` values
  can change from `cancelled` to `superseded`, or `failed` to `infra-retried`.
  Clients must accept these display enum values. The former `waiting`
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
  Only fixed status vocabulary and curated reason summaries are returned:
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
- Step resource measurements never widen admission. Source-backed single-main
  container/script leaves with explicit commands run through the read-only
  server-delivered `oberth internal-step-exec` wrapper on new submissions
  only when the leaf has an explicit memory limit of at
  least 256Mi. The full server's package initialization consumes the main
  container's memory budget. Smaller or unspecified limits stay unwrapped,
  with an admission log/template annotation warning and null usage, without
  changing requests, limits or scheduling. Requests and inherited/defaulted
  limits cannot establish instrumentation headroom. The policy annotation
  preserves pre-policy wrapped specs on adoption; existing Pods are not
  rewritten. For measured leaves the wrapper forwards signals,
  preserves streams/exit codes, and writes a bounded, numeric-only v1 summary
  to the kubelet termination file. No new RBAC, hostPath, capabilities or
  writable root is required. The server reads it from the already-bound final
  Pod/main container status, never logs or LastTerminationState. This is advisory
  self-reported by untrusted build code, never trusted diagnostic evidence and
  never an input to admission or size grants. Any aggregation must saturate on
  overflow; currently counters are stored/projected individually without sums.
  Wrapped Pods add `kubernetes.io/arch=runtime.GOARCH` through strategic
  podSpecPatch merge, preserving workflow/parent scheduling selectors, since
  the seed copies this server's single-architecture executable. Catchable signals
  are forwarded to the child process group, except SIGCHLD and Go's SIGURG,
  matching emissary; SIGKILL/SIGSTOP cannot be caught. A child killed by a signal
  exits with 128+signal. A command that cannot start returns shell-style 127
  (a failed step), rather than emissary's executor Error classification.
  A step-written nonempty termination message is preserved verbatim instead of
  writing telemetry; its usage is unknown. No message contents are copied into
  the numeric usage fields. A wrapper killed before reporting leaves usage null.
  `MaxRSSBytes` is Linux wait4 maximum descendant RSS (not concurrent summed
  RSS); `MemoryPeakBytes` is this container's cgroup-v2 `memory.peak`, including
  cache. `UserCPU`/`SystemCPU` are cumulative container CPU from `cpu.stat`,
  including executor/wrapper overhead, stored/API-encoded as duration nanoseconds
  (divide by 1e9 for seconds). Missing counters remain independent nulls, including
  on cgroup v1, OOM/deadline kills that prevent reporting, missing Pods, skipped
  steps, resource/ContainerSet templates, image-only ENTRYPOINTs,
  custom termination-file paths or old workflows.
  No host-level aggregate fallback or deadline widening is allowed. Schema v33
  migrates each ambiguous legacy zero counter to NULL independently; positive
  legacy counters survive. Only newly collected summaries can establish zero.
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
  FIFO CI run for the exact accepted commit. A newer push to the same repository
  and branch marks the older run `interrupted`, records which newer run
  superseded it, and creates a durable cancellation obligation for any in-flight
  Kubernetes Job. Settling that obligation also withdraws the superseded run's
  admission reservation when it was still preparing or waiting, without a
  restart; a granted reservation is released only by its owner, once the
  Workflow is terminal or its deletion was accepted.
- An explicit run cancel (`run_cancel`, `oberth run cancel`, `oberth runs
  cancel`) terminalizes one queued, waiting or running run through an audited
  `CancelRun(actor, run, reason)`: the run becomes `interrupted` with phase
  `cancelled` and reason `cancelled by <actor>[: <reason>]` (projected
  unchanged by `run_get`, `run_list` and `/api/runs`), and a `run.cancel`
  audit action carrying actor, reason, previous state, force and any warning is
  appended in the same transaction. No run status is added: every consumer
  already treats `interrupted` as terminal and non-green, and a new status
  would need a `runs` CHECK-constraint table rebuild. A queued run simply
  never gets claimed. A claimed run receives the existing supersede obligation
  with no successor (`run_cancellations.reason = 'superseded'`, empty
  `superseded_by`): its admission reservation, weight and group position are
  withdrawn at once if ungranted, and its Workflow is deleted through the
  normal Delete path; the owner then
  settles without recording a result, releasing a granted reservation only
  after the deletion was accepted. The record is terminal before the deletion
  finishes, exactly as for a supersede; a failed deletion stays a pending
  obligation for the cancellation pass, including after a restart. The MCP
  request performs the Workflow deletion synchronously on its own context
  (`AcceptCancellations`): a client disconnect or timeout leaves the
  committed cancel with a pending obligation (`workflow: pending`) that the
  next cancellation pass completes. Cancelling
  a terminal run is refused (HTTP 409 / `ErrInvalidState`) without any change.
  A run owning a pending publication is refused: the publication owns it.
  Cancelling promotion CI fails the promotion in the same transaction, so no
  publication can begin and nothing is merged or pushed. A cancel projects no
  CI issue. Authorization: an admin uplink may cancel any run it can see, but
  tag/release and apply runs require an explicit `force` and record the
  warning that their publication may be partial (anything the Workflow
  already pushed stays published); a running concurrency-group member's
  cancel warns that the external state its group guards may need recovery. A
  non-admin uplink may cancel only `branch`-trigger runs outside any
  concurrency group in repositories its scope can see; release, tag, plan,
  apply, promotion and scheduled runs and group members are refused with
  `ErrForbidden`. The decision is re-made on the run as it is inside the
  cancel transaction; a run's group is known once the scheduler has read its
  document after claim, so a still-queued member is an ordinary branch run.
  Because a cancellation pass can settle an obligation before the owner
  creates the Workflow, the owner re-reads the run right before creation and,
  if it must delete a Workflow it created for a preempted run and fails,
  re-arms the obligation so the next pass or restart deletes it.
- An explicit rerun (`run_rerun`, `oberth run rerun <id> [--reason]
  [--force]`, #1098) queues a new run with the same repository, ref, SHA,
  trigger, tested and base SHA as one failed or interrupted run, for what
  `run_retry` refuses: a failed step (a flaky test), a run that is no longer
  the newest of its ref, and tag/release runs. The checks, the enqueue and a
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
  resolve the exact claim/requeue/publication identity before retrying.
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
  Workflow's run ID, ref, tested SHA, trigger, persisted concurrency group and
  namespace must match the run and configured engine. Its spec identity must
  be present; its UID, identity and live spec remain pinned during observation.
  A name collision or unreadable API stops recovery without deleting or
  re-firing that work. Startup logs and returns the run ID and the exact
  `<namespace>/<name>` of a refused Workflow (binding mismatch, invalid size
  declaration, conflicting concurrency group) with the `kubectl delete`
  command for it; once it is deleted, the next start takes the
  missing-Workflow fallback below. Admission weight is
  restored from the submitted Workflow's size declaration before new claims,
  and its repository-scoped group remains held. Existing work may temporarily
  exceed a reduced capacity; new work waits until it drains.
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
  amber/cancelled presentation and `run_retry` apply.
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
  Workflow is deleted and only that run fails with phase `infrastructure`;
  the scheduler keeps running. If that deletion also fails, the run stays
  owned for the next restart, exactly as for a submitted run.
  A missing branch Workflow, or a claimed branch run without a Job, retains
  the requeue-with-supersede fallback (#270): the old run links to a queued
  copy of the same commit and any deletion obligation completes before the
  replacement is claimed. The copy starts uncredentialed and re-earns delivery
  through normal admission. A replacement inherits the stranded run's
  `admission_sequence`, retaining its FIFO place while receiving a fresh
  `queue_sequence` for identity and unchanged history paging (#1092).
  Argo tag pipelines require the durable release flag; a historical tag Job
  without that capability keeps its conservative fallback and never gains
  release/WIF authority from its ref name. A missing promotion/tag/release
  Workflow, or one that never acquired a Job name, is interrupted without
  automatic re-fire.
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
  run/publication/promotion owners permit terminal cleanup. Explicit cancellation and newer-push
  supersession still delete the exact owned Workflow, and terminal runs are
  never adopted or automatically re-fired.
  Upgrade note: the first upgrade to a release with adoption is still
  disruptive, because the old process acts on its own SIGTERM: it re-queues
  its running branch runs, and a process without promotion/tag/release
  adoption deletes those Workflows and records them as interrupted
  ("scheduler stopped"). Adoption applies from the next restart onward.
- Green branch runs force-sync that same branch to the upstream forge. Red
  default-branch and scheduled branch runs create or update one open CI issue
  per repository and branch with the new SHA, a bounded (16 KiB) failure excerpt,
  and full burn-log command hint; green closes it. Ordinary feature-branch runs,
  promotions, and tag/release runs do not register CI issue work. Projection
  never acquires, renews, or deletes issue locks; explicit coordination locks
  remain available. An explicitly cancelled run (or promotion) projects no CI
  issue and leaves an existing one unchanged. Transient kubelet evictions,
  including node memory/storage pressure and executor scratch eviction, also
  leave CI issues unchanged. Scheduler-side infrastructure failures (checkout,
  concurrency-group or size-grant refusal) still open/update default-branch and
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
- A reachable tag runs the release burn with the release-only cache and
  store-sourced credentials; an unreachable tag receives no release credentials
  and is not synced.
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
  ServiceAccount, a non-root container security baseline, the source mount, the
  per-repository build cache mount and its `GOMODCACHE`/`GOCACHE`, the
  `OBERTH_REPO`/`REF`/`SHA`/`TRIGGER` environment, the active deadline and the
  TTL. A document's own `serviceAccountName` is never honoured — Vault validates
  identity, not intent, so a branch push that could name the release identity
  would defeat the tier gate before OpenBao ever saw it.
- The presence of declared secret-store paths selects the identity, not the
  trigger type. A pipeline that declares no secret paths — whether CI or
  release — binds to the pipeline ServiceAccount and runs with no
  ServiceAccount token, so an in-Pod store login cannot even be attempted.
  A pipeline that declares approved paths binds to the credentialed
  ServiceAccount, which is the identity the OpenBao Kubernetes-auth role is
  bound to by exact `(namespace, ServiceAccount)` pair. The CI system-path
  prohibition (branch pipelines may not declare system-namespace paths) and
  the approval-table grant check are enforced as defense in depth,
  independently of the identity switch.
- Admission refuses everything that would reach the node or the cluster around
  the envelope: `podSpecPatch`, `hostNetwork`, `hostAliases`, `hostPath`,
  `nodeSelector`, `affinity`, `hostPort`/`hostIP`, `workflowTemplateRef`,
  `imagePullSecrets`, Secret or ConfigMap references in `env`/`envFrom`,
  repository-chosen object names, repository-set Pod or container security
  contexts, and any `claimName`. Documents are decoded strictly, and exactly one
  YAML document per file is accepted.
- Admission enforces administrator-owned resource ceilings before submission:
  per-container CPU (8), memory (16Gi), ephemeral-storage (32Gi); retry limit
  (10); DAG tasks (64), step invocations (64); withItems/withSequence
  cardinality (100); workflow/template parallelism (32); memory-backed emptyDir
  sizeLimit (8Gi, required when medium is Memory); PVC capacity (64Gi). GPU and
  custom resource types are not admitted.
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
  a budget; configure retention before relying on disk-floor recovery.
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
  the admission floor, and last sweep/error under `storage`; scoped status
  omits installation-wide storage information.
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
- In-pod upstream/uplink administration has no bootstrap exception: schema,
  token, registration, and upstream-identity Secret mutations each pass the
  live daemon's fail-closed audit gate over its private mode-`0600` Unix socket.
  Host-mode `upstream add` performs zero ungated Secret mutations: a provided
  deploy key is streamed over `kubectl exec` stdin to the in-pod `provide-key`
  handler, which re-validates, checks overwrite semantics, gates via the live
  audit gate, applies via SSA patch with a per-upstream field manager, and
  verifies the readback — the same gated path the in-pod bootstrap uses. The
  sole documented exception is first-run install onboarding
  (`installer/onboard.go applyProvidedDeployKey`): at install time no daemon
  exists yet to gate against.
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
  duplicate-tolerant via `INSERT ... ON CONFLICT(repo, step, secret) WHERE
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
  canonical repository, step and secret tuple; `step="*"` selects only that
  wildcard grant, not its named-step neighbors. A missing tuple is refused
  without mutation. Repository-name canonicalization is unchanged.
- Grant revocation is immediately effective for Oberth admission (the sqlite
  approval table is updated atomically with ConfigMap reconciliation), but the
  Vault credentialed policy retains the exact-path read entry until a policy
  re-sync. Per-step grants (issue #623) scope a secret path to a named Argo
  template: `access_allow repo=X step=release-publish-images
  secret=oberth/data/release/gar-image-key` restricts the path to templates
  named `release-publish-images`; `step="*"` is the backward-compatible
  wildcard granting access to all templates. Enforcement is at admission:
  `admitSecretstoreExecPaths` checks each template's `--path` arguments
  against the step grants before any Workflow is submitted. A path with only
  named-step grants is refused when consumed by an inline, unnamed, or
  fragment-renamed template (fail-closed: the name is not stable or
  authoritative).
- **Per-step Vault identities (issue #623):** for each (repo, step) with at
  least one named-step grant, the installer/sync renders a dedicated
  ServiceAccount `oberth-step-<readable>-<hash12>` in the pipeline namespace,
  a Kubernetes-auth role bound to exactly that SA + namespace + the existing
  audience/TTL conventions, and a policy with exactly the step's approved
  paths (read) plus `auth/token/revoke-self` plus the upstream org/repo
  wildcard. Wildcard (`*`) grants keep the per-repo identity (backward
  compatible). Steps without any grant keep the grant-free pipeline identity.
  At workflow admission, `applyPerStepIdentities` overrides
  `template.ServiceAccountName` and `OBERTH_VAULT_ROLE` on templates
  consuming named-step-granted paths, after `ForceIdentity` set the per-repo
  SA as the workflow default. The per-step SA's projected token is the only
  token that template's Pod can use for Vault login, so a template can only
  read the paths its per-step policy grants. **The two path sets (issue
  #814):** the per-repo policy carries exactly the repo's wildcard (`*`)
  grants — never a named-step path — because the per-repo SA is the default
  for every template without a named grant; the per-step policy carries the
  step's named grants plus the inherited wildcards. A repo whose grants are
  all named still gets its per-repo identity, with a zero-grant policy. Every
  producer applies the same rule (`installer.appendPerRepoGrant` for the
  `access list --json`, tabwriter, and ConfigMap paths;
  `service.computeSecretStorePlan` for the `secretstore_plan` tool) so the
  plan digest and the sync receipt agree. Consequence: revoking a wildcard
  grant narrows the per-repo policy on the next sync even while a named-step
  grant for the same path remains, and `secretstore_verify` with the
  per-repo identity must be denied on a named-step-only path. `secretstore
  plan` and `secretstore sync` render these per-step roles/policies/SAs
  alongside per-repo identities. The chart renders per-step SAs from the
  `argo.perStepIdentities` values list.
  **Fail-closed admission (finding 2):** when per-step identities are
  configured (the map is non-empty) and a template has a named grant but its
  per-step identity is missing from the server's identity store or has an
  empty SA, admission is DENIED with the message: `secret grant denied: step
  "<s>" has a named grant for path "<p>" but its per-step identity is not
  materialized -- run \`oberth secretstore sync\``. This closes the residual
  where a template would run under the per-repo union identity.
  **Prune safety (finding 1):** `ProducePerStepIdentities` returns a hard
  error when the `kubectl exec ... access list --json` call fails — it never
  returns `nil, nil` on exec failure. `secretstore sync` aborts before any
  Vault write when the per-step producer fails, preventing orphan removal
  from running against an empty desired set. The sync prints each planned
  per-step removal before performing it. A `nil` runner (fresh install, no
  pod) is the only case that returns `nil, nil` — safe because there are no
  grants and no orphans. The installer's `PerStepIdentities` in the chart
  args are only set when the per-repo producer AND the per-step producer
  both succeeded (prune-safety: an empty list from a producer failure never
  reaches the chart or the sync).
  **Fail-closed admission (finding 2):** `applyPerStepIdentities` does NOT
  have a `len(config.PerStepIdentities) == 0` early return. Whenever a
  template consumes a path with a named grant and the snapshot has no entry
  (or an empty SA), admission is DENIED with the message: `secret grant
  denied: step "<s>" has a named grant for path "<p>" but its per-step
  identity is not materialized -- run \`oberth secretstore sync\` then
  \`oberth install --install-secretstore --upgrade\``. This closes the
  residual where a template would run under the per-repo union identity.
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
  (repo, step, path) declarations observed at Workflow admission that have
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
- Chart cluster-scoped objects have explicit enabling values:
  `secretstore.createAuthDelegatorBinding` (with `secretstore.enabled`) creates
  the TokenReview ClusterRoleBinding; `argo.resourceSampler.enabled` creates
  a node metrics ClusterRole and binding scoped to allowlisted node names;
  `platformProtection.enabled` creates three platform PriorityClasses for the
  server, Argo controller and OpenBao. The remaining access Roles are namespaced.
- The server-namespace Role grants only ConfigMap access: collection
  create/get/list/watch and update of `oberth-secret-access` by exact name.
  It grants no Pod, `pods/log` or `pods/exec` access. The separate pipeline
  namespace Role grants `pods/exec` create only for source seeding into
  run-scoped volumes. Pipeline secrets arrive through projected identity
  tokens and envconsul, not through the exec stream. The optional
  `secretstore.createAuthDelegatorBinding` ClusterRoleBinding lets OpenBao
  validate login tokens through TokenReview without a stored reviewer JWT.
  Argo-authored workflows
  may select named plain container/script leaves with the static annotation
  `oberth.ci/nonroot-templates: test-one,test-two`. In this initial mode the
  entire workflow must be uncredentialed, with no templateDefaults, and selected
  leaves cannot add init containers, sidecars, plugins, artifacts or nested Pod
  shapes. The fixed main and Argo init/wait UID/GID is65534, with RuntimeDefault,
  dropALL, no escalation and a read-only root. Repository security contexts and
  Pod patches remain forbidden. One server-owned UID0 initializer, using the
  pinned source-seed image with the same restrictions and no token, prepares
  only named fresh per-Pod EmptyDirs. It cannot mount source, PVCs or host paths.
  No chown or fsGroup change is made. Main remains tokenless; Argo's existing
  executor token remains scoped to its init/wait. Source and shared tool inputs,
  including their executor mirrors, remain read-only; the selected leaf has no
  host cache mount. TMPDIR/GOTMPDIR point to private scratch and Go/tool caches are ephemeral
  within that bounded private Pod volume. Collected artifacts retain their
  separate existing run-owned mount. Actual PostgreSQL startup and Chromium's
  namespace/seccomp sandbox require separate execution proof; this interface
  alone does not establish either test suite's coverage.
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
  a token or secret value). Over verified HTTPS it also creates or validates
  the managed non-exportable Transit key; `--disable-transit` is an explicit
  KV-only development mode and is required for HTTP. `oberth secretstore verify` runs in the pod with
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

### Trivy cache guard (#655)

Admission refuses an Argo DAG with two Trivy leaves that are UNORDERED (no DAG
path between them) and whose `--cache-dir` resolves to the SAME shared volume.
The predicate keys on the resolved shared volume, not the string alone: a
template with `oberth.ci/workspace-mounts: none` or `cache` uses a per-Pod
emptyDir, so two such templates sharing the same `--cache-dir` string are
accepted. A `--cache-dir` inside the persistent cache (`/work/cache`) is shared
on every leaf that keeps that mount (`cache`, `readonly` or no annotation).

Accepted shapes:
- Single Trivy leaf (no pair).
- Two Trivy leaves with an ordering edge (direct or transitive).
- Two unordered Trivy leaves whose caches are private (`workspace-mounts: none`
  or `cache`, no VCT mount at the cache path, and not inside `/work/cache`).

Rejected shape: two unordered Trivy leaves where both have `--cache-dir` at a
path backed by the workspace VCT (when `workspace-mounts` is absent or
`readonly` and a `work` VCT is declared), by the persistent cache (`--cache-dir`
inside `/work/cache` and `workspace-mounts != none`), or by a repo-declared VCT
mount at the same path.

### Disk floor admission (#673)

Opt-in, fail-closed disk-space admission input. When `--admission-disk-floor-bytes`
(chart value `admission.diskFloorBytes`) is positive, admission is held until the
filesystem containing the data root has at least that many free bytes. The check
uses `statfs` (sub-microsecond kernel read, cached 5 s). When held, the scheduling
observation reports the floor and current free bytes. Default 0 = disabled.
No taints, deletions, evictions, or security bypasses.

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

### Go module mirror (#662)

Credential-free Oberth-served Go module mirror for an explicitly configured
private module namespace. The `internal/goproxy` package serves the GOPROXY protocol
(`@latest`, `/@v/list`, `.info`, `.mod`, `.zip`) read-only from Oberth's own git cache
for repositories it already mirrors. Canonical tags and validated pseudo-versions
are supported; discovery (`@latest` and `@v/list`) remains tags-only.

**Trust argument:** the mirror exposes only source Oberth already hosts for the
same trust tier (the git cache is the source of truth for branch and tag pushes
alike). It runs on a ClusterIP Service in the server namespace, reachable only
from Argo pipeline pods via a NetworkPolicy that matches pods carrying the
`oberth.ci/trigger` label (set by applyServerMetadata on every pipeline pod).
It is not on the public/proxied ingress, not on any NodePort, and requires no
auth token — it is read-only source that pipeline pods can already clone
through the Git SSH path if they had credentials (which they do not, by design).

**Canonical zip guarantee (#726):** `.zip` responses are built with
`golang.org/x/mod/zip.CreateFromDir`, producing byte-identical output to what
`go mod download` creates from a direct VCS fetch. This means the `h1:`
dirhash in `go.sum` matches the served zip. The handler extracts the git
archive to a temporary directory, excludes `vendor/` trees during extraction
(matching Go's VCS-layer behavior; `CreateFromDir` does not handle this
itself), and delegates all other exclusion rules to `CreateFromDir`: nested
modules (subdirectories with their own `go.mod`), `.git`/`.hg`/`.svn`/`.bzr`
VCS metadata, symlinks, and deterministic entry ordering and zip metadata.
A non-canonical zip causes Go's checksum verification to fail closed with
`SECURITY ERROR … checksum mismatch` — the correct behavior for a tampered
module, but a false positive when the mirror simply builds the zip differently
than the canonical library.

**Verbatim `.mod` guarantee (#726):** `.mod` responses are the exact `go.mod`
bytes at the tag — no trimming, no normalisation. `ShowFile` uses
`run()` with a raw `bytes.Buffer` instead of `capture()` (which returns
via `tailBuffer.String()` → `bytes.TrimSpace`, correct for SHA/URL/ref
output but destructive for file content). Go's `/go.mod` line in `go.sum`
is `dirhash.Hash1` over the exact file bytes; any whitespace change
(including a stripped trailing newline) produces a different `h1:` hash and
`go mod tidy` / `go mod download` fails closed with `SECURITY ERROR …
checksum mismatch`. Both `.mod` and `.zip` are verified against go.sum
vectors in tests.

**What it never serves:** write operations (non-GET rejected by the handler
before routing); private modules Oberth does not mirror (403); branch
names as versions; any secret, credential, or private key. Pseudo-versions
require exact commit/time and reachability checks. `@latest` selects the highest release tag,
falling back to the highest prerelease when no release exists. Empty lists are
successful; an empty `@latest` result is a terminal 403.

**HTTPS listener:** TLS 1.3 minimum, using a certificate issued from a
chart-generated `oberth-goproxy-ca` (same `genCA`/`genSignedCert` pattern as
the server's own TLS secret). The CA certificate is public material; the
private key stays in the Secret volume, never in pipeline pods.

**CA trust distribution to pipeline pods:** the goproxy CA certificate is
streamed into the source claim at seeding time (like the VaultCACertPEM) and
mounted at `/etc/ssl/certs/oberth-goproxy-ca.pem` via a subPath mount. Go's
`crypto/x509` cert directory scan reads it alongside the runner image's system
CAs — no `SSL_CERT_FILE` override needed, no system CAs lost.

**Namespace authority:** enabling the listener requires explicit
`--argo-goproxy-module-prefix`, `--argo-goproxy-upstream`, and
`--argo-goproxy-organization` values. `--argo-goproxy-repository-prefix` optionally
selects a repository-name prefix (for example `sample-`). The namespace root has
no trailing slash or glob characters. Only catalog repositories owned by that
exact upstream and organization can supply modules; qualified cache names avoid
cross-upstream aliases. Repository `go.mod` contents never grant namespace
ownership. An unenrolled upstream yields an empty mapping while the entire
configured namespace still fails closed. Catalog failures and a registered
upstream with a different organization fail startup.

**Private routing (#764):** classify the entire configured namespace
(including its root) before parsing any request shape. Unknown private modules,
missing module directories, missing tags, malformed requests and unsupported
versions return 403. Backend unavailability returns 503 with `Retry-After: 1`;
artifact-integrity and archive/zip failures return 500.
Pseudo-version commit/time/base/reachability denials return terminal 403 with
`Cache-Control: no-store`. A pinned commit without `go.mod` returns terminal 403
with `module_go_mod_missing`, without retrying. Private failures never return
404 or 410. The dedicated listener passes requests
directly to the handler so a mux cannot redirect a malformed private path into
another namespace. Canonical `.mod` and `.zip` bytes remain unchanged.

**Immutable artifact cache:** the enabled server stores version artifacts
under `<data>/goproxy`. SHA-256-addressed `.info` and verbatim `.mod` publish as
cheap metadata; canonical `.zip` is built lazily from the same pinned source and
published separately. Objects are fsynced before atomic, no-clobber manifests. Keys include the mapped repository, module path and
version. Tagged cold fills verify tag membership and pin one Git object for all
representations; warm tagged requests verify the cached object's size/digest and do
no Git or catalog lookup. Pseudo-versions pin the full commit and validate live
reachability even for warm artifacts, again before writing response headers. Cache hits still require the current handler's
namespace/repository mapping. Private cache data stays on the server PVC;
pipeline mounts and network authority are unchanged. Corrupt entries fail
closed without rebuilding from a possibly moved tag. `list` and `@latest`
remain live queries that filter revoked versions; selected `@latest` info uses
the same pinned metadata and rechecks denial before serving.

New `.info` artifacts include Go `Origin` data: `VCS=git`, `URL` is the cached
repository's configured upstream URL, and `Hash` is the full peeled commit SHA
used for the `.mod` and lazy `.zip`. Tagged versions include
`Ref=refs/tags/<version>`; pseudo-versions omit `Ref`. Annotated tags pin their
commit rather than their tag object, preserving the published timestamp.
`go mod download -json` and `go list -m -json` carry this Origin to verifiers.
Published metadata, Origin and ETags remain immutable across tag/remote changes
and restarts. Older artifacts without Origin remain readable and unchanged;
there is no automatic backfill. Strict verifiers require metadata first
published with Origin, including in Go's own cache. Existing h1 checksums,
SHA-256 integrity checks and no-clobber publication remain in force.

**Nested modules:** `<namespace>/<mapped-repository>/<subdir>` resolves inside
the same mapped repository. The selected directory must have a `go.mod` whose
module directive exactly matches the requested path; the repository need not
have a root `go.mod`. Nested tags use `<subdir>/vX.Y.Z`, and discovery strips
only that module's exact tag prefix. Origin includes `Subdir=<subdir>` and
tagged `Ref=refs/tags/<subdir>/vX.Y.Z`; pseudo-versions still omit Ref. Go `/vN`
module paths use the unversioned directory as the tag prefix/Origin.Subdir and
allow a matching `go.mod` there or in its `vN` child; ambiguous layouts refuse.
The lazy zip uses only the selected module directory, retaining canonical
exclusions and inheriting the repository-root LICENSE when no module-local
LICENSE exists. Cache/revocation keys already include the full module path,
so siblings never share artifacts. A module-path mismatch is terminal 403
(`module_path_mismatch`), without retry or public fallback.

Nested pseudo-versions retain commit/time/default-branch checks and live warm
revalidation. Base tags and opt-in tag-only reachability use that module's own
`<subdir>/` namespace; root and sibling tags cannot authorize it. Unknown
commits remain terminal 403, never fetched or negative-cached. Nested discovery
without matching tags returns an empty list; `@latest` then refuses.

At most two cold version fills execute per handler, with duplicate requests
sharing one fill. Each fill gets two read-only attempts of at most 15 seconds
with 250ms backoff, under a separate 31-second fill context including admission
waits. Waiters have their own request deadlines and cannot cancel shared work.
All repository-lock callers join one fair queue; deadline-aware module readers
can leave it. Git repository-lock and catalog waits honor the fill context; local filesystem
operations and Git process teardown retain their OS/cleanup behavior. Failed
fills are not cached. Go does not automatically retry a terminal 503 or interpret
`Retry-After` as a retry policy; these safe retries happen inside the proxy, not
by replaying pipeline steps. Transient exhaustion is 503 with `Retry-After: 1`; a positively missing pinned
tagged commit is terminal 500 (`pinned_source_missing`) with no internal retry or tag
rebinding. The cache
has no automatic expiry/GC and consumes server data volume space. Tag deletion
does not revoke already pinned bytes; administrator-owned per-version markers
under `revoked/` deny all explicit reads without Git, including after restart.
The local `hack/goproxy-revoke.py` utility creates these durable markers and
warns on unmatched coordinates while permitting pre-emptive denial. The scratch
server cannot run it; use a pinned Python maintenance pod or the node data owner. See
[cache operations](docs/goproxy-availability.md#immutable-version-artifacts).

**Pseudo-version policy:** default-branch ancestry is enabled automatically for
mapped repositories. Administrators can expand it with
`--argo-goproxy-pseudo-non-default-branches=true` or Helm
`argo.goProxy.allowNonDefaultBranchPseudoVersions: true` (default false). The
expanded policy includes public non-default branches and canonical module-compatible
tags already allowed by the proxy; private retention/promotion refs never grant
reachability. The conservative default requires default-branch ancestry even
for tag-only commits. Cached symbolic HEAD determines the default branch; no
branch name is assumed and no requested revision is fetched from a remote.
Default-branch ancestry uses `merge-base --is-ancestor`, without listing refs.
Opt-in branches use a one-ref query; tag-only eligibility uses the existing
16 MiB bounded ref reader with cancellation on overflow.

A pseudo-version needs a unique lowercase 12-hex commit abbreviation, exact
committer timestamp in UTC and a canonical module-compatible version. Noncommit
prefix collisions are ignored; multiple matching commits are refused. Base-derived
forms require the exact ancestor tag and a later commit. Annotated tags are peeled
for that ancestry check; author/tagger timestamps are not pseudo-version times.
A warmed manifest may never change its full source identity. Missing, ambiguous,
unreachable, wrong-time/base or rebound revisions are terminal 403, never negative-cached.
Policy IO failures are 503 and do not fall back to stale successful authority.
Cached bytes and SHA-256 ETags stay immutable, but pseudo-version HTTP responses
use `private, no-cache` to require fresh origin eligibility checks; ordinary Go
clients still retain their own immutable module cache. Explicit administrator
revocation markers retain precedence and return 403. `@latest` and `@v/list`
never include pseudo-versions, even if a Git tag is named like one.

**GOPROXY ordering:** `<mirror>,https://proxy.golang.org|direct`
- Comma after mirror: fall through only on 404/410.
- Public module misses return 404, deliberately reaching the public proxy.
- All private failures stop at the mirror, including pseudo-version denials and
  TLS/transport failures. Backend/policy read failures remain retryable 503.
- Pipe after public proxy: fall through on any error, preserving public direct
  fallback for public modules.

Repository YAML can opt a selected build/release Workflow into mirror-only
resolution with `metadata.annotations["oberth.ci/goproxy"]: mirror-only`.
Every pipeline container, including inline leaves on CI and release tiers,
then receives `GOPROXY=<mirror>` exactly, with no public or direct fallback.
Omission preserves the default chain above. Any other value (including empty)
is refused with `oberth.ci/goproxy must be "mirror-only" when present`; server
admission also refuses mirror-only without a configured mirror. This is a
Workflow-wide declaration sourced only from repository YAML, with no hardcoded
repository exceptions. The mirror serves mapped private modules only; public
cache misses fail in mirror-only mode. Public sumdb policy is unchanged.

**Environment injection:** `GOPROXY`, `GONOPROXY=none` and
`GONOSUMDB=<configured module prefix>` into every pipeline container (CI and release),
replacing document-provided values. Explicit `GONOPROXY=none` prevents ambient
`GOPRIVATE` or `GONOPROXY` from bypassing the mirror; the sumdb exclusion covers
the private root and all descendants. Public module sumdb checks are unaffected.
This is the server-provided resolver configuration; repository code that
explicitly replaces its own environment is not a network isolation boundary.

**Availability (#1204):** only the read-only proxy Service publishes not-ready
pod addresses, so server dependency-readiness failures do not withdraw its sole
healthy listener from running pipelines. API/SSH readiness and all mutation,
namespace, TLS and network-policy gates remain unchanged. All server probes use
explicit timeouts including TLS/scheduling time (five seconds for startup and
readiness, fifteen for liveness). Proxy HTTP failures
carry a fixed `X-Oberth-GoProxy-Error` class and emit bounded, quoted diagnostics
at most once per class per handler per 30 seconds, including the count suppressed
since the preceding emission. Fixed class windows bound limiter memory;
normal public 404 fallthrough remains quiet. Listener/TLS errors are attributed
to goproxy. Live and retained step logs annotate refusals from the configured
mirror. Generated hints count toward both the 32 MiB per-step transformed-output
limit and the aggregate run-log limit; they can advance truncation by their
encoded length. Command verdict rules are unchanged. No whole-step replay,
integrity retry or private fallback is introduced. See
[proxy availability](docs/goproxy-availability.md).

**Deployment:** the chart exposes the same namespace/owner parameters under
`argo.goProxy`. A fresh independent install leaves the private mirror disabled
until the administrator supplies its mapping. An enabled mirror with missing
namespace authority is refused; upgrades must preserve the installed namespace
and owner configuration. TLS identities, CA distribution and NetworkPolicy
remain required, with no public listener or insecure fallback.

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
| Honour a repository-supplied synchronization mutex/semaphore name without trigger/repo scoping | Forbidden |
| Let a repository-declared concurrency group span repositories, or change a run's weight, identity, or credentials | Forbidden |
| Change the `oberth.ci/concurrency-group` annotation name, name grammar, or cross-trigger membership | Breaking |
| Admit a `volumeClaimTemplate` with `dataSource`, `dataSourceRef`, or `selector` | Forbidden |
| Reduce a resource ceiling without verifying all known pipeline documents remain admissible | Breaking |
| Admit a construct that reaches the node (`hostPort`, `hostPath`, `hostNetwork`, `nodeSelector`, `affinity`) or the cluster (`workflowTemplateRef`, `imagePullSecrets`) | Forbidden |
| Bind an OpenBao role to a wildcard ServiceAccount name or namespace, or grant the CI tier a ServiceAccount token | Forbidden |
| Change the RunnerImage declaration shape or widen the default prefix allowlist | Breaking security change |
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
`argo.goProxy.enabled`, `argo.goProxy.port`, and the three `compatibility.*` identity tokens.
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
utility pin. Native Git is the only backend. `hack/test-chart-upgrade.sh` renders the
new chart against the unmodified v0.17.53 values shape, both with and without
new defaults. It runs from branch and release chart gates before publication.

Helm >= 3.14 is checked before a real upgrade. The three compatibility identity
values are read from the installed release with `helm get values --all`,
validated as protocol tokens and pinned explicitly with `--set-string`;
new chart defaults and upgrade overrides must never redefine them. Missing or
unreadable identities refuse the upgrade. Preflight renders the target chart
with stored user values (not the old defaults), the same pins and requested
overrides, so target schema errors and unknown keys refuse before either the
Argo controller or Oberth mutation. Temporary stored-value files are private
and removed on all exits; rendered manifests are not printed by this preflight.
Unknown user keys are reported, not automatically pruned. Manual reconciliation
and downgrades require `--reset-values` with a cleaned explicit values file
matching the target schema and preserving existing compatibility identities;
forward-only database migration constraints still apply.

Install re-runs and restore deliberately retain `--reuse-values`, preserving
old chart defaults. Restore overrides only its database compatibility values.
Every newly added chart key must have a template fallback, exercised by the
historical raw-values gate and actual Helm install/restore upgrade tests.

## Protected generic runtime preparation

Ordinary external execution targets do not authorize protected preflights.
A matching repository target or retained nonlocal run binding refuses before
bundle fetching, reservation or allocation, including other refs and trigger
kinds. Removing the target definition cannot redirect a bound protected run
locally. Existing local protected cleanup retains its original controller.
See [execution-targets.md](docs/execution-targets.md).

The protected runtime defaults to `legacy`. Only the explicit server flag
`--trusted-tests-runtime=generic-capability` (chart `trustedTests.runtime`)
selects generic device authority. Existing grants alone never select it.
The independently approved suite/guest archives and pinned host harness remain
the test roots; the host intent/report wire and all-case verifier are unchanged.
Candidate resource status cannot substitute for a protected receipt.

The server-only `GenericAdmission` binds the original intent, complete operator
capability and positive supplemental group. The forward migration stores it in
`protected_runtime_admissions`, atomically with lifecycle/capacity reservation
and `protected.runtime-admitted`. Both full/incremental audit state and mutation
fences verify its row, digest, timestamp and intent against the chained event.
Immutable guards reject update/delete/replacement; missing or corrupt metadata
refuses instead of selecting the legacy path. Existing schema/audit identities,
historical rows and host JSON remain unchanged.

New admission, Create, receipt acceptance and delivery/publication recheck
current authority. The final bounded resource graph must fit the exact grant.
Cleanup after restart selects the persisted admission, including uncertain
Create without a recorded UID, and reconstructs its original group and grant
solely for owned Workflow/UID-fenced cleanup. Generic startup with current group
zero permits this recovery but refuses new execution, Create and Receipt.
No grant recreation, receipt minting or publication follows from reconstruction.
The closed host leaf, credential isolation, actual Pod/process observation,
cancellation and durable cleanup requirements remain enforced. Activation and
legacy retirement require separately reviewed external runtime qualification;
this source preparation does not provide that evidence.

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
| v8 | v0.13.5 | runs: add credentialed flag | Database restore |
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
| `YYYYMMDDHHMMSS-<slug>-<random>` | Future migrations | Per-file `Purpose` and `Rollback` contract in `internal/store/migrations/`; no shared number allocation | Per-file contract (database restore by default) |

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
external ingress and connectors separately. Retired `watchTunnel` values are
accepted for upgrade compatibility, ignored, and reported in Helm notes.
Before upgrading a release that formerly managed a connector, annotate needed
live resources `helm.sh/resource-policy=keep`, verify their retention and
complete the [external ownership handoff](docs/ownership-handoff.md). The push banner remains an optional, display-only dashboard URL.

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

### Protected controller prerequisite and upgrade (#887)

An explicit `upgrade --argo-controller-profile=nonroot-static-v1` reconciles
only the existing managed Argo profile and normal Oberth release, including an
explicit same-version transition. Its dry-run identifies both effects; actual
execution uses the exact chart archive digest, unchanged reused values and
strict before/after readbacks. No automatic rollback or partial success is
allowed. Normal upgrades without the option retain existing behavior.

Upgrade override grammar (#888): each `--set` accepts one canonical dotted key
and scalar string; repeat the flag for multiple assignments. Escaped literal
commas in values retain Helm semantics. Unescaped separators, list syntax and
escaped/noncanonical keys fail before any mutation. Pin, schema and explicit
controller-profile checks use the same grammar. This is a documented subset
of Helm strvals, not a replacement parser.

Controller selection (#889) excludes historical Pod records only after a
complete terminal-incarnation proof: Failed/Succeeded, undeleting fixed
controller shape, explicit false readiness, matching runtime IDs, ordered
nonzero termination timestamps, and coherent prior restart history. Synthetic
or unknown current termination reasons, incomplete/contradictory statuses and
terminating Pods refuse. Exactly one Running candidate must then pass every
existing pinned image, process, mount and immutable configuration predicate.
Historical records are never deleted and confer no authority. This trusts the
Kubernetes API/kubelet's final status; it is not attestation against a malicious
node or administrator. A prior restart with a separately identified final
terminated incarnation is permitted, including an unknown *prior* reason;
that history cannot substitute for evidence of the current incarnation's exit.
Pod names and UIDs must be nonempty and unique. Creation and both incarnation
timestamps are bounded by one observation time captured for the selection;
ordered timestamps wholly in the future cannot establish termination.


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

Server selection during the explicit controller-profile upgrade (#893) applies
an independent strict historical-record check before the unchanged live
Deployment/ReplicaSet/template/version proof. Only the fixed Oberth server plus
one ordinary `prepare-caches` init container is supported. Every declared
container must have a matching final runtime incarnation, coherent timestamps
and prior restart history. The init must have completed successfully before the
server starts; its normal kubelet `Ready=true` is permitted only with
`Started=false`, a terminated state and no restartable-init policy. Additional,
missing, waiting, running, unknown or contradictory lifecycle records refuse.
Exactly one live candidate remains mandatory. This neither deletes history nor
changes the Argo-controller predicate; it trusts API/kubelet status and does not
attest against a malicious node. Terminal record identity and observation-time
checks apply equally to server and init history.

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
clears run-level test attribution, permits eligible branch `run_retry`, and does
not open/update/resolve repository CI-red issues. Argo's server-injected executor
scratch defaults to 1Gi for nonroot leaves and is configurable
through `argo.executorScratchSizeLimit` / `--argo-executor-scratch-size-limit`;
the nonroot verifier enforces the configured bound. These
records trust the Kubernetes API/kubelet and bind observation time separately
from container event times; they are not independent execution attestations.

The cold OpenBao CI leaf fixes compiler parallelism at 1, GOMAXPROCS 2 and a
256 MiB per-process Go memory target within its 3136Mi container limit.
Download, cold compilation and execution have separate 45s/600s/120s bounds;
the 791s outer deadline leaves 26s cleanup margin. Download uses 40s plus 5s
TERM-to-KILL, compilation 595s plus 5s TERM-to-KILL; verification/extraction
are inside the download phase. Race instrumentation, all
`TestPolicyIsolation` cases and the widened-policy negative control remain
required. Compilation uses fresh private caches and verified module downloads;
this is not a network-free test. Direct execution retains the installer package
working directory used by `go test`.

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
`size_grant`, `promote_grant` and `access_allow` grants; existing grants stay
readable and revocable. Schedules skip archived
repositories before reading Git. Git refreshes/probes fail closed, and the
qualified cache is removed under receive/refresh locks. Retry archive if cache
removal failed; the catalog stays archived. Unarchive allows lazy cache recreation.
Dashboard repository lists default to active; the archived filter retains counts
and historical links. `repo_list` includes archive metadata so operators can
inspect the whole catalog. Upstream-level health probes remain shared by active
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

`run_get` and the dashboard run detail expose `Timing`, version 1. These are
observations, independent of the verdict and the older `Run.StartedAt` field.
Missing timestamps are unknown; they never mean a zero duration. Legacy rows
have `unavailable=not-recorded`; no timestamp is backfilled from `StartedAt`.

A run ID identifies one execution attempt. An owner resume keeps it; a restart
re-fire has a new run ID even when it inherits admission_sequence. Known facts
are write-once. Admission parking closes an episode; another request/grant opens
a new numbered episode, preserving previous waits and grants. Queue time is
`queued_at` to `claimed_at`, not queued to the historically overloaded StartedAt.
Checkout completion, admission request and grant, source seed start and readiness,
Workflow creation, and durable terminalization have separate timestamps. Source
seeding is once per Workflow, not once per step. Admission grants are recorded
atomically with MarkRunAdmitted; submission alone cannot infer a grant.

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
by adoption; an admission episode left open by the previous owner stays open.

Migration v30 stacks after #1102's repository-archive migration v29. Shipped
migration bodies and existing ledger rows remain unchanged; older backups
replay v29 and v30 in order on restore.

Controller profile verification failures keep all new admission paused. Unscoped
status exposes the fixed verifier `controller_profile.reason` (never raw API
errors); unchanged failure logs are limited to once per 30 seconds. Recovery
clears the reason and resumes admission without changing existing background tasks.

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

**Host PSI admission:** the configurable serving valve uses hard CPU and IO
`some avg10` thresholds (defaults 50% and 40%) with independent latches and
recovery strictly below each threshold minus ten percentage points. Priority,
release/tag status, idle state and starvation timers cannot bypass hard pressure.
Historical observe/enforce, avg60 and max-defer settings remain accepted and are
ignored by serving admission. Existing/adopted work continues. Missing or invalid
PSI retains the established fail-open behavior and reports its absence; the
workflow ceiling and configured disk floor still apply. Qualify the host scope
and actual thresholds separately. See [host pressure](docs/io-pressure-admission.md).

A currently observed nonzero init-container termination always refuses restart-cut
classification, including when main is still Waiting/PodInitializing. This guard
is local to restart-cut and does not depend on the log-continuity input. The
shared partial-execution evidence contract and its RCA/fallback regression are
compiled in this topic and included in its bundle.


**RCA recovery / run-list UX:** eligible branch failures in the infrastructure or
workflow-level job classes accepted by `run_retry` receive one atomic, audited
retry by default. `--auto-retry-infrastructure=false` / Helm
`autoRetryInfrastructure: false` disables it; the budget is fixed at one.
Trusted-policy-refresh deadlines, lost executors and step-free workflow failures
qualify after terminal observation or cleanup. Missing/corrupt progress history,
any retained failed step (including infrastructure-classified failures),
cancellation, release/tag, promotion, plan, apply, publication, pending deletion
or uncleaned preflight prevent this retry. An execution deadline after any step
started or passed is never retried; pre-step deadlines remain eligible.
Started/passed steps alone do not prevent non-deadline workflow recovery. All retained progress attempts,
engine observations and writer-side newest/protected-work fences apply.
Admission sequence, queued time and priority are inherited for every retry;
the history sequence and run ID are new. The existing reason and `run.retry.auto`
audit record `automatic retry of <id> after infrastructure failure: <reason>`.
The automatic budget survives restart, legacy reasons and `run_rerun` replay
lineage. A manual rerun in that lineage keeps the operator's text after the
server-owned header `rerun of <id> (lineage: automatic retry of <origin>)`.
Only that header determines the inherited budget; operator text cannot change
the origin. Its reason and the lineage retain their separate audit receipts.
Explicit `run_retry` creates a fresh manual request with ordinary
presentation and its own single automatic-retry budget; it never inherits the
automatic reason or lineage. No historical terminal backlog is retried and no
new migration is required.
See [administrator and upgrade policy](docs/infrastructure-retries.md).
Raw failed/interrupted statuses remain; additive retry links and `infra-retried`
(amber) / `superseded` display states explain them. The dashboard's all-status
view shows scoped latest-ref heads plus any missing active runs on every page;
status chips use the flat retained list, so failures in other schedule lanes,
superseded attempts and automatic retries remain findable. Branch/tag history
lives on run detail and includes superseded runs. `latest_per_ref=1` remains
opt-in for HTTP clients.
Readiness uses a 30s background reader budget, retaining green on deadline or
typed SQLite reader contention without renewing the 90s last-success age.
Confirmed writer failures receive no second 90s grace. Hard errors,
startup recovery, liveness, write integrity and admission gates remain enforced.
See [recovery, history and readiness semantics](docs/rca-run-recovery.md).

### Runtime admission capacity

The optional `--max-concurrent-jobs-file` is re-opened every five seconds, so
projected ConfigMap symlink swaps take effect without restart. The boot flag
remains the initial default. Accepted changes are audited before application;
invalid/unreadable values and audit failure retain the last good configuration.
The server counts granted workflows and delegates pod placement to Kubernetes.
Legacy size weights do not constrain this ceiling; one workflow is valid.
Lowering never cancels running work, and raising wakes the queue.
`system_status.admission.configuration` and the dashboard show effective jobs,
source and reload errors; scoped views omit this host-wide configuration.
The chart mounts `<fullname>-admission` read-only as a directory, without
subPath or a pod-template checksum. See [runtime capacity](docs/runtime-admission-capacity.md).

### Administrative admission hold

`admission_hold` (MCP), `POST /api/admission/hold` and `oberth admission hold`
require an unscoped admin and a reason. Allow classes are tag, release and
promotion (default tag+release; empty allows none); a run matching any class
remains eligible under ordinary resource constraints. The hold preserves queue
sequence, priority/FIFO order and existing grants. It skips held claims before
preparation, group and VM ordering so permitted work can reach admission.
Hold settings and audit commit atomically, outside the gate mutex; new grants
are fenced during the bounded write. Restart reloads the original absolute
expiry. Default duration is two hours, hard cap twelve; expiry is evaluated at
admission and reported as inactive, with normal idle dispatch waking within
fifteen seconds. `system_status.admission.hold` shows settings and deadline;
scoped status omits installation configuration. Existing queued reason text
reports the hold; no status or UI layout is added. The hold applies to the
Kubernetes-decides workflow gate, without restoring weight-based admission. See
[admission hold](docs/admission-hold.md).

### Evicted controller replacement and visible admission holds (#1214)

Incomplete kubelet eviction/shutdown controller history is ignored only after exact live
profile verification and a converged controlling ReplicaSet (observed generation,
selector and UID, one desired/current/Ready replica). A predecessor ReplicaSet
must be fully scaled to zero and belong to the same Deployment. Failed/Terminated
history needs the exact kubelet imminent-shutdown message; generic termination
is not eligible for replacement evidence. Uncertain live
or historical authority stays blocked. ReplicaSet API failures retain the
profile-read retry class and five-second per-attempt budget. The chart grants
only namespaced ReplicaSet GET in a new Role, preserving frozen existing rules.
`controller_profile.admission_paused`, reason and remedy are exposed in status;
the dashboard shows a runs-list banner and Status card, refreshing aggregate
status in the background at most every five minutes; direct Status visits remain
fresh. Existing work is not
canceled or re-authorized by profile recovery.

**Report-only size comparison:** outside-step usage rows may carry the observed
Workflow admission label; legacy/ambiguous/conflicting classes remain unknown.
Per-step run views and repo_usage group sampled peaks by S/M/L/XL/unknown with
coverage/kill flags. Labels are not numeric resource budgets. Progress or
termination-message declarations are not authority for these labels. The view
never changes authored requests/limits, weights, grants or execution results;
unknowns and OOM lower bounds cannot authorize smaller resources. Details:
[measured size reports](docs/measured-size-report.md).


**External container measurements (#1216):** schema 37 guards version-2 telemetry
JSON while preserving schema-36 tables and version-1 main rows; v37 also adds
the global observation-age retention index. The opt-in sampler
covers verified Workflow app/init/sidecar/ContainerSet/ephemeral containers,
with Kubernetes lifetime timing and optional kubelet pod
CPU/memory, rootfs/log and complete mounted-PVC usage. `repo_usage` adds separate
container/pod quantiles and per-run overlap without changing main-only size
comparisons or admission. Shared PVC usage is not additive across Pods. Existing 4,096-row/run,
8-KiB/row, bounded read/batch windows remain. Retention targets 90 days/100,000
newest rows globally in oldest-first sweeps
of at most 1,000 rows, with 100-row DELETE batches on a separate SQLite connection
(one-second pass, 25ms busy wait; backlog may exceed targets until swept).
Unchanged terminal rows are not rewritten and sessions suppress up to 4,096
successfully recorded finishes. Helm rollback alone fails schema-too-new and
requires a pre-upgrade database restore; see [schema 37](docs/backup-restore.md).
Full scopes, exclusions and overhead:
[external sampler coverage](docs/step-resource-usage.md#external-sampler-coverage-1216).


### Operator-owned workflow capabilities (#1294)

Resource templates and qualified extended resources require an audited grant
for the durable repository ID and execution namespace. The operator provides
the ServiceAccount, namespaced RBAC, namespace ResourceQuota and any device
capacity. Oberth does not create infrastructure for a particular workload.
Resource admission checks a static, single-object envelope and explicit API
version/kind/action allowlist. Kubernetes discovery must confirm namespaced
scope before execution. Grants cannot combine operator authority with
secret-store or WIF identities. The server fixes the resource executor identity
and rechecks current grant authority before submission, token delivery and
restart adoption. Repository YAML cannot choose or widen it.

Extended-resource requests and limits must agree as positive integers. Their
bounded graph peak is checked against the grant and explicit server capacity;
missing declarations or capacity never fall back to size-based admission.
The source receipt pins exact source, fragments and run binding across sizing
and submission. Ordinary workflows retain the existing admission behavior.

Schema 46 starts capability authority empty. Schema 47 keeps legacy execution,
capacity and KubeVirt grants writable so startup can recover work in flight.
The old `OBERTH_KVM`/`OBERTH_GUEST_TEST`, guest-test and `.oberth/tests.yaml`
paths are deprecated and retained, including publication gates, recovery,
cleanup, existing grants and chart RBAC. Operators must migrate and qualify
replacement pipelines before the later Stage 2 deletion. Schema 45 retires
only the inactive pilot. See [workflow capabilities](docs/workflow-capabilities.md).

## Additive HTTP/MCP compatibility (v0.17.x)

Public typed records retain legacy keys and add snake_case aliases after
redaction. Primary run records/control responses accept or expose `run_id`
alongside their existing ID spelling; conflicting input aliases are refused.
HTTP collection arrays remain the default; `envelope=1` opts into the matching
MCP collection object. `/api/issues` explicitly removes body and internal
list fields for privacy; all other existing names and envelopes remain.
Any removal requires a later major version after an announced deprecation
window of at least 90 days and one stable minor release. No removal date is
announced here. See [the compatibility policy](docs/api-compatibility.md).

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
recheck all sources, merge afresh and run CI. Trusted preflight restart recovery
retains the already planned tree and full train evidence. Every included grant
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
