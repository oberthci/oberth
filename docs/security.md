# Security invariants

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
- Repository code runs only in a Job with no Kubernetes credentials.
- Every Job has CPU, memory, and ephemeral-storage limits; its writable
  `emptyDir` is size-bounded, retained logs have a hard byte ceiling, and the
  shared PVC is provisioned at a fixed capacity.
- CI Jobs have no Secret volumes. Release Secrets and writable caches are wholly
  separate from branch CI.
- Release logs are redacted across streaming write boundaries before reaching
  disk or API responses.
- Runner and server images are immutable digest references. Helm refuses guarded
  placeholder digests.
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
- SQLite accepts only the immutable fresh-schema identity; incompatible or newer
  databases fail before startup recovery.
