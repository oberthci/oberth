# Secret delivery on the Argo execution engine

How a release-tier Argo step reaches a credential, why it works that way, and
what the residual risk is.

> **Current state (v0.13.1+):** Oberth's own `.oberth/release.yaml` no longer
> uses the envconsul chain described below; its credentialed steps run
> `oberth secretstore exec`, which fetches the declared `--path`s and writes
> **each KV field verbatim** to `$OBERTH_SECRETSTORE_DIR/<path-base>/<field>`
> — there is no mapping/renaming layer. `release.sh` therefore reads the
> store's own field names (`GAR_SA_KEY`, `R2_UPLOAD_TOKEN`, `COSIGN_KEY`,
> `COSIGN_PUB`, `SSH_KEY`). The envconsul + `oberth-secret-materialize` chain
> described below is **retired** — `cmd/oberth-secret-materialize` has been
> deleted. The historical rationale below explains why the store fields carry
> env-var-style names. Renaming a store field and updating its readers must
> happen together: v0.13.1's release failed because the exec migration assumed
> file-style field names the store never had.

## The shape

On the Go path Oberth fetches release credentials from OpenBao **server-side at
admission**, before the Job exists, and exec-delivers them into a tmpfs volume.
The server holds the values, so it can mask them in the log stream and fail the
release closed before any Pod is created.

The Argo path cannot do that. A repository-authored Workflow is a set of
independent Pods the Argo controller schedules; there is no single container to
exec into and no admission-time list of which step wants which value. So the
fetch moves into the Pod:

```
envconsul -once \
  -config .oberth/envconsul.hcl \
  -vault-k8s-auth-role-name "$(OBERTH_VAULT_ROLE)" \
  /tmp/oberth-tools/bin/oberth-secret-materialize \
    -dir /run/oberth-secrets \
    R2_UPLOAD_TOKEN=r2-upload-token/token \
    COSIGN_KEY=cosign-secret/cosign.key \
    -- ./.oberth/release.sh publish-r2
```

`envconsul` (HashiCorp, MPL-2.0) logs into Vault with the Pod's own
ServiceAccount token, reads the paths declared in `.oberth/envconsul.hcl`,
injects one environment variable per KV field, and execs the child. It has no
file sink, so there is nothing to misconfigure into writing a secret to disk.

## Why there is a shim in the middle

Release scripts read **files**. `cosign` wants a key path, `gcloud` wants a JSON
key file, and the Kubernetes Job path delivered exactly that shape at
`$OBERTH_SECRETSTORE_DIR/<name>/<key>`. envconsul delivers **variables**. Handing
`release.sh` straight to envconsul fails at the first `test -s "$key_file"`.

There is a second mismatch under the first. envconsul names each variable after
the KV field verbatim, so a field called `cosign.key` becomes a variable called
`cosign.key` — which `execve` accepts, `getenv` reads, and no POSIX shell can
dereference at all (`/bin/sh` answers "Bad substitution" and exits 2). This is
why the KV fields are named `COSIGN_KEY`, `COSIGN_PASSWORD`, `COSIGN_PUB`,
`R2_UPLOAD_TOKEN`, `GAR_SA_KEY` with underscores.

`cmd/oberth-secret-materialize` closes both. It writes each named variable to
`<dir>/<name>/<key>` at mode `0400` under `0700` directories, sets
`OBERTH_SECRETSTORE_DIR`, and execs the script with those variables — and every
`VAULT_`/`BAO_`/`CONSUL_` variable, including the login token — removed from the
environment. The script reads files; it has no use for the credential that
produced them. The mappings live in the pipeline document beside the step that
needs them, so what a step can materialise is reviewable in one place, and
destinations are validated to exactly `<name>/<key>` (no absolute paths, no
traversal, no duplicate destinations) because they come from a repository-authored
document.

`-dir` points at a **memory-backed `emptyDir`** the same template declares:

```yaml
volumes:
- name: oberth-release-secrets
  emptyDir:
    medium: Memory
    sizeLimit: 1Mi
```

Without that volume the shim would still "work" — onto the container's own
writable layer, which is the disk this design exists to avoid.

## The flags are not optional

- **`-once`** — without it envconsul stays resident and watches for rotation,
  sending SIGTERM to the child when a value changes. A mid-build rotation would
  kill the step.
- **`-pristine` is deliberately not passed.** It means "only use values retrieved
  from prefixes and secrets, do not inherit the existing environment variables",
  so the child would lose `PATH`, `HOME`, and `OBERTH_RELEASE_TAG` — the release
  script cannot run without them. An earlier design restored them with an `env {
  pristine = true, custom = [...] }` stanza; **that stanza does not exist in the
  pinned v0.13.2**, which rejects the config outright with `'' has invalid keys:
  env`, so the steps would have failed at config parse before contacting the
  store. Keeping the credential out of the child is the shim's job instead, and
  it does it by name rather than by discarding everything.
