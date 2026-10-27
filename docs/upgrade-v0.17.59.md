# Upgrade an existing install to v0.17.59

This runbook covers an existing v0.17.58 install, including its schema v32
history. Review the [release notes](releases/v0.17.59.md) before starting.
There are **no required new values**: CLI delivery remains the default, shadow
mode is off, and existing release files/roles/identities keep their behavior.
An explicit retained go-git selection now refuses unsupported export rules.

Migration v33 rebuilds `step_results` in a startup transaction. Allow database
space for the rebuild and time proportional to retained step history. Do not
use the historical schema-v2 wipe/reinstall procedure: this migration preserves
history. Rollback requires both Helm rollback and a pre-upgrade database restore.

## 1. Prepare and back up

Use the existing namespaces and context throughout. These placeholders must be
replaced with the install's actual names; the Helm release/Deployment is `oberth`.
Keep Helm 3.14+ (required by default-merging upgrades), kubectl, OpenSSL 3, a
working operator kubeconfig, and both old and new CLI binaries available.

```sh
export OBERTH_CONTEXT='your-context'
export OBERTH_NAMESPACE='your-server-namespace'
export OBERTH_ARGO_NAMESPACE='your-pipeline-namespace'
export OBERTH_OPENBAO_NAMESPACE='your-openbao-namespace'
umask 077
helm --kube-context "$OBERTH_CONTEXT" -n "$OBERTH_NAMESPACE" history oberth
helm --kube-context "$OBERTH_CONTEXT" -n "$OBERTH_NAMESPACE" get values oberth -o yaml > pre-v0.17.59-user-values.yaml
helm --kube-context "$OBERTH_CONTEXT" -n "$OBERTH_NAMESPACE" get values oberth --all -o yaml > pre-v0.17.59-all-values.yaml
helm --kube-context "$OBERTH_CONTEXT" -n "$OBERTH_NAMESPACE" get manifest oberth > pre-v0.17.59-manifest.yaml
```

Record the last healthy **pre-upgrade revision**, server image/digest, CLI
version, database schema and current status. Save existing values files, image
mirrors, compatibility identities and credentials securely; values/manifests
may contain sensitive configuration. Helm values are not included in a backup.
Keep the old chart/images available for rollback.

Coordinate a maintenance window: pause external submissions and scheduled work
using the install's normal controls, and drain queued/running work, especially
publish/apply operations. `--wait-idle` is advisory waiting, **not an admission
freeze**. Keep submissions paused until checks finish so rollback need not lose
post-backup runs or abandon already-published external changes.

With the old server still running, take an encrypted backup:

```sh
oberth backup --context "$OBERTH_CONTEXT" --namespace "$OBERTH_NAMESPACE" \
  --argo-namespace "$OBERTH_ARGO_NAMESPACE" --openbao-namespace "$OBERTH_OPENBAO_NAMESPACE" \
  --output pre-v0.17.59.tar.gz.enc
oberth restore --inspect pre-v0.17.59.tar.gz.enc
```

Supply the OpenBao root token through the echo-off prompt for a full backup;
pressing Enter instead produces history-only. Confirm the reported scope,
schema, history counts, integrity/audit verification and SHA-256. If only the
database is needed, explicitly use `oberth backup --history-only` with the same
cluster/output flags. A history-only backup does not protect secrets/grants.

The first backup creates a local recipient key/certificate. Store the private
key off-machine and verify this archive can be inspected with that key. With a
custom key, add `--key /secure/path/recipient.key` to inspect/restore commands.
Keep the archive checksum and protected values alongside the key inventory.
Stop if backup or inspection fails. See [backup and restore](backup-restore.md)
for encryption, scope and recovery limits.

## 2. Choose optional values

No values file is required for the default upgrade. `oberth upgrade` merges
new chart defaults for absent keys while retaining stored configuration and
pinning installed `compatibility.*` identities. Do not replace those identities
with new chart defaults or feed the `--all` snapshot back as a new baseline.
An older unsupported key is refused for explicit migration, not silently removed.

