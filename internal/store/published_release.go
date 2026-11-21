package store

import (
	"context"
	"encoding/json"

	"github.com/oberthci/oberth/internal/model"
)

// PublishedRelease reads the sole delivered receipt of an immutable repository tag.
func (s *Store) PublishedRelease(ctx context.Context, repo, ref string) (model.Publication, error) {
	var p model.Publication
	var body string
	err := s.readers.QueryRowContext(ctx, `SELECT value FROM publications WHERE repo_id=(SELECT id FROM repositories WHERE canonical=?) AND status='delivered' AND json_extract(value,'$.release.tag')=?`, repo, ref).Scan(&body)
	if err != nil {
		return p, databaseError(err)
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		return p, refuse(corrupt, err)
	}
	return p, nil
}
