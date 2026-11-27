package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/oberthci/oberth/internal/stateport"
)

// mutationChange covers control facts even when the caller emits no event.
// Read both sides of a mutation so removal and scope moves invalidate old reads.
func (s *Store) mutationChange(ctx context.Context, tx *sql.Tx, m mutation) error {
	switch m.resource {
	case "run":
		return s.runChange(ctx, tx, m.id)
	case "repository":
		return s.repositoryChange(ctx, tx, m.id)
	case "promotion", "publication":
		return s.recordChange(ctx, tx, m)
	case "issue":
		if m.id != "" {
			return s.issueChange(ctx, tx, m.id)
		}
	case "authority", "schedule", "capability":
		s.noteChange(stateport.Change{Repository: m.id, Invalidate: true})
	case "key", "installation", "upstream", "invitation":
		s.noteChange(stateport.Change{Authority: true})
	}
	return nil
}

func (s *Store) runChange(ctx context.Context, tx *sql.Tx, id string) error {
	var repo string
	err := tx.QueryRowContext(ctx, `SELECT p.canonical FROM runs r JOIN repositories p ON p.id=r.repo_id WHERE r.id=?`, id).Scan(&repo)
	return s.factChange(stateport.Change{Repository: repo, Run: id}, err)
}

func (s *Store) repositoryChange(ctx context.Context, tx *sql.Tx, id string) error {
	if strings.Contains(id, "/") {
		s.noteChange(stateport.Change{Repository: id, Invalidate: true})
		return nil
	}
	var repo string
	err := tx.QueryRowContext(ctx, `SELECT canonical FROM repositories WHERE id=?`, id).Scan(&repo)
	return s.factChange(stateport.Change{Repository: repo, Invalidate: true}, err)
}

func (s *Store) recordChange(ctx context.Context, tx *sql.Tx, m mutation) error {
	var repo string
	query := `SELECT p.canonical FROM publications r JOIN repositories p ON p.id=r.repo_id WHERE r.id=?`
	if m.resource == "promotion" {
		query = `SELECT p.canonical FROM promotions r JOIN repositories p ON p.id=r.repo_id WHERE r.id=?`
	}
	err := tx.QueryRowContext(ctx, query, m.id).Scan(&repo)
	return s.factChange(stateport.Change{Repository: repo}, err)
}

func (s *Store) issueChange(ctx context.Context, tx *sql.Tx, id string) error {
	var c stateport.Change
	err := tx.QueryRowContext(ctx, `SELECT repo,upstream FROM issues WHERE id=?`, id).Scan(&c.Repository, &c.Upstream)
	if c.Upstream == "global" {
		c.Upstream = ""
	}
	return s.factChange(c, err)
}

func (s *Store) factChange(c stateport.Change, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err == nil {
		s.noteChange(c)
	}
	return databaseError(err)
}