### Optional shadow observation

Save as `v0.17.59-shadow.yaml`:

```yaml
gitcache:
  shadow: true
```

This forces served CLI results and adds observation load, never go-git delivery.
Disable later with `gitcache.shadow: false` in the values file and another
upgrade. Do not use a zero mismatch count alone to approve go-git: inspect
errors/skips and the attribute scope described in the release notes. The shadow
CLI neutralizes system/HOME attributes; the served CLI retains its environment.

### Optional release WIF and tag routing

The following is a **public admin authorization example**, not defaults to apply
unchanged. Replace repository, provider, cloud account and step account pins.
`releaseWIF.namespace` must equal the existing `argo.namespace`; `pipelines`
here is only an example. The keys inside `releaseWIF` use the shown snake_case.
Merge with existing role/repository maps rather than dropping their entries.

Save as `v0.17.59-release-policy.yaml`:

```yaml
argo:
  namespace: pipelines
  releaseWIF:
    namespace: pipelines
    roles:
      artifact-deployer:
        provider: projects/123/locations/global/workloadIdentityPools/deployers/providers/ci
        service_account: deployer@example-project.iam.gserviceaccount.com
    repositories:
      github/example/project:
        templates:
          demo-publish: artifact-deployer
        step_service_accounts:
          demo-publish: project-demo-publish-wif
  releaseTagRoutes:
    github/example/project:
      - pattern: demo-deploy-*
        file: .oberth/demo-deploy.yaml
```

Each role must be a DNS label, at most 32 total, with a distinct pinned pool
and target account. Only exact canonical repository/template mappings grant use;
there are no wildcard identity grants. A pinned account must be distinct from
repository, shared, CI and all other step accounts. The chart creates it in the
pipeline namespace with automount off and no RBAC bindings. No dummy secret
grant is needed. WIF-only leaves cannot consume the Oberth secret store.

`service_account_name` may be omitted as above when **every** configured WIF
template has a dedicated step account. Legacy unpinned WIF templates still need
that field set to the live repository release account. Preserve existing named
secret-grant identity configuration for any actual secret-consuming steps.
A grant-free workflow uses the ordinary pipeline identity except for explicitly
pinned leaves. A branch, promotion, schedule or sibling template cannot obtain
the dedicated identity. Explicit authored token opt-outs veto WIF projection.

The route selects a file; size belongs to that file. For example, commit this
reviewed document as `.oberth/demo-deploy.yaml` on the repository's default
branch before activating the route (replace the illustrative image/command):

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: M
spec:
  entrypoint: demo-release
  activeDeadlineSeconds: 600
  templates:
    - name: demo-release
      dag:
        tasks:
          - name: publish
            template: demo-publish
    - name: demo-publish
      metadata:
        annotations:
          oberth.ci/release-wif-role: artifact-deployer
      container:
        image: example.invalid/deployer@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: ["deploy"]
