package store

import (
	"context"
	"fmt"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

const publicationPolicyWaitMigrationVersion = 49

// Keep availability state separate from the immutable, audited push intent.
const publicationPolicyWaitSchema = `CREATE TABLE publication_policy_waits (
 publication_id TEXT PRIMARY KEY REFERENCES publications(id),
 since INTEGER NOT NULL
);
CREATE TRIGGER publication_policy_wait_cleanup AFTER UPDATE OF status ON publications
WHEN NEW.status != 'pending' OR OLD.status != 'pending'
BEGIN DELETE FROM publication_policy_waits WHERE publication_id=NEW.id; END;
`

// SetPublicationPolicyWaiting preserves the first unavailable time across
// retries and restarts. A successful policy read clears the outage, even if
// the push itself later needs retrying. Repeated calls are idempotent.
func (s *Store) SetPublicationPolicyWaiting(ctx context.Context, id string, waiting bool) (time.Time, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return time.Time{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var status model.PublicationStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM publications WHERE id=?`, id).Scan(&status); err != nil {
		return time.Time{}, translateNotFound("publication", err)
	}
	if status != model.PublicationPending {
		return time.Time{}, fmt.Errorf("%w: publication is terminal", ErrInvalidState)
	}
	var since time.Time
	if waiting {
		if _, err := tx.ExecContext(ctx, `INSERT INTO publication_policy_waits(publication_id,since) VALUES(?,?) ON CONFLICT(publication_id) DO NOTHING`, id, unixNano(s.now())); err != nil {
			return time.Time{}, err
		}
		var value int64
		if err := tx.QueryRowContext(ctx, `SELECT since FROM publication_policy_waits WHERE publication_id=?`, id).Scan(&value); err != nil {
			return time.Time{}, err
		}
		since = fromUnixNano(value)
	} else if _, err := tx.ExecContext(ctx, `DELETE FROM publication_policy_waits WHERE publication_id=?`, id); err != nil {
		return time.Time{}, err
	}
	if err := tx.Commit(); err != nil {
		return time.Time{}, err
	}
	return since, nil
}
