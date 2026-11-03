# Website copy — secret store release secrets (oberth.ci)

Snippet for the "What Oberth does today" section. Written for the website
agent to integrate; tone matches the existing capability bullets.

---

## Release secrets straight from your secret store — never through etcd

Point Oberth at your OpenBao or Vault, and release pipelines get their
credentials the way they should: fetched at release time, delivered directly
into the build's memory, gone when the build ends.

- **Zero etcd, zero disk.** A repository declares the secrets it needs in an
  annotation on its release Workflow YAML. Release steps use `envconsul` to
  fetch credentials from your secret store via the release-tier ServiceAccount.
  They never become Kubernetes Secrets, never land in etcd, never touch a node
  disk (on swapless nodes, the Kubernetes default) — and every value is masked
  in the build log stream.
- **Pre-flight validation, fail-closed.** Before the release job even exists,
  Oberth authenticates to your store and reads every declared path. A missing
  secret, an unreachable store, or a path outside the administrator allowlist
  fails the run immediately with the exact path in the error — not twenty
  minutes into a build.
- **Scoped to the repository that asks.** Secrets live in a hierarchy the
  server enforces at admission: `oberth/upstream/<org>/<secret>` is readable
  by every repository of that org, `oberth/upstream/<org>/<repo>/<secret>`
  only by that exact repository — a repository declaring another repo's path
  fails the release before its job exists. Writing the secret at the scoped
  path *is* the grant; no allowlist bookkeeping. System-level paths remain
  behind an explicit administrator allowlist, branch builds get nothing at
  all, and every fetch is written to Oberth's tamper-evident audit chain,
  attributed to the identity that pushed the tag.
- **One-command setup.** Run `scripts/setup-secretstore.sh` next to your
  OpenBao or Vault — it enables Kubernetes auth, creates a read-only policy
  and a role bound to Oberth's ServiceAccount, and prints the exact Helm
  values. Then `helm upgrade --install oberth`. Done. No AppRole ceremony, no
  static tokens, nothing stored anywhere.

```yaml
# .oberth/release.yaml — declare paths in the annotation
metadata:
  annotations:
    oberth.ci/secret-paths: oberth/upstream/acme/payments/gh-token,oberth/upstream/acme/cosign

# Release steps wrap their command with envconsul:
- name: publish
  container:
    command: ["/tmp/oberth-tools/bin/envconsul"]
    args:
    - "-once"
    - "-pristine"
    - "-config"
    - ".oberth/envconsul.hcl"
    - "./.oberth/release.sh"
    - "publish"
```

`envconsul` authenticates to OpenBao via Kubernetes auth, reads the declared
paths, and injects them into the child process environment. See
[docs/argo-secret-delivery.md](argo-secret-delivery.md) for the full
mechanism and residual risk analysis.