```

Patterns match the whole tag name without `refs/tags/`, using path globs;
`*` does not cross `/`. Backslash escapes, whitespace and `refs/` prefixes are
refused. Multiple matching routes refuse the tag. Use at most 32 routes per
repository, 256 repositories and 256 characters per pattern. Files must be clean
single-level `.oberth/<DNS-label>.yaml`, excluding build/fragment/schedule files.
A missing or invalid selected file fails without fallback. Unmatched tags still
use release.yaml; non-tag runs still use build.yaml. No route bypasses fresh
default-branch reachability, credential checks or normal size admission.

Role/step identity maps are **not file-specific**: use distinct template names
for distinct capabilities across release documents. Preflight checks the default
release file and every routed file; local `oberth validate` alone does not load
admin routes. Changes apply after a server rollout to future/unsubmitted work
and explicit reruns; submitted Workflows retain their immutable spec.

Provision cloud pools/providers and IAM separately, binding the intended role
only to `system:serviceaccount:pipelines:project-demo-publish-wif` after adapting
the namespace/account. Verify intended exchanges and refusals for other roles,
repositories and branch identities. A successful chart render/preflight proves
no live cloud authorization. The upgrade does not create IAM policy, retire
keys or migrate publisher commands. See [release WIF](release-wif.md).

## 3. Upgrade

Install the verified v0.17.59 CLI through the normal release channel and check
`oberth version`. Use its embedded target chart version; do not use a development
binary. Preview first, adding only the optional files you chose:

```sh
oberth upgrade --context "$OBERTH_CONTEXT" --namespace "$OBERTH_NAMESPACE" --dry-run
oberth upgrade --context "$OBERTH_CONTEXT" --namespace "$OBERTH_NAMESPACE" --wait-idle
```

For optional settings, add `-f v0.17.59-shadow.yaml` and/or
`-f v0.17.59-release-policy.yaml` to **both** commands. Preview the effective
policy, image pins and compatibility identities. The CLI announces the
forward-only migration and requests confirmation; automated/non-local use
requires the applicable `--yes` confirmation. The idle-wait timeout is separate
from the rollout timeout. Keep external submissions paused throughout.

The first writable server start applies v33. Monitor rollout and migration logs;
allow for retained-history rebuild cost rather than repeatedly restarting it.
Do not add Helm `--atomic`: automatic rollback cannot undo a database migration.

## 4. Post-upgrade checks

```sh
kubectl --context "$OBERTH_CONTEXT" -n "$OBERTH_NAMESPACE" rollout status deployment/oberth --timeout=5m
kubectl --context "$OBERTH_CONTEXT" -n "$OBERTH_NAMESPACE" exec deployment/oberth -- oberth version
kubectl --context "$OBERTH_CONTEXT" -n "$OBERTH_NAMESPACE" exec deployment/oberth -- oberth version --schema
oberth status --json
oberth admission
```

Use the existing authenticated CLI connection to this same install for the last
two commands. Expect server v0.17.59 and schema 33; `version --schema` prints the
binary's supported schema, so also verify successful database startup/health.
Check database, audit, VCS, cluster and secret-store health, existing history and
uplink access. Schema, image or migration errors are a stop condition.

Before resuming normal submissions:

1. Run an ordinary supported source-backed container/script step. Inspect its
   retained StepResult: measured RSS/CPU and cgroup memory peak should be
   plausible where supported. CPU is nanoseconds, not seconds. Unknown fields
   stay null; old ambiguous zeros becoming null is expected. A child-written
   termination message remains intact and leaves usage unknown. Confirm the
   wrapped Pod retains workflow/parent scheduling selectors plus the server's
   architecture. Counters are untrusted self-reports, never admission inputs.
2. If shadow is enabled, use an unscoped administrator connection for
   `oberth status --json` (`/api/status`) and the existing authenticated
   `/metrics` scrape. Observe completed `shadow_runs`, errors/skips and both
   shadow metrics. Investigate `gogit_hash_guard` or genuine mismatches with
   repository/SHA/hash evidence; preserve the pack layout without logging file
   contents or credentials. `expected_attr_gap` is an archive-parity gap, not
   proof of corruption. A restart resets counters; shutdown cancellation is
   excluded. Verify actual delivered source independently where ambient Git
   attributes differ from the neutralized shadow reference.
3. If new release policy is enabled, check the pinned step accounts, exact
   cloud trust subjects, branch/sibling refusal and successful intended tag
   exchange. Confirm a matching tag audits the routed `pipeline_file` and uses
   its document size; an unmatched tag still uses release.yaml. Do not issue a
   publishing/apply tag just to test routing without its normal authorization.

Resume external triggers and schedules only after these checks. Take a new
post-upgrade backup for forward recovery, but retain the pre-upgrade archive
and key: a v33 backup cannot be restored into the older v32 server.

## 5. Roll back: Helm revision plus database restore

Prefer fixing forward when possible. If rollback is necessary, pause all
submissions/schedules and drain or explicitly account for active Workflows.
A restore replaces history: runs and uplink changes after the snapshot are lost;
external publishing/cloud changes are not undone. Do not fabricate a down
migration, edit the migration ledger, or start an empty database.

Use the recorded healthy **pre-upgrade** Helm revision and backup, not the newest
backup by filename. Keep the v0.17.59 CLI available for inspection/restore.
Verify the backup again and stop the server:

```sh
oberth restore --inspect pre-v0.17.59.tar.gz.enc
export OBERTH_PRE_UPGRADE_REVISION='recorded-healthy-revision'
kubectl --context "$OBERTH_CONTEXT" -n "$OBERTH_NAMESPACE" scale deployment/oberth --replicas=0
kubectl --context "$OBERTH_CONTEXT" -n "$OBERTH_NAMESPACE" wait --for=delete pods \
  -l app.kubernetes.io/name=oberth,app.kubernetes.io/instance=oberth --timeout=5m
