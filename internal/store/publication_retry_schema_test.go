package store

import "strings"

// Historical fixtures reconstruct an older schema from a fresh database.
// Remove v38 first, including its guards, so its real upgrade runs once.
var publicationRetryDowngradeSQL = `
DROP TRIGGER publications_guard_update;
ALTER TABLE publications DROP COLUMN attempts;
ALTER TABLE publications DROP COLUMN retry_after;
DELETE FROM schema_migrations WHERE version=38;
` + "CREATE TRIGGER publications_guard_update" + strings.Split(strings.Split(migrations[16].sql, "CREATE TRIGGER publications_guard_update")[1], "DROP TRIGGER ci_issue_projection_monotonic")[0] +
	"DROP TRIGGER promotions_guard_update; CREATE TRIGGER promotions_guard_update" + strings.Split(strings.Split(migrations[18].sql, "CREATE TRIGGER promotions_guard_update")[1], "CREATE TRIGGER promotion_grants_no_delete")[0]
