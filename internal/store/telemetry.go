package store

import (
	"context"
	"time"
)

func (s *Store) recordUses(ctx context.Context) {
	defer close(s.usesDone)
	var retryAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case digest := <-s.uses:
			batch := s.useBatch(digest)
			if s.options.Clock().Before(retryAt) {
				continue
			}
			if err := s.touchUses(ctx, batch); err != nil {
				retryAt = s.options.Clock().Add(time.Minute)
			}
		}
	}
}

func (s *Store) useBatch(first string) []string {
	unique := map[string]bool{first: true}
drain:
	for range cap(s.uses) {
		select {
		case digest := <-s.uses:
			unique[digest] = true
		default:
			break drain
		}
	}
	result := make([]string, 0, len(unique))
	for digest := range unique {
		result = append(result, digest)
	}
	return result
}

func (s *Store) touchUses(ctx context.Context, digests []string) error {
	// One bounded transaction coalesces optional accounting independently of login.
	bounded, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	tx, done, err := s.beginWrite(bounded)
	if err != nil {
		return err
	}
	defer done()
	at := s.options.Clock().UTC()
	for _, digest := range digests {
		_, err := tx.ExecContext(bounded,
			`UPDATE credentials SET last_used=? WHERE digest=? AND revoked=0 AND (last_used IS NULL OR last_used<?)`,
			at.Format(timestampFormat), digest, at.Add(-time.Minute).Format(timestampFormat))
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
