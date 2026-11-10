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

Since v0.21.0 `oberth install` (with or without `--upgrade`) never changes the chart version of an existing Oberth release. When the CLI is newer than the release it stops before touching any release and points at `oberth upgrade`; when the CLI is older it refuses to downgrade the server, whose newer database the older binary cannot open, and asks for a newer CLI. Same-version reconciliation (a secret store, Rekor, TLS names, Argo limits with `--argo-parallelism` / `--argo-sync-limit`) stays with `install --upgrade`. A local `--chart` is not a version move.

Version changes go through one command:

```sh
oberth upgrade --yes            # from the directory where the backup and rollback record should go
```

It, in order:

1. refuses an installed release newer than the CLI (`--allow-downgrade` accepts one within the same database schema) and a running schema newer than the CLI's;
2. fetches the signed release for the CLI's version (`release.json`, `oberth-<version>.tgz` and their sigstore bundles from `releases.oberth.ci` and `charts.oberth.ci`, or `--release-dir DIR`), verifies both signatures against the release key compiled into the CLI, checks the chart against the record's digest and pins the server image digest the record names; any failure refuses the upgrade (`--chart` is the explicit unverified development override);
3. previews the Helm change server-side and prints the changed value paths;
4. takes a verified history-only backup (`oberth backup --history-only`, in `--backup-dir`, default the current directory) and writes a rollback record next to it, before the first change; without a backup it refuses unless `--no-backup`;
5. applies the Argo controller profile transition if asked, upgrades the Oberth release, waits for the rollout and verifies the running version.

To roll back the upgrade that record describes:

```sh
oberth upgrade --rollback ./oberth-upgrade-v0.20.1-to-v0.21.0-<time>.json --dry-run   # the plan
oberth upgrade --rollback ./oberth-upgrade-v0.20.1-to-v0.21.0-<time>.json --yes
```

It refuses another cluster, a release that moved since, and a stale record: each release's deployed revision must be the one the recorded upgrade made (or the one a rollback of it made), so a later `install --upgrade` or upgrade makes the record unusable unless `--force-record` (check `helm history` first). It rolls back the argo-workflows release (when the upgrade changed it) and the Oberth release to the recorded revisions, and, when the upgrade migrated the database, restores the pre-upgrade backup's history (`oberth restore --only history`), because the previous server cannot open the migrated database. Rolling back to a release older than v0.21.0, which does not read the restored-runs marker, the restore instead holds admission in the restored database whenever it holds queued or waiting runs (a hold the backup already had is kept): the older server starts held, the backup's queued work waits, and the rollback prints how to release the hold once you have checked those runs (`oberth admission hold off --reason "restored runs checked"`; the hold lapses by itself after 12 hours, and a backup taken before v0.17.85 cannot carry one). Onto v0.21.0 and later, no run of that backup is dispatched again: running ones whose Workflow is gone are interrupted, and queued or waiting ones (which may have run after the upgrade) are cancelled with the reason "restored from a backup; not dispatched again" (see [backup and restore](backup-restore.md)).
