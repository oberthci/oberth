package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func submissionOrderSQL() string { return "admission_sequence, queue_sequence" }

// Automatic retries keep their queue position but wait briefly before dispatch.
// The durable receipt and creation time preserve the delay across restarts.
const automaticRetryDelay = 5 * time.Second

func automaticRetryReadySQL(now time.Time) string {
	return fmt.Sprintf(`(created_at <= %d OR NOT EXISTS (SELECT 1 FROM audit_actions WHERE action='run.retry.auto' AND resource_type='run' AND resource_id=runs.id))`, unixNano(now.Add(-automaticRetryDelay)))
}

const claimableRunSQL = `status='queued' AND NOT EXISTS (SELECT 1 FROM run_cancellations WHERE run_cancellations.run_id=runs.id AND completed_at IS NULL) AND NOT EXISTS (SELECT 1 FROM publications AS publication WHERE publication.status='pending' AND publication.repo_id=runs.repo_id AND publication.ref_kind=runs.ref_kind AND publication.ref=runs.ref)`

func (s *Store) NextQueuedRun(ctx context.Context) (model.Run, error) {
	return scanRun(s.reader().QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE `+claimableRunSQL+` AND `+automaticRetryReadySQL(s.now())+` ORDER BY `+submissionOrderSQL()+` LIMIT 1`))
}
