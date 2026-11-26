package store

import (
	"context"
	"database/sql"
	"github.com/oberthci/oberth/internal/publicerr"
)

func resumeInspection(ctx context.Context, tx *sql.Tx, id string) error {
	var inspected int
	err := tx.QueryRowContext(ctx, `WITH RECURSIVE chain(id) AS (
SELECT ? UNION SELECT json_extract(r.origin,'$.resumed_from') FROM runs r JOIN chain c ON r.id=c.id
WHERE json_extract(r.origin,'$.resumed_from') IS NOT NULL)
SELECT EXISTS(SELECT 1 FROM audit a JOIN chain c ON a.resource_id=c.id
WHERE a.action='run.assist' AND json_extract(a.details,'$.Kind')='exec')
OR EXISTS(SELECT 1 FROM runs r JOIN chain c ON r.id=c.id
WHERE json_extract(r.origin,'$.resume_plan.InPod') IS NOT NULL)`, id).Scan(&inspected)
	if err != nil {
		return databaseError(err)
	}
	if inspected != 0 {
		return publicerr.New(publicerr.InvalidTransition, "workspace state; push for a full run", "Push the fix with -o oberth.run=1 for a full unassisted run.", nil)
	}
	return nil
}
