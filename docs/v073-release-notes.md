# v0.17.73 schema-35 upgrade requirement

This candidate includes DB-deadline R2, init-readonly R2, IO admission R2,
restart-cut R3, RCA UX R2, runtime capacity #1215, controller admission #1214
R3 and API reader isolation #1217. IO R2 de95c99 is Opus approved with the two
nits folded into assembly: disk-floor reasons take precedence over pressure,
and a pressure clock that has not started cannot be considered expired.
Final candidate/chart/image pins and the final-pin restore rehearsal await
approved log-continuity R2.

**During a long IO-pressure hold, lower admission capacity; do not raise it.**
Raising capacity can send more work onto the saturated disk when pressure clears
or individual maximum-deferral clocks expire. Lowering preserves running work
and lets it drain; it does not cancel existing grants. Use the effective runtime
capacity shown in Status and the [runtime-capacity procedure](runtime-admission-capacity.md).

Upgrading from v0.17.72 applies migration **35** (`runs_latest_ref_idx`). Take
and independently verify a fresh pre-upgrade **schema-34** backup immediately
before upgrade, retain it off-volume with its restore key, old values and old
chart/image/CLI pins, and drain KubeVirt/resource-delete runs first. Binary
`version --schema` reports support; the verified backup establishes actual DB
schema and audit head.

**Rollback is database restore only.** v0.17.72 refuses schema 35; Helm rollback
alone cannot recover it. Index/ledger edits are unsupported. `oberth restore`
alone does not downgrade the binary and the new binary migrates a restored old
DB back to 35. Follow the [named old-chart/old-CLI emergency procedure](v073-upgrade-runbook.md):
stop writers, stage a maintenance copy of the old chart at zero replicas, use
the old CLI's `restore --only history` dry-run then confirmed restore, verify
old-version readiness/schema and re-apply the normal old chart. The old chart
rejects `replicaCount=0` in both validation layers; the rehearsed maintenance
copy changes only the Deployment's rendered replica field, leaving values at one.

Restore loses post-backup database history/tokens/audit entries and deletes
Workflows absent from the restored DB. It does not undo external publishes or
VM/resource operations. History-only backup/restore does not recover OpenBao
KV/policies/grants, chart values, identities or log/artifact files; preserve
those separately. The previous DB files remain on the PVC as recovery evidence.

The first Rekor publication outside the backup's audit prefix can close
ordinary restore, sometimes even before upgrade because backup appends a later
completion audit entry. Check pending intents, pinned continuity records and
authenticated remote witness history before replacing data. Archive integrity
and an empty local list do not authorize resetting external history. If that
continuity cannot be proved, choose forward repair.

The [local qualification](schema35-rollback-qualification.md) now exercises real
encrypted CLI backup/inspect/dry-run/history restore, Helm staging, old-server
readiness, schema 34 -> 35 -> 34, nonempty audit-prefix preservation and actual
orphan Workflow deletion. It uses disposable identities/storage and no external
Rekor. It is not production witness/storage authorization or qualification of an
unassembled release image. Complete logs and JSON accompany the handoff.