- **`-vault-k8s-auth-role-name "$(OBERTH_VAULT_ROLE)"`** — the role is the
  administrator's, injected into every release-tier container by
  `internal/argojob/spec.go` from the trigger's own role flag
  (`--argo-vault-credentialed-role` on release, `--argo-vault-ci-secrets-role`
  on CI), and expanded by the
  kubelet from the container's own environment. A literal in the document would
  drift from the server's configuration silently and surface as an authentication
  failure at publish time.

There is **no `exec` subcommand**. The syntax is `envconsul [FLAGS] COMMAND
[ARGS...]`; the child command is simply the remaining arguments
(`envconsul/cli.go` uses `flags.Args()`). Earlier design notes described
`envconsul -once -pristine exec -- <cmd>`; that form does not exist and would be
interpreted as running a program named `exec`. The `--` in the invocation above
belongs to the **shim**, separating its mappings from the command it wraps.

```hcl
vault {
  # Address comes from VAULT_ADDR, injected server-side.
  k8s_service_account_token_path = "/var/run/secrets/kubernetes.io/serviceaccount/token"
  renew_token                    = false
}

secret {
  no_prefix = true
  path      = "oberth/data/release/cosign-secret"
}
```

## Where envconsul comes from

**Decision: fetched inline by the pipeline's own setup step, sha256-pinned,
into the shared workspace volume.** Not baked into a runner image, and not
injected by `podSpecPatch`.

- *Not a baked image*, because publishing one is itself a release: a new image
  would need building, signing, and pushing before any pipeline could use it,
  and that chicken-and-egg is exactly the sort of thing that turns a release
  into an outage. Oberth's existing pipeline already fetches cosign the same
  way, with the pin in `.oberth/pins/` next to the code that uses it.
- *Not `podSpecPatch`*, because admission refuses it outright — it is a raw
  strategic-merge string applied after Oberth binds the ServiceAccount, so no
  structural check can make it safe.

The trade-off is a network fetch per pipeline rather than a layer in an image.
That is the same trade-off the Go path already makes for cosign, trivy, helm,
and golangci-lint, and it keeps the trust chain visible in the repository.

## What actually enforces the boundary

Not envconsul, and not the declared paths. **The ServiceAccount Oberth forces
server-side, and the Vault role bound to it.**

Vault's Kubernetes auth method validates *identity*, not intent: a role bound
to `(namespace, oberth-argo-credentialed)` will issue a token to any Pod
running as that ServiceAccount, whatever caused the Pod to exist. So the only
thing between a branch push and a release credential is that Oberth — not the
repository's YAML — decides which ServiceAccount the Pod runs as, from the
trigger AND the declared paths.

Four identities, and a role per credentialed tier:

| ServiceAccount | Runs | Kubernetes permissions | Vault role → policy |
|---|---|---|---|
| `oberth-argo-pipeline` | any trigger with no declared paths | none | none |
| `oberth-argo-credentialed` | admitted release tags with approved paths | none | `oberth-argo-credentialed` → upstream subtree + exact approval-table grants (release secrets) |
| `oberth-argo-ci-secrets` | branch pipelines with approved upstream paths | none | `oberth-argo-ci-secrets` → upstream subtree only, never grants |
| `oberth-argo-executor` | Argo's init/wait containers | `workflowtaskresults` create/patch | none |

