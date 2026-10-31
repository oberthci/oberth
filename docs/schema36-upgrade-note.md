# v0.17.74: schema 36 and platform/sampler qualification

The #1216 sampler targets **v0.17.74**, based on final v0.17.73 `ac5f7138`.
Schema v36 is applied even when the sampler is disabled. It adds
`step_resource_usage` and its repository/time/step indexes; v35's covering
latest-ref index and every older migration are preserved. There is no supported
ledger edit, DROP-table shortcut or Helm-only database downgrade.

The [v0.17.73 upgrade runbook](v073-upgrade-runbook.md) and
[schema35 rehearsal](schema35-rollback-qualification.md) are historical evidence
for their pinned **34→35→34** path. They do not qualify a 36 migration/restore.
Do not change those historical pins and claim the rehearsal still applies.

Before v0.17.74 release qualification, pin the actual old/new chart, server and
CLI digests and establish the observed pre-upgrade database schema, integrity,
foreign keys, audit head/domain and continuity mode. Keep that evidence separate
from emergency shell variable setup. Take and independently inspect the matching
encrypted pre-upgrade backup before migration. Preserve the current OpenBao,
schema/audit identities and kubelet/runtime admission state.

The usual v0.17.73 predecessor is schema 35, so a v0.17.74 rollback rehearsal
must assert **35→36→35** using the schema-35 backup and old CLI/image. If an
operator deliberately targets an older schema-34 release, it needs its own
schema-34 backup/CLI/image and must instead assert **34→36→34**. A schema-35
backup is not a substitute for restoring schema 34.

Assertions for the new qualification:

- Before migration: actual old schema and pinned old image/CLI; verified backup
  integrity/FKs/audit and current immutable identities.
- After migration: schema 36, new usage table/indexes, retained v35 index/data and
  unchanged old ledger entries/audit prefix. Disabling sampling does not remove
  migration 36.
- Before restore: writers fully stopped and the **asserted staged old image**;
  use the existing zero-writer maintenance-chart procedure, not a normal old
  server briefly starting against schema 36.
- After restore: exact selected predecessor schema, its integrity/FKs/audit
  prefix/restore receipt, one old Ready server, correct cleanup/continuity and
  working OpenBao authentication/token delivery. Retain failed/superseded-run
  evidence according to the restore protocol.

This branch runs real v35→v36 store migration tests. It does not claim a new
encrypted operator restore rehearsal or production qualification. The release
coordinator must repeat the pinned operator rehearsal for v0.17.74 rather than
copy the v0.17.73 schema assertions.

Platform rollout is independently gated by the
[R2 protection plan](platform-protection-plan.md): measure Argo/OpenBao before
first limits, lower CI capacity first, qualify namespace critical-class policy
and pressure recreation, check unseal/application health per restart, and restore
capacity last. Source-only sampler approval does not authorize blind host changes.
