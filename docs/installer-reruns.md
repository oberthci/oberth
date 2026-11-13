# Install and upgrade reruns

A rerun preserves the installed release's settings unless an explicit flag or values file changes them. Release lookup includes pending and failed revisions. A failed lookup or invalid Helm response stops the operation before it can be treated as a new installation.

Before applying an existing Oberth release, the installer renders a server-side Helm preview and prints the sorted paths of changed values. Values themselves are omitted because they can contain credentials. An unreadable live configuration or a preview without its planned configuration stops the operation. Forward repair compares against the last deployed revision, even if the latest revision failed. OpenBao and controller changes follow the preview.

- `--network-policy=auto` chooses a default only for a new release. On a rerun it preserves the live value and reports a disagreement with cluster detection. Use `--network-policy=true` or `false` to change it. If node access fails and the cluster type is unknown, a fresh install requires an explicit choice.
- `service.type` remains unchanged on reruns unless `--service-type` is supplied. Fresh installations default to `NodePort`.
- `promotion.grantMaxLifetime` comes from reused values or an explicit values file (`oberth upgrade --set promotion.grantMaxLifetime=...` also works). The chart and server provide the 24-hour default when the value is absent.
- An explicit `--argo-vault-credentialed-role` wins over the managed default. Existing roles remain in reused values. If an identity producer fails or returns no identities while the live release carries per-repo or per-step identities, provisioning stops rather than reverting to shared identities.
- An existing push banner URL is preserved. Use `--push-banner-url=https://host/runs` to change it, or `--push-banner-url=` to clear it. TLS certificate names alone do not rewrite the live banner.
- An outstanding witness genesis acknowledgement remains in reused values. Clear it explicitly with `--install-rekor --accept-witness-genesis=` after verifying that the server consumed it.

Explicit settings are reconciled even when the installed chart version matches the CLI. Helm and Kubernetes targeting errors are propagated; an unreadable kubeconfig cannot silently select a different Helm target. Database migrations remain forward-only: repair with `oberth upgrade`, not a bare Helm rollback.

## Install reconciles, upgrade moves versions

Since v0.21.0 `oberth install` (with or without `--upgrade`) never changes the chart version of an existing Oberth release. When the CLI is newer than the release it stops before touching any release and points at `oberth upgrade`; when the CLI is older it refuses to downgrade the server, whose newer database the older binary cannot open, and asks for a newer CLI. Same-version reconciliation (a secret store, Rekor, TLS names, Argo limits with `--argo-parallelism`) stays with `install --upgrade`. A local `--chart` is not a version move.

Every Oberth chart the CLI deploys is verified first, the same way `oberth upgrade` does in step 2 below: a fresh `oberth install`, a same-version `oberth install --upgrade` (at the installed version), and the Helm upgrade `oberth restore` applies when the backup's compatibility values differ (at the version the release runs). The chart never comes from a Helm repository. Without `--chart`, `oberth install --image` must name the server image the signed record pins; a different image is a development install and needs `--chart`. `--release-dir`, `--release-origin` and `--chart-repository` choose the source on all three commands, and a development build (no release version) needs `--chart`.

Version changes go through one command:

```sh
oberth upgrade                  # from the directory where the backup and rollback record should go
```

When the kubeconfig has more than one context, name the cluster with
`--context`; the upgrade refuses to pick one and lists them. It prints
`Target: <context> (<server>)` before anything else.

It, in order:

1. refuses an installed release newer than the CLI (`--allow-downgrade` accepts one within the same database schema) and a running schema newer than the CLI's;
2. fetches the signed release for the CLI's version (`release.json`, `oberth-<version>.tgz` and their sigstore bundles from `releases.oberth.ci` and `charts.oberth.ci`, or `--release-dir DIR`), verifies both signatures against the release key compiled into the CLI, checks the chart against the record's digest and pins the server image digest the record names; any failure refuses the upgrade (`--chart` is the explicit unverified development override);
3. previews the Helm change server-side, prints changed value paths and reviews the native Argo security defaults; an unmanaged or incompatible Argo release refuses before writes;
4. takes a verified history-only backup (`oberth backup --history-only`, in `--backup-dir`, default the current directory) and writes a rollback record next to it, before the first change; without a backup it refuses unless `--no-backup`. The backup is encrypted to the default backup key; without a terminal that key must exist already (`oberth backup keygen`, then `oberth backup key --stored` once a copy is off the machine), or `--backup-recipient CERT` names a certificate you manage. The value-change list ignores a key whose value is null on one side and absent on the other (Helm treats them alike), so it shows only what changes;
5. fills missing native Argo security values at the installed Argo chart version before upgrading the Oberth release, then waits for rollout and verifies the running version.

To roll back the upgrade that record describes:

```sh
oberth upgrade --rollback ./oberth-upgrade-v0.20.1-to-v0.21.0-<time>.json --dry-run   # the plan
oberth upgrade --rollback ./oberth-upgrade-v0.20.1-to-v0.21.0-<time>.json --yes
```

