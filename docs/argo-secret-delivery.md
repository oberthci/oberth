# Argo secret delivery

Granted steps use the server-injected `/run/oberth/bin/oberth`:

```yaml
command: ["/run/oberth/bin/oberth"]
args:
  - secretstore
  - exec
  - --dir=/run/oberth-secrets
  - --path=oberth/data/release/signing-key
  - --
  - ./.oberth/release.sh
```

Declare each path in `oberth.ci/secret-paths` and grant access in Oberth for
that step in its exact workflow file, repository, and upstream. A trigger or a declaration alone
grants no secret-store settings.

The single definition lives in `GrantedSecretSteps` in
[granted_secret_steps.go](../internal/argojob/granted_secret_steps.go): the main
Container or Script must invoke a recognized store consumer and consume at
least one static path. Every consumed path must be declared, approved for the
selected repository/upstream and exact workflow file, and granted to the stable template name
or `*`. Inline and unnamed templates require wildcard grants. Missing
step grants confer no authority. Token opt-outs receive no store settings.
Admission and Build delivery use this same decision. The grant table is keyed by repository/upstream, exact repository-relative workflow file, template name, and path. The workflow field is required; no default, alias, or wildcard file selector is accepted. `*` grants cover steps only within that exact file. Pending declarations, revocation, policy names, and store identities retain the same file scope.

Only the granted step's container gets the secret-store env, mounts, token and helper; non-secret settings (address, role name, CA path) can be visible in the Pod's template metadata. Oberth does not inject those settings into authored sidecars or init containers or into other steps. Argo retains its native mirrors of main-container mounts in wait. Argo serializes the main template into `ARGO_TEMPLATE`; this metadata carries public settings and mount descriptions, never the projected token bytes or materialised secrets. No per-run settings ConfigMap is created. Oberth sets `automountServiceAccountToken: false` on the Workflow and every template, independently of the live ServiceAccount setting. Granted mains keep their explicit projected login token. Authored token opt-outs are checked before identity normalization.

Branch credentials use a separate role
limited to the upstream subtree; release grants require a release trigger.

The wrapper verifies tmpfs, effective-user ownership and directory permissions
before fetching. It logs in with the projected ServiceAccount token, fetches
KV values, revokes its short-lived token, and writes each value to
`$OBERTH_SECRETSTORE_DIR/<last-path-segment>/<key>` with mode 0400.
It removes `VAULT_*`, `BAO_*`, `CONSUL_*` and `OBERTH_VAULT_ROLE` from the child
environment, redacts fetched values from stdout and stderr, and preserves
the child's exit status. Disable disk-backed swap to keep tmpfs memory-only;
`OBERTH_REQUIRE_SWAPLESS` makes detected swap a hard failure.

The server injects a memory-backed emptyDir for the secret root. Argo retains
its native wait-container mirrors of the secret root, login token, CA and helper.
Every container of a credentialed step (main, sidecars, containerSet members and
init containers) shares that step's trust; output refusals guard Argo's reads
and do not create a boundary between containers of one step.

On steps receiving secret-store or cloud-identity mounts, output parameter file
paths must be absolute and outside all volume mounts. Use an absolute path on
the step's own filesystem. If the step has sidecars or several containerSet
members, path-read output parameters and references to its `outputs.result` are
refused because another container of the Pod can redirect what Argo reads.
Sidecars remain allowed when those output reads are absent.

The environment-variable materialize command is retired.
`oberth secretstore materialize` is no longer accepted. Migrate callers to
`secretstore exec` and read credentials from files. See
[the example release workflow](../examples/go-project/.oberth/release.yaml).

OpenBao policy remains the enforcement boundary for reads. Join the Oberth
admission/fetch evidence with OpenBao audit records when attributing a read;
annotations record intent and do not replace store policy.

The `oberth-argo-` and per-step identity prefixes are reserved for policies and
roles managed by Oberth. Avoid manually provisioning identities under those
prefixes: authoritative sync removes managed names absent from the live grant
table. Sync refuses a ConfigMap fallback or a producer warning before making
changes, because a partial desired set cannot authorize orphan deletion.

Only `oberth secretstore exec` receives the secret-store login token.
Migrate older credential commands to `oberth secretstore exec`.

For an external OpenBao store configured without a reviewer JWT, Kubernetes
TokenReview uses each login JWT. The chart binds `system:auth-delegator` to the
server and the exact release-login, CI-secret, per-repository release/CI and
per-step login ServiceAccounts. Holders of these tokens can call Kubernetes
TokenReview and SubjectAccessReview; this delegation is granted to pipeline login
identities only for external stores. Pipeline and executor accounts receive no such
binding. In-cluster stores keep their local reviewer behavior and the existing
server-only binding. The chart detects `.svc` and `.svc.cluster.local` endpoints;
set `secretstore.external: false` for an in-cluster store behind custom DNS, or
`true` for an external store behind a cluster service alias. An explicit
`argo.vault.address` determines the pipeline store's mode. Operators managing
these bindings themselves can set `secretstore.createAuthDelegatorBinding: false`.

Run records have no `Credentialed` field or column. Admission reloads current
step grants, including on retry and rerun, and requires healthy access
reconciliation for granted consumers. Release triggers alone do not load the
approval table or grant store settings. Step-level credential evidence remains
part of replay safety.

The v0.22.5 migration drops the existing approval table and creates an empty
workflow-scoped table. No approvals are backfilled; old ConfigMap approvals
without workflow scope are removed once by the server start that commits the migration, with the count logged. Later unscoped entries are rejected with an error naming the ConfigMap, entry and edit remedy; they cannot restore approvals. Fresh databases do not print the clearing notice. Audit history is
retained. When the migration commits during an install or upgrade, its output says:

all secret approvals are cleared; re-approve.

For example, approve with `access_allow` arguments
`{"repo":"upstream/org/repo","workflow":".oberth/release.yaml","step":"publish","secret":"oberth/data/release/signing"}`.
Use that exact tuple with `access_revoke`. `access_list` and pending declarations
return the workflow file; `access_dismiss` requires it too. After re-approval,
run `oberth secretstore sync` to prune the old OpenBao roles and policies, then install the scoped identities.

For upstream-scoped paths, the step and workflow-file scope is enforced at admission. The current OpenBao policy still grants the repository upstream subtree; a follow-up narrows that policy.

The optional helper image is published for Linux amd64 and arm64 alongside each
release. It contains only the static Oberth binary at `/usr/local/bin/oberth`
and CA certificates. Select its immutable reference from `images.helper.ref`
in the signed `release.json`; that record also includes the helper digest in
its release-note paragraph. The image uses the same release signing key as the
server image. Use it directly for Oberth commands, or copy its binary into a
step image with the tools that `oberth secretstore exec` must launch; the helper
contains no shell or build tools. It provides no grants, credentials or automatic
Workflow injection.
