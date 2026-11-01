# Schema 37: container telemetry and rollback

Schema 37 retains the version-2 observation-format guard. It also adds
`step_resource_usage_retention_idx(last_observed_at)` for oldest-first bounded
cleanup. Schema-36 table/index definitions and version-1 rows are preserved;
new observations upgrade an existing main incarnation in place. Unlike the
initial measurement patch's no-op guard, this migration builds an index over
existing usage rows. Qualify that one-time build on the real database size.
The migration applies even with `resourceSampler.enabled: false`.

**Helm rollback alone stops the server with schema-too-new.** A schema-36 binary
refuses schema 37, and the chart's Recreate strategy leaves no old Pod serving.
Rollback requires restoring the independently verified pre-upgrade database
backup for the selected old image/schema, following the existing zero-writer
maintenance-chart, identity, OpenBao and audit-continuity restore procedure.
Restoring that backup loses database writes made after the backup. Never edit
the migration ledger or drop the new index to simulate a database downgrade.

The [schema-36 upgrade note](schema36-upgrade-note.md) remains historical for
that target. For a schema-36 predecessor, qualification must pin the actual old
and new chart/image/CLI digests and rehearse **36 -> 37 -> restored 36**:

1. Pause intake and drain all Workflow/VM/helper work. Independently inspect the
   encrypted pre-upgrade backup, including schema, integrity, foreign keys,
   audit head/domain and original immutable identities. Retain the old pins.
2. Upgrade the pinned new chart/image. Verify schema 37, the retention index,
   unchanged old ledger entries/audit prefix, readable version-1 rows, valid
   version-2 observations and application/OpenBao/token-delivery health.
3. Confirm the pinned old binary refuses the upgraded database as schema-too-new
   in an isolated rehearsal, with production writers stopped before any restore.
4. Restore the verified schema-36 backup using the pinned old CLI and zero-writer
   maintenance procedure. Verify exact restored schema/integrity/FKs/audit
   prefix/restore receipt, original identities, one Ready old server, VM cleanup
   and working OpenBao authentication/token delivery before resuming intake.

A schema-33 or schema-35 predecessor instead needs its own matching backup and
pinned **33 -> 37 -> restored 33** or **35 -> 37 -> restored 35** rehearsal. The
historical schema-35/schema-36 evidence does not qualify these paths.

The source tests cover migration preservation and the existing schema-too-new
refusal boundary. No live Helm rollback or encrypted operator restore rehearsal
was performed in this change; those remain release-qualification work.
