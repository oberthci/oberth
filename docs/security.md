# Security invariants

- Audit mutation gates hash and verify a fresh read-only WAL snapshot before
  timestamp verification and Kubernetes continuity reconciliation. External
  calls run after the read transaction ends and hold no SQLite writer
  reservation. A short `BEGIN IMMEDIATE` transaction then accepts the result
  only if the audit head (ID and SHA-256) and the same reader connection's
  `PRAGMA data_version` are unchanged. The reservation excludes other writers
  through the gate commit. The version check also catches changes to older
  audit rows, pending publications, or receipts that leave the head unchanged.
  If the counter changes while the head is unchanged, the gate ends the writer
  transaction and re-verifies a fresh snapshot on the same reader connection.
  At most two extra local rounds are allowed; they must produce the same head
  and byte-identical checkpoint supplied to external verification. External
  calls run once. Head movement, changed checkpoints, tampering, cancellation
  and exhausted retries still refuse mutation. The total gate deadline is
  30 seconds by default, including waiting for refresh; a shorter caller deadline
  applies. Every live reader uses the Store's busy timeout and query_only,
  with its DSN resolved at startup so opening a reader does not queue on the
  primary connection. Local and timestamp-only gates hash the full chain,
  including its prefix. Their verified-prefix pin is scoped to the current
  verification cycle; a new cycle may establish a new pin.

- Pipeline leaves can opt out of their pipeline token and automatic shared
  workspace access even inside workflows with secret-consuming steps. Explicit false token
  requests, read-only mount aliases and executor mirrors are preserved through
  controller Pod construction. These controls grant no new identity authority;
  see [workspace isolation](workspace-isolation.md) for supported shapes,
  defaults restrictions and controller-proof limits.

- Authored Pod and container `securityContext` fields pass through unchanged,
  except Go admission still refuses AppArmor, SELinux, procMount, sysctls,
  Windows options and beta AppArmor Pod annotations. Kubernetes enforces the
  namespace Pod security floor at Pod creation. Root and writable-root execution
  are allowed; privileged execution, unsafe capabilities, escalation and unsafe
  seccomp are refused. The removed `oberth.ci/nonroot-templates` annotation is
  refused. Authors declare any user, groups and private scratch directly; see
  [authored nonroot execution](nonroot-test-leaves.md).

- Install and upgrade require Argo security defaults even when
  `argo.podPolicy.enabled=false`. They check the live
  controller ConfigMap for `mainContainer.securityContext` and
  `executor.securityContext`: no privilege escalation, drop ALL capabilities,
  only NET_BIND_SERVICE added back, and RuntimeDefault or Localhost seccomp.
  Seccomp may be inherited from `workflowDefaults.spec.securityContext`.
  A non-null `workflowDefaults.spec.templateDefaults.securityContext` replaces
  that Pod context, including when it has no seccomp profile. Valid Pod defaults
  remain unchanged without adding container overrides. Explicit effective
  Pod-level Unconfined or Unconfined in template default container contexts
  refuses the operation, even when other containers select safe profiles.
  A `workflowDefaults.spec.podSpecPatch` (or template default patch) that can
  change security contexts is refused with the native key named; configure
  those defaults directly. Resource-only patches remain allowed.
  Fresh managed installs supply missing defaults. Existing Argo releases require
  approval in interactive security review or `--argo-security-defaults` to fill
  absent native Helm values; operator values are preserved. Without consent the
  command refuses and prints the exact values to set. An external controller
  passes once its live configuration complies, including when installation uses
  `--skip-argo-workflows`. Unreadable live configuration refuses before applying changes. With
  `controller.configMap.create=false`, the live external ConfigMap decides
  Workflow-default compliance; controller and server Pod contexts are still
  checked from their stored Helm values.
  Otherwise the error names the exact ConfigMap keys to configure and
  links the [v0.22.5 migration](releases/v0.22.5.md) for old-profile releases.

- SSH accepts only registered uplink public keys and exactly the two Git smart
  protocol commands. It rejects shells, PTYs, forwarding, traversal, and helper
  protocols. Receive-pack accepts only branch and tag namespaces; Git replacement
  semantics are disabled and replacement refs are removed from retained caches.
- HTTPS uses TLS 1.3 or newer. API and MCP routes require one bearer token whose
  plaintext is never persisted.
- Every push, sync, promotion, and issue mutation records the uplink identity.
- An upstream-scoped uplink (`uplink add --scope`) reaches only its upstreams'
  repositories over SSH, MCP and the JSON API. Out-of-scope records answer as
  missing; lists, counts and pages cover only the scope; installation-wide
  tools are refused. A scope that cannot be read never authenticates as
  unscoped, and a removed upstream narrows a scope rather than widening it.
  Global run and promotion sequences are withheld (paging uses record IDs).
  Issue IDs remain one global counter: accepted, they only hint at the total
  number of issues.
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
  per run (64 MiB by default; `--run-log-limit-bytes`) with a breach failing the run rather than silently
  truncating its evidence, and the shared PVC is provisioned at a fixed
  capacity.
- CI Jobs have no Secret volumes. Release Secrets and writable caches are wholly
  separate from branch CI.
