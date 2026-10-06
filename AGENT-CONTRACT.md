# Oberth cross-component contract

This document defines the compatibility boundary between Oberth, repository
uplinks, repository-owned pipelines, Kubernetes, and the configured upstream
Git forge. The server architecture is the authority when this document and an
implementation detail disagree.

## Required inputs

### Persistent data

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
- The chart's `prepare-caches` init container creates the CI cache root
  `/var/cache/oberth/ci` and release cache root `/var/cache/oberth/release` and
  hands them to UID/GID `65534` before the server starts; no manual node
  preparation is required. The two roots must not be equal, nested, aliased, or
  located under a critical system directory.

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

### Kubernetes Secrets

- Optional release federation is disabled unless an administrator supplies
  `--argo-release-wif-config` (`argo.releaseWIF` in the chart). This public-only
  configuration binds exact namespace/canonical repository/per-repo release SA,
  named container/script leaf and image-writer/image-reader/chart-writer role.
  Role pools and targets must differ; CI, shared identities, token opt-outs and
  unnamed/unsupported leaves are denied. A 600-second role-audience JWT and
  nonsecret external_account ADC are projected read-only only into main; helper
  mirrors, ordinary sibling leaves and controller wait receive neither token
  nor credential mount. Existing explicit Vault projections and their separate
  wait protections remain intact. Startup copies freeze the capability maps;
  their public digest joins the immutable spec/audit binding. No provider, IAM,
  release-consumer cutover or key retirement is activated by shipping this
  feature. See [release WIF](docs/release-wif.md) for separate acceptance gates.

- The server reads a persistent SSH host private key and HTTPS certificate/key
  from the names configured in the Helm values. When no existing Secret is
  named, the chart generates each once, reuses it on upgrade, and keeps it
  across uninstall (`helm.sh/resource-policy: keep`). The SSH host key also
  derives the deployment's public audit-witness identity and must never be
  regenerated casually.
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
- Template annotations `oberth.ci/workspace-mounts: none|readonly` and
  `oberth.ci/workspace-env: none` reduce automatic workspace access. `none`
  omits automatic work/cache/artifact mounts and their injected environment,
  preserving explicit non-reserved mounts and the read-only source checkout.
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
  overridden unless workspace environment injection is opted out as above.
  Only these two caches are shared; tool binaries stay on the run's
  own ephemeral volume. When no root is configured for a tier the mount is
  absent and runs simply start cold.
- The server reads YAML statically at submission -- it never executes
  repository code in the server process.
