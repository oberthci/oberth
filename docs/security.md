# Security invariants

- Pipeline leaves can opt out of their pipeline token and automatic shared
  workspace access even inside credentialed workflows. Explicit false token
  requests, read-only mount aliases and executor mirrors are preserved through
  controller Pod construction. These controls grant no new identity authority;
  see [workspace isolation](workspace-isolation.md) for supported shapes,
  defaults restrictions and controller-proof limits.

- Uncredentialed Argo workflows can select fixed UID/GID65534 ordinary test
  leaves with `oberth.ci/nonroot-templates`. Direct securityContext and Pod
  patch input remains rejected. Main and generated Argo init/wait retain
  RuntimeDefault, dropALL, read-only root and no privilege escalation. A fixed
  server initializer uses UID0 only to prepare fresh per-Pod scratch directories;
  it has no token, PVC or host mount and never changes shared ownership. Selected
  leaves consume source/tools read-only and use bounded ephemeral `/tmp` caches.
  This is separate from trusted VM execution. Browser sandbox compatibility
  still needs proof under the actual runtime; no capabilities, unconfined
  seccomp or sandbox-disabling flags are granted by this mode.

- SSH accepts only registered uplink public keys and exactly the two Git smart
  protocol commands. It rejects shells, PTYs, forwarding, traversal, and helper
  protocols. Receive-pack accepts only branch and tag namespaces; Git replacement
  semantics are disabled and replacement refs are removed from retained caches.
- HTTPS uses TLS 1.3 or newer. API and MCP routes require one bearer token whose
  plaintext is never persisted.
- Every push, sync, promotion, and issue mutation records the uplink identity.
- Receive reservations bind that identity before Git mutates a public ref and
  replay under the same identity after a callback failure or restart. Public
  ref grammar is enforced both for SSH receives and upstream materialization;
  release reservations retain the exact admission SHA across default-branch
  renames.
- Bearer credentials remain inactive until their complete one-time output write
  succeeds and authenticate only through one unique credential-to-uplink binding.
- Manual issue reopening uses that authenticated uplink, the existing audit
  mutation gate and active issue lock. Its state transition and audit record are
  atomic; it cannot reopen CI issues or change the run-owned CI projection.
- Repository code runs only in a Job with no Kubernetes credentials.
- Every Job has CPU, memory, and ephemeral-storage limits; its writable
  `emptyDir` is size-bounded, retained logs are bounded per step (32 MiB) and
  per run (64 MiB) with a breach failing the run rather than silently
  truncating its evidence, and the shared PVC is provisioned at a fixed
  capacity.
- CI Jobs have no Secret volumes. Release Secrets and writable caches are wholly
  separate from branch CI.
- Pipeline pods run under trigger-selected ServiceAccounts: no declared secret
  paths means the tokenless pipeline identity on every trigger; declared paths
  bind release runs to the credentialed identity and CI runs to the separate
  ci-secrets identity. The two credentialed OpenBao roles bind those exact
  (namespace, ServiceAccount) pairs, the ci-secrets policy covers the
  upstream subtree only and never receives approval-table grants, and only
  the release-tier policy carries exact release-secret grants — so a branch
  push cannot reach release credentials at the Vault layer, independent of
  the admission gate that already rejects a CI document declaring a
  system-namespace path. Secret paths a repository-authored envconsul
  configuration or `oberth secretstore exec` invocation would fetch are
  admission-checked against the same declared annotation, read from the
  immutable run workspace.
- Release preflight (`oberth secretstore verify --release-tier --repo upstream/org/repo`)
  requests a short-lived token for the same release ServiceAccount a run uses.
  The server's established Role in `argo.namespace` grants `create` on
  `serviceaccounts/token` only for `argo.credentialedServiceAccount`. A separate
  Role and RoleBinding grant the same server identity token requests for the
  exact release identities in `argo.perRepoIdentities`, only when that list is
  nonempty. The installer populates the list alongside the ServiceAccounts;
  apply the chart upgrade after provisioning new identities. Keeping the
  established Role unchanged avoids an upgrade conflict with older field
  managers on its rules list.
  This adds no token permission for CI-tier, executor or unrelated accounts,
  and grants no permissions to the per-repo ServiceAccounts themselves.
