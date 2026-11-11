# Adding schema migrations

Run from your checkout:

```sh
python3 -I hack/new-migration.py add-run-index
```

This creates `internal/store/migrations/YYYYMMDDHHMMSS-add-run-index-<random>.sql`
and appends its ID to `.oberth/migration-sequence.txt`.
The timestamp is UTC; a 128-bit random suffix separates independent branches
created in the same second with the same slug. Exclusive file creation prevents
local overwrites. Keep the complete filename/ID unchanged when merging or
rebasing. The directory is embedded automatically: there is no Go registration,
shared next number, or latest-version constant to edit.

Write forward-only SQL and fill in the file's `Purpose` and `Rollback` comments.
Add an upgrade test that preserves existing data and audit history. Use a new
migration to correct a deployed one; never edit shipped bodies or ledger rows.
SQL, the ID receipt, and the initial ledger-format transition commit in the same
transaction. Do not put `BEGIN`, `COMMIT`, or connection-level PRAGMAs in a new
SQL migration. Historical Go and connection-level migrations remain unchanged.

Migrations 1–55 are frozen and run first. Pending timestamp IDs run in lexical
order. A previously applied later ID does **not** suppress an earlier pending
ID: merging either branch after deploying the other applies both exactly once.
Independent migrations therefore need to work in either deployment order. If
changes depend on each other or alter the same objects, reconcile their SQL and
test the supported upgrade paths before release; unique IDs do not resolve SQL
conflicts. For dependent migrations, ensure the complete ID sorts after every
prerequisite (generate in a later UTC second), and include those prerequisites
in the branch.

`schema_migrations` is the only applied-migration ledger. Existing `version` and
`applied_at` values remain intact; their IDs are the decimal strings `1`…`55`.
For new entries, `id` holds the full timestamp and slug, and `version` is a local
receipt ordinal allocated on application. That ordinal can differ across
installations and must never identify a timestamped migration. Use `WHERE id=?`
in fixtures, assertions, and maintenance queries for new migrations.

The numeric `oberth version --schema`, status field, and backup schema field stay
compatible: they report the migration count (or applied receipt count for a
snapshot). A count alone is not proof of compatibility. Every database open and
backup verification checks all applied IDs against the binary's catalog. An
unknown ID refuses with `ErrSchemaTooNew`, even at an equal or lower count, before
recovery or migration writes. Corrupt receipt gaps also refuse. Older integer-only
binaries see a receipt above 55 and refuse under their existing newer-schema
check. Downgrades still require restoring a suitable pre-upgrade backup; v1's
explicit continuity path and v2's backup-and-replace requirement are unchanged.

## Migration sequence check

`.oberth/migration-sequence.txt` lists every timestamped migration ID in apply
order and only grows at its end. The `generic-names` CI step runs
`.oberth/check-migration-sequence.py`, which requires a new migration to be
numbered main's latest+1: its ID sorts after every ID already listed. It
refuses an ID listed twice, two migrations with the same timestamp, a file
that is not listed (or a listed ID without a file), and a list that is not
strictly increasing. A branch whose migration was generated before a migration
that has since landed on main conflicts at the end of the list; after the
rebase, regenerate the migration with `hack/new-migration.py`, move its SQL and
drop the old ID. Locally, `python3 -I .oberth/check-migration-sequence.py
--base origin/main` also checks that main's list is an unchanged prefix.

CI's store suite includes `TestMigrationIDsNoNewIntegerMigrations`, which freezes
the integer catalog at 55 and rejects integer-named SQL files, invalid timestamp
IDs, and duplicate IDs. `TestEveryMigrationDocumentedInAgentContract` keeps the
legacy contract table complete and requires `Purpose`/`Rollback` in each new SQL
file. Keep future timestamped migration details in that file so independent
branches need not edit a shared table. The required `generic-names` CI job also
runs the generator's tests.

Before submitting:

```sh
python3 -I hack/test-new-migration.py
go test ./...
go test -race ./internal/store ./internal/service
golangci-lint run ./...
```