- Two Workflow-level annotations are scheduler declarations, read statically
  from the run's own immutable document (`build.yaml` for branch and promotion
  runs, `release.yaml` for tags) before resource admission.
  `oberth.ci/size: S|M|L|XL` (absent selects `M`) is the admission weight
  (1, 2, 3 of a budget of 2 × `maxConcurrentJobs`; XL runs alone).
  `oberth.ci/concurrency-group: <name>` (#658) makes the run a member of a
  mutual-exclusion group scoped to the declaring repository: at most one run
  of a (repository, group) holds admission at a time, across branch,
  promotion and tag triggers, and members are admitted in claim (FIFO) order.
  The group never changes a run's weight or identity, XL keeps its
  exclusivity, and the same-branch supersede rule is unchanged. A member
  waiting on its group holds no weight and no dispatch slot, so other
  repositories' runs are admitted beside the group; admitted runs never
  exceed `maxConcurrentJobs`, and claimed runs are bounded at twice that. A
  hold ends only when the member's Workflow is terminal or its deletion has
  been accepted (Pods of a deleted Workflow may still be inside their
  termination grace period). Names are 1 to 63 characters of `[a-z0-9-]` and
  are never normalized; anything else fails `oberth validate` and server
  admission, and the scheduler fails that run (phase `infrastructure`) before
  a Workflow exists. Because the group spans trigger tiers by design (a
  Terraform plan must never overlap an apply), a branch-tier member can delay
  a release-tier member of the same repository, never read or alter it, and
  only by the members claimed ahead of it, each bounded by the server's
  Workflow deadline: strictly less than any branch document can already
  impose on every repository by declaring XL. A release run's membership is
  decided by its own reviewed `release.yaml`. The admitted group is recorded on the durable run
  (`runs.concurrency_group`, schema v16, immutable once recorded; Go field
  `ConcurrencyGroup` in `run_get`, `run_list` and `/api/runs`,
  `concurrency_group` in `status` and `wait`) and in the Workflow-submission
  audit details.
- Server-owned helper operations (secret-store delivery, trusted plan
  capture/acknowledge/deliver/load) are exec'd through the same
  `go -C /work/src/.oberth run .` invocation with explicit flags; the SDK's
  `Main` dispatches them before any pipeline evaluation.
- Pipeline steps receive `OBERTH_REPO`, `OBERTH_REF`, `OBERTH_SHA`, and
  `OBERTH_TRIGGER`, plus the documented Go cache variables. The SDK itself is
  configured through `OBERTH_SOURCE_DIR`, `OBERTH_STEP_TIMEOUT`,
  `OBERTH_SECRET_DIR`, `OBERTH_SECRETSTORE_DIR`, and (Apply only)
  `OBERTH_PLAN_DIGEST`/`OBERTH_PLAN_SIZE`.
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
  step metadata inside the Job (rusage markers, per-step telemetry) and does
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
- The same Release DAG publishes the oberth.ci website (#649): after
  `release-verify`, the `release-website` leaf deploys the admitted
  `website/` (Worker `oberth-ci`, custom domains oberth.ci and www.oberth.ci)
  with exactly one credential, `oberth/data/release/cloudflare-oberth-workers-token`
  (field `CLOUDFLARE_API_TOKEN`; Terraform-owned account token scoped to
  Workers Scripts Write plus read-only Zone Read/Workers Routes Read on
  oberth.ci), after its init containers bind the source to the admitted tag
  and install Node (sha256 pins) and wrangler (lockfile sha512, offline,
  scripts disabled) into Pod-private scratch. The tokenless
  `release-website-readback` leaf then requires the public URLs to serve the
  admitted bytes. Both maintained `setup-secretstore.sh` copies therefore
  ship through the signed release: the canonical one embedded in the binary
  and image, the public one on https://oberth.ci/setup-secretstore.sh. Their
  semantic audit preflight blocks must stay byte-identical; the files as a
  whole intentionally differ (the public copy adds the Argo tiers).

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
  (`codeberg/acme/acme-beacon`) matches a bare-spelled
  ConfigMap entry (`acme-beacon`), removing all canonical
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
  responses are bounded to 256 KiB per poll), `/api/repos`, `/api/issues`, and
  `/api/status`. The run views resolve by full run ID or an unambiguous
  hexadecimal prefix of at least 12 characters (the push-feedback ID); responses retain the full
  canonical ID. Ambiguous prefixes are rejected with guidance to use the full
  ID; malformed or shorter prefixes are invalid input. These views never
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
- MCP exposes 31 tools: `status`, bounded named-step `logs`, exact-run
  `run_get`/`run_logs`, `artifacts`/`artifact_get`, `wait`, `sync`, `promote`,
  `promotion_list`, `promote_status`, `publish_retry`, issue
  create/get/get_many/update/close/reopen/delete/list/lock,
  secret-access list/allow/revoke, `repo_list`, `repo_remove`, `run_list`,
  `system_status`, and admin-only `secretstore_plan` (#611),
  `secretstore_sync_receipt` (#712), and `secretstore_verify`.
- MCP `run_get` and `run_logs` accept the same full IDs and unique prefixes
  as the dashboard run views. ID prefix resolution is read-only; scheduler
  and mutation lookups still require exact durable IDs. `run_logs` and `logs`
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
  This is the complete tool surface. `issue_create` takes only a `title` and a
  `body` and creates a workspace-global manual issue. Run selectors are resolved across
  repositories without a repository input. `issue_list` accepts optional `repo`,
  `kind` (`manual` or `ci`), `state` (`open`, `closed`, or `all`), `limit`
  (1..50, default 50), and exclusive `before` issue-ID cursor. Omitted filters
  retain the all-issues default. Filters apply before pagination; records are
  ordered by descending creation ID, not update time. Follow `next_before`
  with the same filters until absent for a complete scan. Repository filtering
  uses stored association: global manual issues have no repository, regardless
  of component names in their titles.
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
- `publish_retry` retries the upstream publication for a run or promotion whose
  burns all passed but whose upstream push failed (#696). Input is exactly one
  durable run ID or promotion ID — never a ref, SHA, or tag name.
  Preconditions (all fail closed with `ErrInvalidInput`): the run/promotion is
  terminal; every burn/step passed; the failure is recorded in the publishing
  phase (`run.phase = "publishing"`); the recorded SHA still resolves in
  Oberth's git cache (re-read, not trusted from the row alone). For tag runs
  the exact tag object SHA recorded at admission is what gets pushed; for
  branch runs the exact SHA (force-sync semantics identical to the original
  publish); for promotions the exact result SHA to the exact target (never
  force). Zero repository code executes. Idempotent: if upstream already has
  the object, succeeds without pushing. Rate-limited by the delivery permit (one
  concurrent publication delivery per scheduler). An audit-chain entry is
  recorded BEFORE the push attempt. On success the run transitions to
  `passed`/`phase=passed` and the CI issue closes; on failure the error is
  stored verbatim (minus anything secret) and the run stays retryable.
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
- The package substrate is a digest-pinned multi-arch index built from this
  repository's Dockerfile; `releaseimage.Repack` removes the substrate's
  executable and re-verifies the required/forbidden path contract per child at
  publish and at verify.
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
- Scheduling queue position is one-based accepted order among all durable
  queued runs, not an ETA or execution order: publication-gated refs can be
  bypassed. Admission position is separately one-based among live weighted
  reservations, with `preparing`, `waiting`, and `admitted` states. Missing
  reservations or Workflows never imply a cause or a scheduler failure. A
  grouped reservation adds optional `group`; while it waits on its group it
  also names either `group_holder` (the run of the same repository holding
  the group) or `group_ahead` (the earlier member queued ahead of it), both
  server run IDs, and the scheduling message names the group and that run.
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
- Per-step declared size and measured resource usage never widen the binding
  array: the runner emits one `oberth-step-rusage {json}` marker line per
  step into the retained run log (`max_rss_bytes`, `user_cpu_ns`,
  `system_cpu_ns`, and optional `declared_size`), and the server enriches the
  durable step record from the final marker at persist time. Marker decoding
  is advisory — a malformed record defaults the step to size `M` with zero
  usage.
- Retained output is prefixed by burn and step, redacted before disk for
  streams the oberth credential chain (exec or materialize) wraps, indexed by
  byte range, and served only through bounded reads. Retained logs are bounded
  per step (32 MiB) and per run (64 MiB), enforced at two points of one write
  path: the engine's step-log replay writer (step-attributed) and the
  scheduler's run-log writer. On breach the run fails with an error naming the
  offending step instead of filling the PVC unchecked. Progress-marker write
  failures are surfaced rather than swallowed: a run whose durable progress
  record is degraded is never reported as green.
- Job CPU, memory, and ephemeral storage are bounded. `/tmp` uses a size-limited
  `emptyDir`; persistent source state and the split CI/release caches retain the
  fixed PVC and node-path topology.

## Behavioral guarantees

- Branch pushes, including pushes to the discovered default branch, enqueue one
  FIFO CI run for the exact accepted commit. A newer push to the same repository
  and branch marks the older run `interrupted`, records which newer run
  superseded it, and creates a durable cancellation obligation for any in-flight
  Kubernetes Job.
- A server stop or restart re-fires stranded ordinary branch runs through
  the same supersede mechanism (issue #270), on all three termination paths:
  graceful-shutdown compensation in the stopping process, owner startup
  recovery for claimed runs without a Job, and startup reconciliation for
  Jobs that cannot report a terminal result. The stranded run is interrupted
  with a link to a fresh queued copy of the same commit, and the stranded
  Job's deletion obligation executes before the replacement can be claimed
  (in the upgrade case, by the successor's startup cancellation pass). Jobs
  that finished while the server was away keep their real result. Branch
  runs that mounted ci-secrets are re-fired like any other branch run — the
  copy starts uncredentialed and re-earns delivery through normal admission.
  Promotion CI, tag/release, and publication-owning runs are never re-fired
  automatically; they terminalize conservatively exactly as before, and a
  branch that already has newer active work keeps only that newest run.
- Green branch runs force-sync that same branch to the upstream forge. Red runs
  create or update one open CI issue per repository and branch with the new SHA,
  a bounded (16 KiB) failure excerpt, and full burn-log command hint; green
  closes it. When the run recorded a failed burn/step, the excerpt comes only
  from that step's own named log slice (what `logs <sha> <step>` returns): its
  tail, preceded by the newest earlier lines of the same slice matching
  `fatal error|panic|FAIL|Error:` when they scrolled out of the tail window.
  Sibling steps' output never substitutes for it; a failed step with no slice
  yields no excerpt. Only a run without a recorded failed step (a Job-level
  failure) excerpts the combined run-log tail.
- Promotion green-gates the candidate. It reuses candidate CI only when the
  fetched target fast-forwards to that exact candidate. Divergent merges and a
  fetched target that already contains the candidate receive target-tree CI.
  The chosen target is pushed without force; a moved target fails the promotion.
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
- Every push, sync, promotion, and issue mutation is attributed to the acting
  uplink in the same durable transaction as the state change where applicable.
- Required VM tests use the closed `.oberth/tests.yaml` version-1 contract:
  profile `ebpf-offline-amd64-v1`, artifact `ebpf/secret_monitor.o`, with no
  repository-selected commands, images, identities, or credentials. The
  scheduler reads the exact tested commit and a freshly fetched private
  upstream default ref before build creation and publication. A candidate
  cannot delete a published requirement. Unreadable policy fails closed,
  including on publication recovery and fast-forward promotion. No protected
  suite executor is activated yet, so a required profile fails explicitly;
  build success, VM shutdown and guest log markers cannot replace test evidence.
- VM lifecycle state uses the audited schema-v13 `vm_executions` journal.
  Reserve immutable run/candidate/suite/image/profile identity before create;
  bind the API-issued UID before treating the object as owned. An ambiguous
  submitted create retains its durable obligation and capacity even after a
  not-found observation. Cleanup requires observed absence of the exact VMI
  and owned launchers. A cleaned lifecycle record is not a passing test result.
- The inactive Beacon transport foundation adds schema-v14
  `vm_suite_executions`, `vm_suite_resources` and one `vm_capacity_slots`
  capacity authority shared with the v13 lifecycle. Its immutable plan binds
  candidate/suite and exact artifact/guest/conductor inputs. Per-resource
  submit intents, API UIDs, one actual conductor container attempt and the
  sole typed restart are audited before they can contribute evidence.
  Credential promotion is refused while a submitted VM or pilot remains
  uncleaned; the reverse ordering refuses VM/pilot submission. Ambiguous
  creates and late owned children retain cleanup and capacity obligations.
  A provisional eight-case result stream must match the actual process exit,
  both host-bound endpoints and complete observed cleanup before it can become
  a protected receipt. No scheduler capability or networked guest/conductor
  execution adapter is enabled. See [trusted VM pilot](docs/trusted-vm-pilot.md)
  for the implemented boundaries and remaining activation requirements.
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
  `repo add`, `uplink add|list|remove`, and `secretstore verify`. Every
  mutation passes the live audit mutation gate and appends an audit action;
  listings are read-only. Push-time repository discovery auto-maps a new name
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
  active row without error. Grant entries may use `*` for step; `*` and glob
  characters (`?`, `[`, `]`) in repo or secret are rejected at parse. The
  namespace Role carries collection `watch` on ConfigMaps for the reconciler
  and `update` scoped by `resourceNames` to exactly
  `oberth-secret-access`; unnamed ConfigMap update, patch, or delete stay
  forbidden so audit-anchor continuity ConfigMaps remain unwritable by the
  server's own identity.
- Grant revocation is immediately effective for Oberth admission (the sqlite
  approval table is updated atomically with ConfigMap reconciliation), but the
  Vault credentialed policy retains the exact-path read entry until a policy
  re-sync. The server's own identity has no Vault policy-write capability, so
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
- Kubernetes access is namespace-scoped, with one documented exception: when
  `secretstore.enabled` is set, the chart may create a `system:auth-delegator`
  ClusterRoleBinding for the Oberth ServiceAccount so OpenBao validates login
  tokens via TokenReview without a stored reviewer JWT
  (`secretstore.createAuthDelegatorBinding`), and the namespace Role gains
  `pods/exec` create for the in-memory secret delivery. Job pods receive no
  service-account token, run as UID/GID 0 with all Linux capabilities dropped
  (`Capabilities.Drop: ["ALL"]`), have bounded resources and deadlines, never
  retry, and are garbage-collected after completion. Argo-authored workflows
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
  host cache mount. TMPDIR/GOTMPDIR are `/tmp` and Go/tool caches are ephemeral
  within that bounded private Pod volume. Collected artifacts retain their
  separate existing run-owned mount. Actual PostgreSQL startup and Chromium's
  namespace/seccomp sandbox require separate execution proof; this interface
  alone does not establish either test suite's coverage.
- Argo-authored workflows may select named plain container/script leaves for
  server-injected KVM device access with the annotation
  `oberth.ci/kvm-templates: leaf-one,leaf-two`. Each named leaf must be a plain
  container or script leaf with `oberth.ci/workspace-mounts: none`,
  `oberth.ci/workspace-env: none`, `automountServiceAccountToken: false`, no
  `templateDefaults`, and must not be credentialed (no secret-paths, no release
  WIF role, not also in `oberth.ci/nonroot-templates`). Repository-declared
  `devices.kubevirt.io/*` resources are refused. `inputs.parameters` are allowed
  (unlike nonroot leaves). When `vm.kvm.enabled` is true on the server, each
  declared leaf receives `devices.kubevirt.io/kvm: "1"` (request == limit) and
  `OBERTH_KVM=1` in its environment. When the switch is false, a workflow
  declaring KVM leaves fails at admission with an infrastructure-class error.
  This is Lane A (coverage acceleration) under the ordinary trust model: same
  pod, same deadline, kubelet exit code. It is not Lane B (trusted-suite
  verdict). The consumer asserts `-accel kvm` only when `OBERTH_KVM=1` is
  present; no TCG fallback is permitted with the env set.
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

### Trivy cache guard (#655)

Admission refuses an Argo DAG with two Trivy leaves that are UNORDERED (no DAG
path between them) and whose `--cache-dir` resolves to the SAME shared volume.
The predicate keys on the resolved shared volume, not the string alone: a
template with `oberth.ci/workspace-mounts: none` uses a per-Pod emptyDir, so
two such templates sharing the same `--cache-dir` string are accepted.

Accepted shapes:
- Single Trivy leaf (no pair).
- Two Trivy leaves with an ordering edge (direct or transitive).
- Two unordered Trivy leaves whose caches are private (`workspace-mounts: none`
  and no VCT mount at the cache path).

Rejected shape: two unordered Trivy leaves where both have `--cache-dir` at a
path backed by the workspace VCT (when `workspace-mounts != none` and a `work`
VCT is declared), or by a repo-declared VCT mount at the same path.

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
for repositories it already mirrors. Tags-only; branch pseudo-versions are
not supported.

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
pseudo-versions (only canonical semver tags compatible with the module path); any
secret, credential, or private key. `@latest` selects the highest release tag,
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
unsupported nested modules, missing tags, malformed requests and unsupported
versions return 403. Cache failures return 502; archive/zip failures return 500.
Private failures never return 404 or 410. The dedicated listener passes requests
directly to the handler so a mux cannot redirect a malformed private path into
another namespace. Canonical `.mod` and `.zip` bytes remain unchanged.

**GOPROXY ordering:** `<mirror>,https://proxy.golang.org|direct`
- Comma after mirror: fall through only on 404/410.
- Public module misses return 404, deliberately reaching the public proxy.
- Private failures stop at the mirror, including TLS/transport failures.
- Pipe after public proxy: fall through on any error, preserving public direct
  fallback. No private lookup reaches that leg through the injected chain.

**Environment injection:** `GOPROXY`, `GONOPROXY=none` and
`GONOSUMDB=<configured module prefix>` into every pipeline container (CI and release),
replacing document-provided values. Explicit `GONOPROXY=none` prevents ambient
`GOPRIVATE` or `GONOPROXY` from bypassing the mirror; the sumdb exclusion covers
the private root and all descendants. Public module sumdb checks are unaffected.
This is the server-provided resolver configuration; repository code that
explicitly replaces its own environment is not a network isolation boundary.

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
installer's own pinned arguments. Three keys are pinned and cannot be
overridden via `--set` or `--values`: `image.ref`, `argo.goProxy.enabled`,
`argo.goProxy.port`. Setting a pinned key (or a parent path like `image`)
returns `ErrPinnedKeyOverride` before any cluster mutation. Values files are
parsed and their leaf keys checked for the same conflicts.

`--set` key paths are validated against the chart's default values hierarchy
(`helm show values`); a key that does not exist in the chart is rejected with
`ErrSchemaValidation`. This is a minimal typo gate, not deep schema
validation — Helm's own `values.schema.json` validation still runs at upgrade
time and fails closed on type mismatches or constraint violations.

The `--dry-run` plan renders from the same `upgradeHelmArgs` builder the real
path uses, so the rendered `helm upgrade` command is always truthful.

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

**Contract test:** `TestEveryMigrationDocumentedInAgentContract` asserts that
every migration version in `internal/store/schema.go` has a corresponding
entry in this table.

### Bounded promotion recovery (#752)

`promotion_list` reads existing append-only admission records without mutation,
Git resolution, publication retry or issue-lock renewal. Exact optional repo,
full lowercase source SHA and current status filters are applied before the
page bound (default 50, max 200). The exclusive admission-sequence cursor
`next_before`/`before` supports older records. Pending admissions remain visible
without a run/result SHA. Typed output carries only public identity, scope,
status and timestamps; it omits internal error text and actor fields. Listing
is not completion or publication evidence.

## Watch connector adoption and upgrade preservation (#671)

`watchTunnel.enabled` is administrator configuration, preserved by dedicated
upgrades; absent means false and a present value must be boolean. Immutable
cloudflared/OpenBao image pins remain protected. OpenBao and HTTPS origin public
trust are distinct inputs. The optional origin certificate ConfigMap and its
read-only connector mount are chart-owned; no private key or token is a value.

Explicit `install --upgrade --skip-argo-workflows --watch-tunnel-adoption-plan`
accepts only the expiring public four-object contract in
[watch connector ownership](docs/watch-tunnel.md). It preflights every exact
UID/RV/prior owner/public spec and target render before metadata-only CAS.
Partial or unknown outcomes retain confirmed ownership and report the bounded
public subset; no automatic retry, rollback, delete, recreation or general Helm
ownership mutation override exists. A separate bounded, nonmutating server preview uses --take-ownership only together with --dry-run=server and --no-hooks; actual upgrade argv never includes it. A fresh independently reviewed plan is required for
forward recovery. Secret-store and other service installation cannot be combined.

Adoption is Linux-only and refuses cluster bootstrap, local chart/server-image
overrides and network-policy changes. Only connector enablement and the two
public trust bundles may be supplied as values; bounded validation precedes
freezing them in a sealed memory file shared by preview and actual upgrade.
Both installer paths pin the reviewed connector images. Every object is checked
again after metadata changes, including objects retained by a forward-repair
plan. Helm preview output is bounded during subprocess capture and failures
are sanitized; token staging logs contain no token-derived digest.

Connector resource labels contain application name and instance exactly once.
The Deployment retains `cloudflared-watch` / `cloudflared-watch-oberth-v2` in
resource labels, its immutable selector and Pod labels. Its ServiceAccount and
public CA ConfigMaps retain the release name as their instance. Shared chart
labels cannot overwrite these identities.

The existing Helm server preview is a target-render check; it does not prove a
subsequent server-side apply is conflict-free. V1 adoption still requires a
deployed revision and does not authorize rewriting legacy field ownership or
accepting a failed revision. V2 implements the reviewed forward recovery and
the explicit post-handoff resume of the original failed72 attempt. Resume
requires a fresh mode-bound plan hashing the prior failed-adoption
plan and the one-row confirmed handoff receipt. It reconstructs the exact
Deployment managedFields subtraction, binds the same UID/new RV and unchanged
pre-effect specs/generation/public metadata, and requires every other object's
RV/managedFields to remain exact. Resume performs force=false SSA qualification
with no metadata patch or write. It verifies the fixed release signing key,
signed successor installer/chart/checksums, original immutable plan/receipt,
four original UIDs and fresh failed72 record UID/RV/history. Its maximum
30-minute public plan binds current full metadata/managedFields/specs and
separate rendered/defaulted targets. The sole permitted relinquishment is the
legacy Update apps/v1 manager's fetch-token.args leaf. Full metadata UID/RV CAS
preserves every other entry and spec; shared/atomic/unknown ownership refuses.
When a resumed plan uses a newer chart, each prior and fresh rendered/defaulted
target must carry the exact `helm.sh/chart` label for its plan's chart version;
that single version-derived label is normalized only for prior/fresh target
continuity. All other target metadata and the observed live metadata remain
fully compared.
All four real Force=false SSA DryRunAll requests precede the CAS and repeat
after it, using the hash-bound Helm4.2.3 manager. Confirmed receipts survive
later failure; uncertain outcomes stop without automatic retry or rollback.
Both the recovery API client and sealed Helm kubeconfig derive from one frozen
validated literal HTTPS credential. Impersonation and caller transport/auth
hooks are refused; credential file reload and ambient proxy input are removed.
The signed running installer binds the kernel executable inode, not a mutable
installation pathname. The CAS index comes from the observed approved array;
handoff replies preserve complete metadata except confirmed RV/fieldset change.
The implementation and [regression catalog](docs/watch-csa-test-catalog.md)
remain source-only and unqualified until separately admitted checks pass.
There is no general failed-release or manager-transfer mode. A coordinated
cutover requires the former writer to be quiescent; later drift is still
detectable and refused. No source packet authorizes a live retry.

V2 also binds the canonical public values digest before sealing and rechecks it
on the sealed bytes before preparation. Only the three validated deployment
protocol identities and four exact module-proxy namespace strings may extend
the v1 connector-only whitelist; no security or enablement override is added.
API patches 1.36.2 and 1.36.3 have separately pinned fixture tools, while each
plan retains exact full API GitVersion equality at every effect boundary.
Only the connector Deployment may retain the public
`deployment.kubernetes.io/revision` annotation (canonical positive signed-64-bit
decimal) and the exact `oberth.ci/source` value
`github.com/oberthci/terraform//k8s/cloudflared-watch`. Their observed values
remain bound by the complete metadata comparison and CAS; admitting these keys
does not authorize changing or dropping them during the ownership handoff.