The two credentialed tiers are separate identities on purpose (issue #200):
admission already refuses a CI document that *declares* a system-namespace
path, but a pod's projected token can attempt any read its bound role's
policy allows, declared or not — and repository-authored code runs in that
pod. Binding CI pods to a ServiceAccount the release role does not accept is
what makes release credentials unreachable from a branch push even then.

Pods without declared paths additionally run with
`automountServiceAccountToken: false`, so the step container holds **no token
at all** and cannot even attempt a Vault login; on the credentialed tiers the
server mounts a ten-minute projected token onto exactly the templates whose
command is a credential chain. Argo's executor still gets one, under the
executor identity, mounted into the executor container only. That is a
second, independent layer below the role binding.

Each Vault role must name its own tier's ServiceAccount and namespace exactly
(`oberth install --install-secretstore` and setup-secretstore.sh write both):

```bash
bao write auth/kubernetes/role/oberth-argo-credentialed \
    bound_service_account_names=oberth-argo-credentialed \
    bound_service_account_namespaces=oberth-argo \
    policies=oberth-argo-credentialed ttl=20m

bao write auth/kubernetes/role/oberth-argo-ci-secrets \
    bound_service_account_names=oberth-argo-ci-secrets \
    bound_service_account_namespaces=oberth-argo \
    policies=oberth-argo-ci-secrets ttl=20m
```

A `*` on either bound field would let any pipeline identity assume the
release-tier role and defeat the whole switch — including the tier split
itself.

## Residual risk, stated plainly

1. **The Pod holds a live Vault credential for its token's TTL, not just the
   one declared secret.** It can read anything its own tier's policy
   allows. This is bounded by short TTLs and by the policy's own scope, not
   eliminated. It is the normal shape of every OIDC- or Kubernetes-federated
   CI system.

2. **Log masking is lost on this tier.** Oberth cannot mask a value it never
   holds. On the Go path a leaked credential is redacted from the run log; here
   it would not be. Mitigated by review, not by machinery.

3. **The `oberth.ci/secret-paths` annotation is auditable intent, not
   enforcement.** Oberth records it in the audit chain at submission so the
   intended and the granted sets can be compared, but the authoritative
   boundary is the Vault policy. A step could read a path it did not declare if
   the policy allows it.

4. **Audit is split.** Oberth's chain records the `(run, trigger, namespace,
   ServiceAccount)` binding at submission; OpenBao's own audit device records
   the access. Joining the two is what makes an access attributable to a push —
   and OpenBao ships with no audit device enabled, so it must be turned on
   explicitly.

The Go DSL path that fetched credentials server-side and masked them in the log
stream no longer exists, so these are not trade-offs against an alternative —
they are the properties of the only path. Points 1 and 2 in particular are what
a release-tier reviewer is accepting when they approve a change to
`.oberth/release.yaml`.

## Phase 0: Oberth-native credential chain (`oberth secretstore exec`)

`oberth secretstore exec` replaces the envconsul + oberth-secret-materialize
shim with a single Oberth binary that does the entire credential chain:

```
/run/oberth/bin/oberth secretstore exec \
    --dir=/run/oberth-secrets \
    --path=oberth/data/release/r2-upload-token \
    --path=oberth/data/release/cosign-secret \
    -- ./.oberth/release.sh publish
```

### What changes

1. **Binary delivery.** The server streams its own executable into the run's
   source claim during seeding (`fillServerBinary`), verified by SHA-256
   readback. Pipeline containers mount it read-only at `/run/oberth/bin/oberth`.

2. **Full credential chain in one process.** `oberth secretstore exec`:
   - Reads `VAULT_ADDR`, `OBERTH_VAULT_ROLE`, and optionally `VAULT_CACERT`
     from the server-injected environment.
   - Authenticates with the Pod's projected ServiceAccount token via the
     `internal/secretstore.Client`, which handles login, KV fetch, and
     token revocation internally.
   - Validates the `--dir` mount is tmpfs (`TMPFS_MAGIC = 0x01021994`).
   - Writes `<dir>/<last-path-segment>/<key>` at mode 0400 with O_EXCL.
   - Strips all `VAULT_*`, `BAO_*`, `CONSUL_*`, and `OBERTH_VAULT_ROLE`
     from the child environment.
   - Sets `OBERTH_SECRETSTORE_DIR` to the materialised directory.
   - Wraps the child's stdout/stderr in `internal/redact.NewWriter`.
   - Propagates the child's exit code verbatim.

3. **Server-side admission gate.** `Build` walks every template that uses
   `oberth secretstore exec` and verifies each `--path` argument appears in
   the workflow's `oberth.ci/secret-paths` annotation. A path not in the
   annotation is refused at submission.

4. **Server-injected volumes.** For credentialed workflows, the server
   injects a memory-backed emptyDir (`oberth-secrets`, 16 MiB, mounted at
   `/run/oberth-secrets`) into credential chain templates, so component repos
   do not declare it themselves.

5. **Transitional mode.** `oberth secretstore materialize` absorbs the old
   `cmd/oberth-secret-materialize` shim with the same interface: it reads
   secrets from environment variables (set by envconsul), writes them to
   files, and execs the child with redaction. This lets pipelines migrate
   incrementally.

### What this closes (residual risks from above)

- **Point 2 (log masking).** `oberth secretstore exec` pipes the child's
  stdout and stderr through `internal/redact.NewWriter`, keyed on all fetched
  values >= 8 bytes. Log masking is restored for the native chain.

- **Admission path visibility.** The `--path` arguments are statically
  checkable at submission. The approval table and the per-step `--path` flags
  together make the intended and the granted sets comparable without OpenBao
  audit logs.

### What remains

- **Point 1 (live Vault credential).** The Pod still holds a Vault token for
  the fetch window. This is bounded by the client's immediate revocation
  (`FetchKV` logs out in its defer) rather than envconsul's TTL-expiry model.

- **Point 3 (intent vs. enforcement).** The annotation is still auditable
  intent, not enforcement. The vault policy is still the authority. But the
  admission gate now refuses an exec invocation whose `--path` is not in the
  annotation, which makes undeclared reads a submission failure rather than
  an audit finding.

- **Point 4 (split audit).** Still applies. Two audit streams must still be
  joined to attribute a read to a push.