It refuses another cluster, a release that moved since, and a stale record: each release's deployed revision must be the recorded one (the upgrade failed before it reached that release: no Helm rollback of it, and an Oberth release the upgrade never reached needs no restore), the one the recorded upgrade made, or later ones that `helm history` shows are all rollbacks to the recorded revision (a rollback of this record already ran: no second Helm rollback, but the restore runs again, so a failed restore can be retried). A later `install --upgrade` or upgrade makes the record unusable. `--force-record` overrides this and is dangerous: the Helm rollback then also undoes those later changes, and the restore replaces the database with a backup that predates them (check `helm history` first). It rolls back the Oberth release to its recorded revision and, when the upgrade migrated the database, restores the pre-upgrade backup's history (`oberth restore --only history`), because the previous server cannot open the migrated database. Rolling back to a release older than v0.21.0, which does not read the restored-runs marker, prints a warning: the old server can dispatch restored work again. Keep that server scaled to zero and inspect or cancel restored work before resuming it. Onto v0.21.0 and later, no run of that backup is dispatched again: running ones whose Workflow is gone are interrupted, and queued or waiting ones (which may have run after the upgrade) are cancelled with the reason "restored from a backup; not dispatched again" (see [backup and restore](backup-restore.md)).

Argo helper budgets use the separate controller release's native
`executor.resources`. Managed `oberth install --upgrade` fills this setting only
when absent and reports an existing setting as "stored value kept" in install
review. Partial and empty maps stay unchanged, including values written by an
earlier installer run. The defaults contain only CPU/memory requests; adopting
them for an existing stored map requires an explicit operator edit. Standalone `oberth upgrade`
updates the Oberth release; configure the native Argo setting before using that
path with a server that no longer writes helper resources into Workflow patches.

## Pipeline Pod security

The pipeline namespace admission policy checks every regular, init and ephemeral
container, including Argo's init/wait containers and the Oberth executor-token
waiter. Each container must explicitly disable privilege escalation and drop
`ALL` capabilities. The only capability it may add back is `NET_BIND_SERVICE`,
matching the capability rule in the Kubernetes
[restricted Pod Security standard](https://kubernetes.io/docs/concepts/security/pod-security-standards/#restricted).
Seccomp has two tiers. A pipeline step Pod carries Argo's reserved
`workflows.argoproj.io/workflow` label: every container must use `RuntimeDefault`
or `Localhost`, directly or inherited from the Pod. An unsafe container override
is refused. Other Pods in the namespace may leave seccomp unset; an explicit
`Unconfined` profile at Pod or container level is refused. Authors cannot set or
remove Argo's label through `podMetadata` or template metadata.
Privileged containers, host namespaces and host ports are refused. The existing
exception for repository cache hostPaths beneath the configured roots remains.

Root users and writable root filesystems are allowed. This is Oberth's security
floor; it does not enforce every requirement of the restricted standard.

The installer supplies native Argo `mainContainer.securityContext` and
`executor.securityContext` defaults: `allowPrivilegeEscalation: false`,
`privileged: false`, `capabilities.drop: [ALL]` and
`seccompProfile.type: RuntimeDefault`. It does not set a user, require nonroot,
make the root filesystem read-only or add a capability. Install review shows
current and planned contexts for the main container, executor, controller and
optional server. Controller and server Pod contexts also default seccomp to
`RuntimeDefault`. Fresh managed installs fill missing fields. Reruns require
interactive approval of the displayed security review or
`--argo-security-defaults` before filling missing fields, and preserve all
explicit security values, including nulls and empty lists. An explicit unsafe
value is preserved and causes the resulting Pod to be refused; correct it in
the Argo Helm release before running pipelines. With `--skip-argo-workflows`, the same live checks and security-consent rule apply.

Argo merges authored main container fields over these native defaults, including
script and containerSet containers. Author-provided init containers and sidecars
must supply the floor themselves (or inherit seccomp from the Pod). Authored
unsafe overrides are refused. Admission errors identify the refused requirement
in the Argo node message; the Workflow object itself can be accepted while
Kubernetes refuses its Pod.

The policy evaluates Pod creation and ephemeral-container updates. Metadata
updates to existing Pods remain allowed, so Argo can complete Pods created
before the policy upgrade. Host-process containers are refused. Authored
AppArmor, SELinux, procMount, sysctls and Windows security options remain refused
by Oberth's Go admission. The rest of the native Pod Security baseline will be
enforced by namespace Pod Security in v0.23.0; the CEL policy does not duplicate
those remaining baseline checks.

`oberth install` and `oberth upgrade` inspect the live controller ConfigMap,
following the controller Deployment's `--configmap` argument or native default.
Both main-container and executor defaults must meet the floor. A compliant
controller passes without a security rewrite, including an external controller
or an install using `--skip-argo-workflows`.

For an existing Argo release, missing defaults require approval in interactive
security review or the explicit `--argo-security-defaults` flag. The absent-only
reconcile preserves the installed chart version and unrelated operator values;
it rereads values at apply and verifies the live ConfigMap afterward. Without
consent, without readable live configuration, or when an explicit value prevents
the floor, the command refuses before deploying the policy and prints the exact
values to configure. The same explicit security consent can reconcile an existing release when
`--skip-argo-workflows` is set, without installing a controller or changing other
Argo values. With no Helm release, configure the live controller externally and
rerun. No installer ownership record is created or inferred.

Stored `controller.configMap.create=false` is accepted when the live defaults
comply. When they do not, native Helm values cannot fix that ConfigMap: the
command refuses with the exact ConfigMap keys to configure. An old-profile
release also receives the [v0.22.5 migration](releases/v0.22.5.md) guidance;
removing a key from a small override file with `--reuse-values` does not unset it.

The upgrade dry run makes no changes. A failed reconcile or a live controller
that still lacks the defaults prevents policy deployment. Pod-level Unconfined
seccomp is refused in both tiers even when every container has a safe override.
