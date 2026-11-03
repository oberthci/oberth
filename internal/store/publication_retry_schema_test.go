package store

import (
	"strings"

	"github.com/oberthci/oberth/internal/testdb"
)

// Historical fixtures reconstruct an older schema from a fresh database.
// Remove all later schemas before reconstructing historical fixtures.
var integrationDowngradeSQL = pilotRetirementDowngradeSQL

var publicationRetryDowngradeSQL = integrationDowngradeSQL + `
DROP TRIGGER publications_guard_update;
ALTER TABLE publications DROP COLUMN attempts;
ALTER TABLE publications DROP COLUMN retry_after;
DELETE FROM schema_migrations WHERE version=38;
` + "CREATE TRIGGER publications_guard_update" + strings.Split(strings.Split(migrations[16].sql, "CREATE TRIGGER publications_guard_update")[1], "DROP TRIGGER ci_issue_projection_monotonic")[0] +
	"DROP TRIGGER promotions_guard_update; CREATE TRIGGER promotions_guard_update" + strings.Split(strings.Split(migrations[18].sql, "CREATE TRIGGER promotions_guard_update")[1], "CREATE TRIGGER promotion_grants_no_delete")[0]

// Reconstruct the v38 surface before replaying any earlier migration.
const restartRecoveryDowngradeSQL = testdb.PostV44Downgrade + `
DROP TRIGGER IF EXISTS publications_tag_tuple_insert;
DROP INDEX IF EXISTS publications_tag_tuple_idx;
DROP TABLE IF EXISTS restart_step_timings;
DROP TABLE IF EXISTS restart_workflow_timings;
DELETE FROM schema_migrations WHERE version >= 43;
`
