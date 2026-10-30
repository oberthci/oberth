# Schema 35 rollback qualification — database restore only

Qualification date: 2026-10-30T20:47:16.817453+00:00.
Source implementations: released v0.17.72 `3f9cac207e235235d84a3965c90837e24f40a062` (schema 34)
and approved RCA UX R2 `f9cbe94297c37a87b86dbb22158b36820a9f0d1a` (schema 35).

## Decision

Schema 35 adds `runs_latest_ref_idx`, but migration policy remains forward-only.
**Rollback is database restore only.** v0.17.72 refuses a schema-35 database or
backup with ErrSchemaTooNew. Helm rollback alone cannot make it start. Dropping
the index or deleting migration-ledger rows is not a supported rollback path.
The earlier local index/ledger-removal experiment is not product qualification
and is superseded by this restore-only result and policy.

**Take and verify a fresh backup immediately before the upgrade**, using the
current server's supported encrypted backup/verification workflow. Confirm the
backup's actual schema is 34, retain a protected off-volume copy and its restore
keys, and record its audit head and the current external witness state. The
binary's `version --schema` reports support, not the database's actual ledger.
A schema-35 backup cannot be the rollback backup for a schema-34 binary.

## Closed-copy rehearsal

Probe executables linked directly against the exact store implementations above
ran offline in Linux/arm64 Go 1.26.10 (`oberth-1204-go:lab`, network disabled,
GOMAXPROCS=2, CPUs 0–1). They used WriteVerifiedBackup and VerifyBackupArchive,
not a raw copy of an open SQLite database. No server image/cluster was deployed.

| Step | Result |
| --- | --- |
| Seed schema 34 and nonempty audit chain | 1 run; audit head ID 1 |
| Online snapshot and independently verify archive 34 | schema 34; integrity/FKs true; identical audit report |
| Open with schema-35 implementation and write a failed source plus automatic child | schema 35; 3 runs; audit head ID 3 |
| Open migrated DB with schema-34 implementation | ErrSchemaTooNew, database=35 binary=34 |
| Verify schema-35 archive using old implementation | ErrSchemaTooNew |
| Verify archive 34, restore its database into an empty destination, open with old implementation | schema 34; 1 run; exact original nonempty audit head retained |
| Independent SQLite inspection of restored copy | integrity_check=ok; no FK violations; ledger max 34; no v35 index |
| Corrupt an archive and verify it | rejected |

The two post-upgrade run rows and their later audit history are absent after
restore. This demonstrates real data loss at rollback, not preservation of
post-upgrade writes. The schema-35 source copy remains at 3 runs; it was not
modified into schema 34. **No migration-ledger/index edits were used.**

Verified pre-upgrade fixture archive SHA256: `ea95d7a55de56112d1a1634322ba86df0fb9f706a07863d6406b78d7459e78e8`.
Log SHA256: `bf27fdf97e36b3d6910d151561cec7aadc9ec75b6ba719c96fab07d958fa8c58`. Handoff evidence: `schema35-restore-qualification-linux.log`,
`schema35-restore-qualification-linux.json`, runner `schema35-restore-qualify-linux.py`.
The JSON records exact synthetic fixture paths, both probe-source hashes and
archive/restored-file hashes. A separate host rehearsal also passed.

## Operational limits

This qualifies store snapshot/schema/audit compatibility on synthetic copies.
The fixtures contain no production data or secrets. Production OpenBao key
retrieval, outer backup encryption/signature transport, external TSA/witness
restore authorization and live Workflow/resource recovery were not exercised.
The inner synthetic tar archives are plaintext test artifacts, not production
backup instructions.

Restoring loses all database changes after the backup; it does not undo external
publishes, running Workflows, VM resources, OpenBao changes, chart values,
identities or log/artifact files. Preserve their separate recovery state and
ownership. A verified backup alone is not an operational rollback guarantee:
external witnesses ahead of its audit head can refuse ordinary restore. Do not
reset witnesses or erase audit evidence to force acceptance. If restore is not
authorized by the installation's existing continuity policy, use forward repair
or its explicitly approved disaster-recovery procedure.

Before any restore, stop all writers/controllers that can restart the new
server, preserve the schema-35 DB/WAL/SHM, and use the qualified offline restore
procedure. The pinned old chart/image must be installed before the server starts
again, or the new binary will immediately migrate the restored copy back to 35.
Repeat backup verification and restore feasibility on a fresh production backup
right before deployment. This rehearsal performed no live or PVC operation.
