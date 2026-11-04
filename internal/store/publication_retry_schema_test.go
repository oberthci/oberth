package store

import "strings"

// Historical fixtures reconstruct an older schema from a fresh database.
// Remove v40, v39 and v38 first, including their guards and index changes,
// so their real upgrades run once.
var publicationRetryDowngradeSQL = `
DROP INDEX runs_latest_ref_idx;
CREATE INDEX runs_latest_ref_idx ON runs(repo_id, ref_kind, ref, queue_sequence DESC);
DELETE FROM schema_migrations WHERE version=40;
DROP TABLE IF EXISTS run_priority_requests;
DROP TABLE IF EXISTS priority_grants;
DELETE FROM schema_migrations WHERE version=39;
DROP TRIGGER publications_guard_update;
ALTER TABLE publications DROP COLUMN attempts;
ALTER TABLE publications DROP COLUMN retry_after;
DELETE FROM schema_migrations WHERE version=38;
` + "CREATE TRIGGER publications_guard_update" + strings.Split(strings.Split(migrations[16].sql, "CREATE TRIGGER publications_guard_update")[1], "DROP TRIGGER ci_issue_projection_monotonic")[0] +
	"DROP TRIGGER promotions_guard_update; CREATE TRIGGER promotions_guard_update" + strings.Split(strings.Split(migrations[18].sql, "CREATE TRIGGER promotions_guard_update")[1], "CREATE TRIGGER promotion_grants_no_delete")[0]