- Release credentials are fetched in-Pod by `oberth secretstore exec`, which
  authenticates to OpenBao with the Pod's ServiceAccount, writes each KV field
  to a tmpfs-backed directory, and wraps the child process with
  redact.NewWriter registering base64 (phase-aware), hex, and JSON-escaped
  variants of each value. The Vault token is revoked immediately after fetch.
  Before authentication, unsafe mount roots are refused with numeric root
  UID/GID, effective UID, permission bits, and filesystem kind only. Retrieve
  that diagnostic from the failed step with MCP `run_logs`; it survives Pod
  exit without reading environment variables, directory entries, or secret
  values. UID versus effective UID distinguishes ownership failures, mode
  bits identify setuid/setgid failures, and `fstype=0x1021994` identifies tmpfs.
  Sibling steps that do not invoke the credential chain have their logs copied
  verbatim by the server.
- Runner and server images are immutable digest references. Every runner image
  reference must contain a validated `@sha256:<64 lowercase hex>` digest;
  tag-only references are rejected at admission because a registry writer can
  move a tag between admission and node pull. A human-readable tag may precede
  the digest. Helm refuses guarded placeholder digests.
- Security-backported runner tools are rebuilt from immutable release source,
  expose an Oberth derivative identity, and are accepted only when their patched
  module graph and byte-exact binary digest match the source-owned contract.
- The service account is namespace-scoped to Jobs, pod observation/logs, and
  the explicitly named source/release Secrets. It may create immutable,
  Job-owned release snapshots but cannot read or delete unrelated Secrets.
- Upstream bootstrap uses confirmed Ed25519 and known-host material stored in
  name-scoped Secrets. Its authenticated probe requests only the configured
  repository write command; it does not require an organization-wide mutation.
- Ordinary green branches intentionally force-sync their exact tested result.
  Tags are immutable, and promotion may move only its exact target predecessor.
  A same-ref queued run cannot start until that obligation is terminal.
- Workspace cleanup is allowlisted by durable owner state and never follows an
  arbitrary filesystem scan.
- Pipeline egress is deny-by-default on clusters with a conforming NetworkPolicy
  CNI (Cilium, Calico). DNS, HTTPS (port 443), and the secret store are
  allowlisted; the cloud metadata endpoint (169.254.169.254) is blocked. On k3s
  with the built-in kube-router NetworkPolicy controller, the installer disables
  enforcement automatically and prints a warning, because kube-router's DNAT
  handling causes TCP RST on repeated ClusterIP connections, breaking Argo's
  emissary executor.
- SQLite accepts only the immutable fresh-schema identity; incompatible or newer
  databases fail before startup recovery.

## Private Go module resolution

The enabled Go module mirror routes the complete administrator-configured private namespace
locally. Private missing modules/versions and unsupported requests return 403;
backend failures return 502 or 500. No private failure returns the 404/410
statuses that authorize Go to try another proxy. The listener avoids path
redirects before namespace classification. `@latest` uses canonical tags only,
preferring stable releases; nested modules and branch pseudo-versions remain
unsupported.

Pipeline containers receive `<mirror>,https://proxy.golang.org|direct`,
`GONOPROXY=none`, and `GONOSUMDB=<configured namespace>`. Go's comma separator permits
fallback only on 404/410; the pipe permits it on any error. Public misses
therefore retain public-proxy and direct fallback, while private failures and
mirror transport/TLS failures stop. The explicit no-proxy override prevents an
ambient `GOPRIVATE` from sending private coordinates directly upstream. Public
checksum verification remains enabled. These settings configure the resolver;
they are not an egress sandbox for repository code that replaces its environment.

The namespace prefix and exact upstream/organization owner are explicit trusted
server configuration. Repository names are discovered only within that owner;
repository `go.mod` declarations cannot claim another namespace or disable
public checksum verification. An enabled listener without a complete authority
configuration is rejected. Existing deployments must carry their configured
namespace through upgrades; no domain-name inference or permissive discovery
fallback exists.
