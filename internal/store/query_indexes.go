package store

import (
	"context"
	"database/sql"
)

// Query indexes change no records or constraints and may be absent in archives.
// Each present index must still match its exact definition during verification.
func queryIndexes() []struct{ name, statement string } {
	return []struct{ name, statement string }{
		{"issue_totals", `CREATE INDEX IF NOT EXISTS issue_totals ON issues(upstream,state,json_extract(value,'$.work_status'),json_extract(value,'$.updated_at'))`},
		{"issue_ci", `CREATE INDEX IF NOT EXISTS issue_ci ON issues(repo,json_extract(value,'$.branch')) WHERE json_extract(value,'$.kind')='ci'`},
	}
}

func ensureQueryIndexes(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, index := range queryIndexes() {
		if _, err := tx.ExecContext(ctx, index.statement); err != nil {
			return databaseError(err)
		}
	}
	return databaseError(tx.Commit())
}

func expectQueryIndexes(ctx context.Context, actual, expected *sql.DB) error {
	for _, index := range queryIndexes() {
		var count int
		if err := actual.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE name=?`, index.name).Scan(&count); err != nil {
			return databaseError(err)
		}
		if count != 0 {
			if _, err := expected.ExecContext(ctx, index.statement); err != nil {
				return databaseError(err)
			}
		}
	}
	return nil
}