- Pipeline pods run under trigger-selected ServiceAccounts: no granted store
  consumer means the tokenless pipeline identity on every trigger; granted steps
  select the release-login identity or the separate
  ci-secrets identity. The two OpenBao roles bind those exact
  (namespace, ServiceAccount) pairs, the ci-secrets policy covers the
  upstream subtree only and never receives approval-table grants, and only
  the release-tier policy carries exact release-secret grants — so a branch
  push cannot reach release credentials at the Vault layer, independent of
  the admission gate that already rejects a CI document declaring a
  system-namespace path. Secret paths an
  `oberth secretstore exec` invocation would fetch are admission-checked
  against the same declared annotation.
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
- Server-managed images use immutable digest references. Authors may choose
  runner images with tags or digests; pinning a digest prevents a registry writer
  from changing the image between admission and node pull. Oberth has no built-in
  runner image allowlist. An administrator may configure allowed image prefixes;
  an empty setting admits any image. Helm refuses guarded placeholder digests
  for server-managed images.
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
  allowlisted; the cloud metadata endpoint (169.254.169.254) is blocked. k3s includes a
  NetworkPolicy controller and fresh installs keep enforcement enabled there.
  Reruns preserve an operator's stored choice; an explicitly or previously
  disabled policy produces a prominent warning. Clusters must provide an
  enforcing CNI; selecting the Kubernetes distribution alone does not prove
  that an operator has left its controller enabled.
- DNS egress (UDP/TCP 53) targets resolver Pods in
  `networkPolicy.dnsNamespace` (default `kube-system`). For host-networked or
  node-local resolvers, explicitly set `networkPolicy.dnsAnyDestination=true`;
  this restores unrestricted DNS destinations. Configure this before upgrade
  if the resolver is not represented by ordinary Pods in the selected namespace.
- Pipeline ingress allows only Pods in the pipeline namespace and this
  release's server Pod (both its namespace and identity labels must match).
  `networkPolicy.pipelineIngress.enabled=false` explicitly disables this
  listener isolation; `networkPolicy.enabled=false` disables both directions.
  Upgrade manifests apply the new ingress and DNS defaults even when old
  reused values omit these keys. These are additive Kubernetes policies;
  unrelated administrator policies can grant additional access.
- SQLite accepts only the immutable fresh-schema identity; incompatible or newer
  databases fail before startup recovery.

Host pressure admission reads only the fixed Linux `/proc/pressure/{io,cpu,memory}`
files, with bounded parsing and no host mount or extra privilege. The observations
are scheduling hints; malformed/unavailable PSI fails open and never changes
running-workflow authority or execution verdicts. Scoped status, reservation and
run views omit host PSI numbers while retaining a non-numeric pressure cause,
as they omit aggregate capacity. Non-admin and MCP run reasons also omit
pressure percentages and timing details. `/metrics` answers only a metrics-scoped token or an unscoped admin ([metrics](metrics.md)).

## Release signature verification

The plugin bootstrap verifies the release with OpenSSL and a checksum tool;
see [verified plugin bootstrap](plugin-bootstrap.md) for its pinned-key
protocol and manual verification commands. Release CI also verifies the
Sigstore proof and compares the signed manifests before publication.

Each release publishes `SHA256SUMS`, `release.json` and the Helm chart with a
Sigstore bundle (`<file>.sigstore.json`), and signs the server image and the
OCI chart in GAR. All are signed with one pinned key whose `cosign.pub` has
SHA-256 `036dd71c9d3c07a19bff06d4392874014573f695620ac8554d9df175094c4a31`.
From v0.17.96, every signature is also recorded in the public Rekor
transparency log, so the bundles carry an inclusion proof and an RFC 3161
timestamp. The release verifies them without `--insecure-ignore-tlog`, and so
should you (cosign v3, which fetches the Sigstore trusted root over TUF):

```bash
v=0.17.96
base=https://releases.oberth.ci/oberth/v$v
curl -fsSL -O "$base/SHA256SUMS" -O "$base/SHA256SUMS.sigstore.json" \
  -O "$base/cosign.pub" -O "$base/release.json" -O "$base/release.json.sigstore.json"
echo '036dd71c9d3c07a19bff06d4392874014573f695620ac8554d9df175094c4a31  cosign.pub' | sha256sum -c -
cosign verify-blob --key cosign.pub --bundle SHA256SUMS.sigstore.json SHA256SUMS
cosign verify-blob --key cosign.pub --bundle release.json.sigstore.json release.json
sha256sum -c --ignore-missing SHA256SUMS   # after downloading a binary
curl -fsSL -O "https://charts.oberth.ci/oberth-$v.tgz" -O "https://charts.oberth.ci/oberth-$v.tgz.sigstore.json"
cosign verify-blob --key cosign.pub --bundle "oberth-$v.tgz.sigstore.json" "oberth-$v.tgz"
cosign verify --key cosign.pub "$(jq -r .images.server.ref release.json)"
```

Releases up to and including v0.17.95 were signed without a transparency-log
entry. Their signatures remain valid, but only against the pinned key: add
`--insecure-ignore-tlog` to the `cosign` commands above for those versions
only, and expect cosign's warning that the check proves no publication time or
log inclusion. A release after v0.17.95 whose signature fails without that
flag must be treated as unverified. `oberth install`, `oberth upgrade` and
`oberth restore` do not run cosign: they verify the signed record and the
chart offline against the compiled-in key (the transparency-log entry is not
consulted), so upgrading from v0.17.95 is unaffected.

On steps receiving secret-store or cloud-identity mounts, output parameter file
paths must stay outside every volume mount. Argo wait follows symlinks on mounted
paths, so a previous step could otherwise plant a link that copies a credential
into Workflow outputs. Paths are normalized before this check; paths containing
unresolved substitutions are refused. A file written on the step's own root
filesystem remains a supported output parameter. Native output artifacts remain
refused. Argo wait retains its native mirrors, including CA and helper mounts;
the executor-token delivery and read-only source protections remain in place.