```

Wait for server Pods to be deleted. Then restore the older chart/configuration
from Helm history. **Do not use `--wait`** here: the old binary refuses schema
33 until the database is restored. Helm may temporarily scale it back up; this
schema refusal is expected, not a reason to bypass schema checks. Immediately
scale down again and wait for all server Pods to disappear:

```sh
helm --kube-context "$OBERTH_CONTEXT" -n "$OBERTH_NAMESPACE" rollback oberth "$OBERTH_PRE_UPGRADE_REVISION" --no-hooks
kubectl --context "$OBERTH_CONTEXT" -n "$OBERTH_NAMESPACE" scale deployment/oberth --replicas=0
kubectl --context "$OBERTH_CONTEXT" -n "$OBERTH_NAMESPACE" wait --for=delete pods \
  -l app.kubernetes.io/name=oberth,app.kubernetes.io/instance=oberth --timeout=5m
```

Check that the Deployment now names the old image, its old configuration and
the same database PVC. A Helm error must be resolved before restoring; do not
run the new image against the restored old database or it will migrate again.
Helm rollback alone leaves the v33 database unchanged. If Helm history is
unavailable, stop here and recover the old chart/configuration from the saved
artifacts, preserving installed compatibility identities and removing keys
unsupported by that chart.

With the old Deployment selected and no server Pod running, preview and restore
**history only** using the verified pre-upgrade archive:

```sh
oberth restore pre-v0.17.59.tar.gz.enc --context "$OBERTH_CONTEXT" \
  --namespace "$OBERTH_NAMESPACE" --argo-namespace "$OBERTH_ARGO_NAMESPACE" \
  --openbao-namespace "$OBERTH_OPENBAO_NAMESPACE" --only history --dry-run
oberth restore pre-v0.17.59.tar.gz.enc --context "$OBERTH_CONTEXT" \
  --namespace "$OBERTH_NAMESPACE" --argo-namespace "$OBERTH_ARGO_NAMESPACE" \
  --openbao-namespace "$OBERTH_OPENBAO_NAMESPACE" --only history
```

Review and confirm the target context. Restore validates the archive, checks
the old image's schema in its helper Pod even with the server down, swaps the
database, keeps replaced files in `/data/pre-restore-<time>/`, removes orphaned
Workflows, preserves/restores database compatibility identities as needed, and
starts the selected old server. It restarts at least one replica even if the
Deployment was scaled to zero. On failure, follow its explicit stopped-state
message; do not scale up an image with incompatible database/identity settings.

History-only is sufficient for the schema rollback and preserves live secrets.
If secrets/grants also changed and need reverting, review a separate full-restore
plan with the full archive and OpenBao token; it is not equivalent to a database
rollback. Cloud IAM and other external changes require their own reversal.

Verify the old server version, its schema 32 startup, health, history, audit,
uplink access, previous pipeline behavior and scheduling before resuming
submissions. Remove optional new policy from future upgrade values deliberately;
Helm history restores its old revision, not the external values files you saved.
Retain the failed-upgrade database files for investigation. See
[backup and restore](backup-restore.md) for recovery limitations and failure cases.
