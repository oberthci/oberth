# Migration fixtures

Schema 6 was built by unchanged source at the commit in schema6.json. Its
test-only builder is schema6_build_test.go.txt. Copy that file to
internal/store/migration_fixture_build_test.go in an archive of the source
commit, then run TestBuildMigrationFixture with OBERTH_SCHEMA_FIXTURE naming
a new database path and OBERTH_FIXTURE_COMMIT naming that commit. Close the
database before compressing it with gzip (mtime 0). Add the uncompressed
database's SHA256 to the builder's JSON, keeping its complete record oracle.

Every forward step is indexed by its source schema in currentSchema and pins
that source's baseline. Add schemaN.sqlite.gz, schemaN.json and its builder
from the code that writes schema N. TestMigrationReleaseCoverage opens every
source fixture through the current Open and compares complete issue, run,
repository, grant and audit records. make guards runs this test explicitly.
Release validation also needs that source commit in the checkout: it verifies
that the recorded revision declares the fixture's schema number.
Use a revision reachable from public history so a fresh clone can verify it.

Schema 6 has no production step. Tests inject two real layout changes into
the private open seam to prove forward migration and whole-chain rollback.

Foreign keys stay enabled during every step. Changing foreign_keys inside the
transaction has no effect. Do not drop referenced tables: a drop can refuse or
cascade into dependent records. The first table-rebuild step needs a reviewed
foreign-key strategy and fixtures that prove dependent history is preserved.
