# Oberth cross-component contract

This document defines the compatibility boundary between Oberth, repository
uplinks, repository-owned pipelines, Kubernetes, and the configured upstream
Git forge. The fresh FAB architecture is the authority when this document and
an implementation detail disagree.

## Required inputs

### Persistent data

- One PVC mounted at `/data` stores `oberth.sqlite`, bare Git caches under
  `/data/git`, immutable run workspaces under `/data/work`, retained logs under
  `/data/logs`, and the accepted-push outbox.
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
  not mint a replacement when one is configured.
- Branch CI Jobs mount no Secret. A release Job mounts only the explicitly
  configured release Secret names after the pushed tag has been peeled and its
  commit proven reachable from the freshly fetched upstream default branch.
- Secret-store-sourced release secrets (OpenBao) never become Kubernetes
  Secrets: the server fetches every repository-declared path at release
  admission with its own ServiceAccount identity (Kubernetes auth against the
  configured `secretstore.*` values), holds the values only in process memory,
  and injects them into the running release Pod's memory-backed volume over
  the exec subresource. The values and value-derived digests never appear in
  etcd, Job annotations, node disk (on swapless nodes, the Kubernetes
  default), or logs; only names and paths are annotated.

### Repository contract

- A repository may provide `.oberth/periapsis.go` with `//go:build ignore`.
- The Job-side runner alone interprets the file. The server never evaluates
  repository-authored Go.
- Pipeline steps receive `OBERTH_REPO`, `OBERTH_REF`, `OBERTH_SHA`, and
  `OBERTH_TRIGGER`, plus the documented Go cache variables.
- A repository may declare secret-store-sourced release credentials with one
  literal `var SecretStoreSecrets = map[string]string{"name": "kv/api/path"}`
  beside `ReleaseSecrets`. The declaration is statically parsed, never
  evaluated. Each path must be in the administrator `secretstore.allowedPaths`
  allowlist; a declared path that is missing, non-allowlisted, or unreachable
  fails the release run before its Job is created. Release steps read the
  delivered keys as `0400` files under
  `$OBERTH_SECRETSTORE_DIR/<name>/<key>` (`/run/secrets/secretstore`, tmpfs);
  the runner refuses to start burns until the delivery manifest verifies and
  fails closed on timeout. Branch burns receive zero secret-store secrets.

## Network and wire outputs

### Git SSH

- TCP NodePort `30022` serves authenticated Git smart protocol.
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
  can be forgotten. Recovery replays unacknowledged events idempotently.

### HTTPS and MCP

- TCP NodePort `30443` serves TLS 1.3 or newer.
- `/healthz` is a liveness endpoint and `/readyz` reports dependency readiness.
  Like a vault before initialization, a freshly installed deployment stays
  `Running` but not ready until its first upstream is registered in-pod;
  registration then also requires the upstream's durable SSH credentials for
  readiness to hold.
- `/mcp` and `/api/*` require a bearer token. Static connection guidance may be
  served unauthenticated without exposing repository or run data.
- A bearer credential maps to exactly one uplink public-key fingerprint and
  identity. Plaintext tokens are displayed once and are never persisted.
- MCP exposes status, bounded named-step logs (`logs <sha> <step>`), wait, sync, promote,
  promote-status, and issue create/get/update/close/delete/list/lock operations.
  This is the complete tool surface. `issue_create` takes only a title and text
  and creates a workspace-global manual issue. Run selectors are resolved across
  repositories without a repository input. `issue_list` takes only an optional
  `before` cursor and returns CI and manual issue IDs/states in fixed pages of 50.

### Runner result

- The Kubernetes termination message is a JSON array of step objects with
  `burn`, `step`, `status`, `exit_code`, `started_at`, and `finished_at` fields.
- Step status is one of `passed`, `failed`, `skipped`, or `timed_out`.
- Retained output is prefixed by burn and step, redacted before disk, indexed by
  byte range, and served only through bounded reads. A run that exceeds the
  fixed 64 MiB retained-log ceiling fails instead of filling the PVC unchecked.
- Job CPU, memory, and ephemeral storage are bounded. `/tmp` uses a size-limited
  `emptyDir`; persistent source state and the split CI/release caches retain the
  fixed PVC and node-path topology required by FAB.

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
- A reachable tag runs the release burn with the release-only cache and Secret
  set; an unreachable tag receives no release credentials and is not synced.
- Every secret-store fetch is recorded in the audit chain as an actor-bound
  intent and outcome (`release.secretstore.fetch.*`) attributed to the uplink
  that pushed the tag, before and after OpenBao is contacted. No audit chain,
  no attributable actor, or a failed intent write means no fetch. Fetched
  values join the streaming redactor exactly like mounted release Secrets.
- Every push, sync, promotion, and issue mutation is attributed to the acting
  uplink in the same durable transaction as the state change where applicable.
- In-pod upstream/uplink administration has no bootstrap exception: schema,
  token, registration, and upstream-identity Secret mutations each pass the
  live daemon's fail-closed audit gate over its private mode-`0600` Unix socket.
- Existing SQLite state and its WAL are inspected through a private read-only
  snapshot at the exact current schema, leaving the source database/WAL/index
  byte-exact, and checked against complete external continuity before writable
  open. The live daemon does not migrate older schemas. Fresh genesis requires
  empty immutable intent/completion histories and is witnessed before any
  listener starts.
- A fresh genesis whose host-key-derived witness identity already has published
  Rekor history fails closed. The one-shot `--accept-witness-chain-reset`
  acknowledgment (`auditAnchor.acceptWitnessChainReset`) unblocks exactly that
  state: it must name the exact UUID of the latest published witness, the
  abandonment is logged loudly, and the acknowledgment is recorded permanently
  as audit action 1 of the new chain, whose first witness commits it. The flag
  never overrides existing local audit history, rollback-external ConfigMap
  continuity, or signed checkpoints, and becomes a no-op after the reset
  completes.
- Kubernetes access is namespace-scoped, with one documented exception: when
  `secretstore.enabled` is set, the chart may create a `system:auth-delegator`
  ClusterRoleBinding for the Oberth ServiceAccount so OpenBao validates login
  tokens via TokenReview without a stored reviewer JWT
  (`secretstore.createAuthDelegatorBinding`), and the namespace Role gains
  `pods/exec` create for the in-memory secret delivery. Job pods receive no
  service-account token, run as UID/GID 65534, have bounded resources and
  deadlines, never retry, and are garbage-collected after completion.
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
  a token or secret value). `oberth secretstore verify` runs in the pod with
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
| Store a secret-store value or value-derived digest in etcd, a Kubernetes object, or on disk | Forbidden |
| Add an optional MCP response field | Review required |
| Reduce an advertised maximum while retaining bounded pagination | Review required |
| Change a NodePort, route, MCP tool name/input, environment variable, mount path, or termination JSON field | Breaking |
| Change token-to-uplink cardinality or accepted Git ref namespaces | Breaking security change |
| Mount any Secret in a branch Job or share/nest CI and release cache roots | Forbidden |
| Interpret repository code in the server process | Forbidden |
| Force-push a promotion target | Forbidden |

Any breaking or security-relevant change must update this file, the Helm chart,
the relevant tests, and the operator-facing documentation in the same change.
