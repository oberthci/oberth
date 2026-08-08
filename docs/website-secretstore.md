# Website copy — secret store release secrets (oberth.ci)

Snippet for the "What Oberth does today" section. Written for the website
agent to integrate; tone matches the existing capability bullets.

---

## Release secrets straight from your secret store — never through etcd

Point Oberth at your OpenBao or Vault, and release pipelines get their
credentials the way they should: fetched at release time, delivered directly
into the build's memory, gone when the build ends.

- **Zero etcd, zero disk.** A repository declares the secrets it needs in one
  literal map in `periapsis.go`. Oberth's server fetches them from your secret
  store and injects them into the running release job's memory-backed volume.
  They never become Kubernetes Secrets, never land in etcd, never touch a node
  disk (on swapless nodes, the Kubernetes default) — and every value is masked
  in the build log stream.
- **Pre-flight validation, fail-closed.** Before the release job even exists,
  Oberth authenticates to your store and reads every declared path. A missing
  secret, an unreachable store, or a path outside the administrator allowlist
  fails the run immediately with the exact path in the error — not twenty
  minutes into a build.
- **Allowlisted and audited.** Repositories can only request paths the
  administrator explicitly allowlisted, branch builds get nothing at all, and
  every fetch is written to Oberth's tamper-evident audit chain, attributed to
  the identity that pushed the tag.
- **One-command setup.** Run `scripts/setup-secretstore.sh` next to your
  OpenBao or Vault — it enables Kubernetes auth, creates a read-only policy
  and a role bound to Oberth's ServiceAccount, and prints the exact Helm
  values. Then `helm upgrade --install oberth`. Done. No AppRole ceremony, no
  static tokens, nothing stored anywhere.

```go
// .oberth/periapsis.go — that's the whole integration
var SecretStoreSecrets = map[string]string{
        "r2-token": "oberth/data/r2-upload",
}
```

Release steps read the keys as plain files under
`$OBERTH_SECRETSTORE_DIR/r2-token/` — delivered to memory, verified against a
digest manifest before the first step runs.
