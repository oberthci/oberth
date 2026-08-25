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

### Kubernetes Secrets

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
  trigger's own credentialed identity: release runs to the credentialed
  ServiceAccount — the only identity whose OpenBao role's policy carries
  approval-table release grants — and CI runs to the separate ci-secrets
  ServiceAccount, whose role's policy covers the upstream subtree only and
  never receives grants. The release role binds its exact ServiceAccount
  name, so release credentials are unreachable from a branch push at the
  Vault layer, not only at admission. Two credential chains are supported:
  - **Native (preferred):** `oberth secretstore exec` authenticates to
    OpenBao in-Pod using the ServiceAccount's projected token, fetches the
    declared paths, validates the `--dir` mount is tmpfs, writes files at
    `$OBERTH_SECRETSTORE_DIR/<name>/<key>` (`0400`, memory-backed emptyDir),
    strips credential env vars, and wraps the child's streams in redaction.
    The server delivers the oberth binary into the source claim at seeding
    time and injects the secrets emptyDir volume automatically.
  - **Transitional:** `oberth secretstore materialize` absorbs the old
    `oberth-secret-materialize` shim, reading from envconsul-set env vars.
    `envconsul` remains the legacy path. Both chains produce the same
    `$OBERTH_SECRETSTORE_DIR` file layout and the same env stripping.
  Values and value-derived digests never appear in etcd, Job annotations,
  node disk (on swapless nodes, the Kubernetes default), or server-side
  logs; only names and paths are annotated.
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
  paths binds to the credentialed ServiceAccount, and a CI pipeline with
  approved paths binds to the ci-secrets ServiceAccount — each carrying a
  projected token that only its own tier's OpenBao role accepts, with the
  trigger's role injected as `OBERTH_VAULT_ROLE`. The CI system-path
  prohibition (branch pipelines may not declare system-namespace paths) and
  the approval-table grant check are enforced as defense in depth on top of
  the identity switch. Secret paths a repository-authored envconsul
  configuration (`secret {}` stanzas in `-config` files, `-secret` flags) or
  an `oberth secretstore exec --path` invocation would fetch are
  admission-checked against the declared annotation, with envconsul
  configuration read from the immutable run workspace; a config file outside
  the source checkout is refused. The YAML must not declare
  `serviceAccountName`.
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
  overridden. Only these two caches are shared; tool binaries stay on the run's
  own ephemeral volume. When no root is configured for a tier the mount is
  absent and runs simply start cold.
- The server reads YAML statically at submission -- it never executes
  repository code in the server process.
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
    reserved and rejected in declarations and in the allowlist.
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
  `/api/status`. The run views resolve by exact run ID and never acquire or
  renew a CI issue lock; the MCP `status`, `wait`, and `issue_get` tools are
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
- MCP exposes 22 tools: `status`, bounded named-step `logs`, exact-run
  `run_get`/`run_logs`, `wait`, `sync`, `promote`, `promote_status`, issue
  create/get/update/close/delete/list/lock, secret-access
  list/allow/revoke, `repo_list`, `repo_remove`, `run_list`, and `system_status`.
- A `status` selector naming an existing cached branch with no recorded run
  returns the ref's repository, branch, and current commit SHA with status
  `no-runs` instead of a not-found error; unknown selectors keep not-found.
  This is the complete tool surface. `issue_create` takes only a `title` and a
  `body` and creates a workspace-global manual issue. Run selectors are resolved across
  repositories without a repository input. `issue_list` takes only an optional
  `before` cursor and returns CI and manual issue IDs/states in fixed pages of 50.

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
- Green branch runs force-sync that same branch to the upstream forge. Red runs
  create or update one open CI issue per repository and branch with the new SHA,
  bounded failure tail, and full burn-log command hint; green closes it.
- Promotion green-gates the candidate. It reuses candidate CI only when the
  fetched target fast-forwards to that exact candidate. Divergent merges and a
  fetched target that already contains the candidate receive target-tree CI.
  The chosen target is pushed without force; a moved target fails the promotion.
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
- Each run mounts one PersistentVolumeClaim: a per-run source claim the server
  creates in the pipeline namespace and fills with the exact pushed revision
  before submission, mounted read-only. A Pod may only mount claims in its own
  namespace, so the server's own workspace volume is never the pipeline's. The
  claim is owned by its Workflow and collected with it; unowned claims past a
  grace window are swept.
- Every secret-store fetch is recorded in the audit chain as an actor-bound
  intent and outcome (`release.secretstore.fetch.*`) attributed to the uplink
  that pushed the tag, before and after OpenBao is contacted. No audit chain,
  no attributable actor, or a failed intent write means no fetch. Fetched
  values are redacted in-Pod by the oberth credential chain (`oberth
  secretstore exec` or `materialize`), which wraps each credentialed step's
  stdout and stderr with redact.NewWriter.
- Every push, sync, promotion, and issue mutation is attributed to the acting
  uplink in the same durable transaction as the state change where applicable.
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
  (fail-closed). Only admin uplinks may call `access_allow` and
  `access_revoke`; non-admin callers receive a clear `ErrForbidden` error
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
- Kubernetes access is namespace-scoped, with one documented exception: when
  `secretstore.enabled` is set, the chart may create a `system:auth-delegator`
  ClusterRoleBinding for the Oberth ServiceAccount so OpenBao validates login
  tokens via TokenReview without a stored reviewer JWT
  (`secretstore.createAuthDelegatorBinding`), and the namespace Role gains
  `pods/exec` create for the in-memory secret delivery. Job pods receive no
  service-account token, run as UID/GID 0 with all Linux capabilities dropped
  (`Capabilities.Drop: ["ALL"]`), have bounded resources and deadlines, never
  retry, and are garbage-collected after completion. Moving to non-root
  UID 65534 is a separate follow-up requiring toolchain and PVC ownership
  validation.
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
| Admit a `volumeClaimTemplate` with `dataSource`, `dataSourceRef`, or `selector` | Forbidden |
| Reduce a resource ceiling without verifying all known pipeline documents remain admissible | Breaking |
| Admit a construct that reaches the node (`hostPort`, `hostPath`, `hostNetwork`, `nodeSelector`, `affinity`) or the cluster (`workflowTemplateRef`, `imagePullSecrets`) | Forbidden |
| Bind an OpenBao role to a wildcard ServiceAccount name or namespace, or grant the CI tier a ServiceAccount token | Forbidden |
| Change the RunnerImage declaration shape or widen the default prefix allowlist | Breaking security change |
| Change token-to-uplink cardinality or accepted Git ref namespaces | Breaking security change |
| Mount any Secret in a branch Job or share/nest CI and release cache roots | Forbidden |
| Execute or evaluate repository code in the server process | Forbidden |
| Force-push a promotion target | Forbidden |

Any breaking or security-relevant change must update this file, the Helm chart,
the relevant tests, and the operator-facing documentation in the same change.
