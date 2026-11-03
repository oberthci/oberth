package store

import (
	"context"
	"github.com/oberthci/oberth/internal/model"
)

// Compare durable acceptance order, not priority, admission order or the
// current page. Keep this read-side signal separate from SupersededBy, which
// records an actual cancellation/replacement and can outlive its successor.
// Match enqueue's branch/schedule lane; tags and promotion attempts are independent.
const runSupersededSQL = `EXISTS (
 SELECT 1 FROM runs AS newer
 WHERE runs.ref_kind = 'branch' AND runs.trigger != 'promotion'
   AND newer.trigger != 'promotion'
   AND newer.repo_id = runs.repo_id AND newer.ref_kind = runs.ref_kind
   AND newer.ref = runs.ref AND newer.queue_sequence > runs.queue_sequence
   AND newer.schedule_entry = runs.schedule_entry
   AND newer.status IN ('queued', 'running')
)`

// Read columns are separate from RETURNING columns used by state transitions.
const runReadColumns = runColumns + `,
 (SELECT name FROM repositories WHERE repositories.id = runs.repo_id), ` + runSupersededSQL

func supersededRunRestriction(filter model.RunListFilter) string {
	if filter.Superseded {
		return ` AND ` + runSupersededSQL
	}
	return ""
}

func scanReadRun(row rowScanner, extra ...any) (model.Run, error) {
	var repo string
	var superseded bool
	run, err := scanRun(row, append([]any{&repo, &superseded}, extra...)...)
	run.Repo, run.Superseded = repo, superseded
	return run, err
}

// API selectors add identity/context at the read boundary. Keep Run and every
// state-transition RETURNING record unchanged for scheduler identity reads.
func (s *Store) readRunContext(ctx context.Context, id string) (model.Run, error) {
	run, err := scanReadRun(s.reader().QueryRowContext(ctx, `SELECT `+runReadColumns+` FROM runs WHERE id = ?`, id))
	if err != nil {
		return model.Run{}, translateNotFound("run", err)
	}
	return run, nil
}
