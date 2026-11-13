package store

import (
	"context"

	"github.com/oberthci/oberth/internal/model"
)

// Read columns add repository identity at the read boundary.
const runReadColumns = runColumns + `, (SELECT name FROM repositories WHERE repositories.id = runs.repo_id)`

func scanReadRun(row rowScanner, extra ...any) (model.Run, error) {
	var repo string
	run, err := scanRun(row, append([]any{&repo}, extra...)...)
	run.Repo = repo
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
