# v0.17.73 release notes

This candidate includes DB-deadline R2, init-readonly R2, IO admission R2,
restart-cut R3, RCA UX R2, runtime capacity #1215, controller admission #1214
R3, API reader isolation #1217 and log-continuity R2 382e1a7a. All inputs are
coordinator-approved. IO R2 de95c99 is Opus approved with the two
nits folded into assembly: disk-floor reasons take precedence over pressure,
and a pressure clock that has not started cannot be considered expired.
Exact assembled head, input ancestry, local image/chart pins and final-pin
restore evidence are recorded in the release handoff (V073-READY.md).

**During a long IO-pressure hold, lower admission capacity; do not raise it.**
Raising capacity can send more work onto the saturated disk when pressure clears
or individual maximum-deferral clocks expire. Lowering preserves running work
and lets it drain; it does not cancel existing grants. Use the effective runtime
capacity shown in Status and the [runtime-capacity procedure](runtime-admission-capacity.md).

Behavior changes in this release:

- Transient DB deadlines/contention stay local to the pending database operation;
  background recovery retries without replaying external execution, and shutdown
  allows bounded finalization/cleanup to finish.
- Sequential init may populate a template-local tools emptyDir mounted read-only
  by main. Shared/source/server mounts retain their protections. **Known nit:**
  a writer before a restartable init reader is also forced read-only; that shape
  can fail its init write and fails closed. It has no order-sensitive exemption.
- New grants defer under host IO pressure (default full avg60 30%, resuming below
  20%) with independent bounded pressure clocks. Capacity, priority, disk floors,
  VM/group ownership and backfill gates still apply when those clocks expire.
  Runtime max-concurrent changes can be read from an audited ConfigMap projection;
  invalid changes keep the last good value and lowering leaves running work alone.
- Verified restart-cut runs become interrupted and manually retryable. Real
  observed init/step failures remain failed. Allow-listed branch preparation
  errors with positive pre-Workflow-POST proof can receive one atomic same-SHA
  automatic retry; submitted/uncertain creates and credentialed publication paths
  do not gain automatic replay permission.
- The run list defaults to latest-ref heads with expandable history and explicit
  retry/superseded display states. Schema 35 supplies its covering index.
  Readiness retains a previous good sample across bounded reader delays without
  extending its last-success age or suppressing hard failures.
- Controller admission can recover from incomplete eviction/shutdown history
  only with an independently verified Ready replacement and exact converged
  ReplicaSet/Deployment evidence. Status shows the admission hold; background
  aggregate status refreshes at most every five minutes, direct visits stay fresh.
- Queue patterns and the next-queued peek use the existing read-only reader pool,
  preserving the single writer and separate readiness/auth readers.
- Failed-init logs, observed exit/termination diagnostics, bounded final log reads
  and durable pod-log journals improve log continuity. Bad journals degrade only
  that run's delivery, keeping the real execution result. R2 uses one journal sync
  per durable batch and prunes only after a synced/indexed projection and settled
  terminal ownership. New Workflows retain at least 180 seconds for final reads.
  **Known nit:** after a host crash, journal replay occurs only through observeRun;
  other startup/finalization/read paths do not replay it. This is not a universal
  startup or read-time journal recovery pass.

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

The earlier [local qualification](schema35-rollback-qualification.md) exercises real
encrypted CLI backup/inspect/dry-run/history restore, Helm staging, old-server
readiness, schema 34 -> 35 -> 34, nonempty audit-prefix preservation and actual
orphan Workflow deletion. It uses disposable identities/storage and no external
Rekor. It is not production witness/storage authorization or qualification of an
published/signed release image. The final assembly handoff also records its
exact-head rehearsal separately, with complete logs and JSON.
