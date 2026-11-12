package store

import (
	"context"

	"github.com/oberthci/oberth/internal/model"
)

func submissionOrderSQL() string { return "admission_sequence, queue_sequence" }

const claimableRunSQL = `status='queued' AND NOT EXISTS (SELECT 1 FROM run_cancellations WHERE run_cancellations.run_id=runs.id AND completed_at IS NULL) AND NOT EXISTS (SELECT 1 FROM publications AS publication WHERE publication.status='pending' AND publication.repo_id=runs.repo_id AND publication.ref_kind=runs.ref_kind AND publication.ref=runs.ref)`

func (s *Store) NextQueuedRun(ctx context.Context) (model.Run, error) {
	return scanRun(s.reader().QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE `+claimableRunSQL+` ORDER BY `+submissionOrderSQL()+` LIMIT 1`))
}
