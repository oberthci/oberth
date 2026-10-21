package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

var ErrRepositoryArchived = errors.New("repository archived")

// Archive state is the latest immutable lifecycle transition, scoped by the
// repository's durable ID. The partial index bounds reads to these actions.
const repositoryArchiveAction = ` FROM audit_actions
WHERE resource_type = 'repository' AND resource_id = CAST(r.id AS TEXT)
AND action IN ('repository.archive', 'repository.unarchive')
ORDER BY id DESC LIMIT 1`
const repositoryArchiveAt = `(SELECT CASE action WHEN 'repository.archive' THEN created_at END` + repositoryArchiveAction + `)`
const repositoryArchiveBy = `COALESCE((SELECT CASE action WHEN 'repository.archive' THEN actor END` + repositoryArchiveAction + `), '')`
const repositoryColumns = `r.id, r.name, r.upstream_id, r.default_branch, r.created_at, r.updated_at, ` + repositoryArchiveAt + `, ` + repositoryArchiveBy

func requireRepositoryActive(ctx context.Context, q promotionQueryer, id int64) error {
	var archived sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT `+repositoryArchiveAt+` FROM repositories r WHERE r.id = ?`, id).Scan(&archived); err != nil {
		return translateNotFound("repository", err)
	}
	if archived.Valid {
		return fmt.Errorf("%w: %w", ErrInvalidState, ErrRepositoryArchived)
	}
	return nil
}

// SetRepositoryArchived keeps the identity and all historical evidence. The
// write reservation serializes archive checks with every new run/promotion.
func (s *Store) SetRepositoryArchived(ctx context.Context, actor, name string, archived bool) (model.Repository, error) {
	if strings.TrimSpace(actor) == "" || strings.TrimSpace(name) == "" {
		return model.Repository{}, fmt.Errorf("%w: actor and repository are required", ErrInvalid)
	}
	selected, err := s.RepositoryByName(ctx, name)
	if err != nil {
		return model.Repository{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Repository{}, fmt.Errorf("begin repository archive: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE repositories SET updated_at = updated_at WHERE id = ?`, selected.ID); err != nil {
		return model.Repository{}, err
	}
	current, err := scanRepository(tx.QueryRowContext(ctx, `SELECT `+repositoryColumns+` FROM repositories r WHERE r.id = ?`, selected.ID))
	if err != nil {
		return model.Repository{}, err
	}
	if (current.ArchivedAt != nil) == archived {
		return current, nil
	}
	if archived {
		var busy bool
		if err := tx.QueryRowContext(ctx, `SELECT
   EXISTS(SELECT 1 FROM runs WHERE repo_id = ? AND status IN ('queued','running')) OR
   EXISTS(SELECT 1 FROM promotions WHERE repo_id = ? AND status = 'pending') OR
   EXISTS(SELECT 1 FROM publications WHERE repo_id = ? AND status = 'pending') OR
   EXISTS(SELECT 1 FROM run_cancellations c JOIN runs r ON r.id = c.run_id WHERE r.repo_id = ? AND c.completed_at IS NULL)`, current.ID, current.ID, current.ID, current.ID).Scan(&busy); err != nil {
			return model.Repository{}, fmt.Errorf("check repository activity: %w", err)
		}
		if busy {
			return model.Repository{}, fmt.Errorf("%w: repository has in-flight runs, pending promotions/publications or cancellation cleanup", ErrInvalidState)
		}
	}
	now := unixNano(s.now())
	action := "repository.unarchive"
	if archived {
		action = "repository.archive"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE repositories SET updated_at = ? WHERE id = ?`, now, current.ID); err != nil {
		return model.Repository{}, err
	}
	details, err := json.Marshal(map[string]any{"name": current.Name, "archived": archived, "upstream_id": current.UpstreamID})
	if err != nil {
		return model.Repository{}, err
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: action, ResourceType: "repository", ResourceID: fmt.Sprint(current.ID), Details: string(details)}, now); err != nil {
		return model.Repository{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Repository{}, fmt.Errorf("commit repository archive: %w", err)
	}
	return s.Repository(ctx, current.ID)
}
