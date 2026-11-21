package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"

	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/stateport"
)

// Upstream is the shared public repository-connection binding.
type Upstream = stateport.Upstream

// RegisterUpstream refuses changed bindings or a reused trust domain.
func (s *Store) RegisterUpstream(ctx context.Context, action stateport.Action, input stateport.Upstream) error {
	if reponame.ValidateUpstreamName(input.Name) != nil || !validRepository(input.Name+"/"+input.Org+"/repository") || !validUpstream(input) {
		return refuse(invalid, nil)
	}
	return s.change(ctx, mutation{action.Actor, "upstream.register", "upstream", input.Name, input}, func(tx *sql.Tx) error {
		var old stateport.Upstream
		err := tx.QueryRowContext(ctx, `SELECT name,org,kind,base_url,key_name,removed FROM upstreams WHERE name=?`,
			input.Name).Scan(&old.Name, &old.Org, &old.Kind, &old.BaseURL, &old.KeyName, &old.Removed)
		if err == nil {
			if old != input || old.Removed {
				return refuse(upstreamChanged, nil)
			}
			return unchanged{}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return databaseError(err)
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO upstreams(name,org,kind,base_url,key_name) VALUES(?,?,?,?,?)`, input.Name, input.Org,
			input.Kind, input.BaseURL, input.KeyName)
		if err != nil {
			return refuse(upstreamChanged, err)
		}
		return nil
	}, action.Events...)
}

func validUpstream(input stateport.Upstream) bool {
	endpoint, err := url.Parse(input.BaseURL)
	return err == nil && endpoint.Host != "" && endpoint.User == nil && input.Kind != "" && input.KeyName != "" && !input.Removed
}

// RemoveUpstream refuses repository references and retains restrictive named scopes.
func (s *Store) RemoveUpstream(ctx context.Context, action stateport.Action, name string) error {
	return s.change(ctx, mutation{action.Actor, "upstream.remove", "upstream", name, nil}, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM repositories WHERE upstream=?`, name).Scan(&count); err != nil {
			return databaseError(err)
		}
		if count > 0 {
			return refuse(upstreamBound, nil)
		}
		_, err := tx.ExecContext(ctx, `UPDATE upstreams SET removed=1 WHERE name=?`, name)
		return databaseError(err)
	}, action.Events...)
}

func (s *Store) RegisterRepository(ctx context.Context, action stateport.Action, canonical string) (stateport.Repository, error) {
	var result stateport.Repository
	parts := strings.Split(canonical, "/")
	if !validRepository(canonical) {
		return result, refuse(invalid, nil)
	}
	err := s.change(ctx, mutation{action.Actor, "repository.register", "repository", canonical, nil}, func(tx *sql.Tx) error {
		var org string
		if err := tx.QueryRowContext(ctx, `SELECT org FROM upstreams WHERE name=? AND removed=0`, parts[0]).Scan(&org); err != nil {
			return databaseError(err)
		}
		if org != parts[1] {
			return refuse(foreignOrganization, nil)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO repositories(upstream,name,canonical) VALUES(?,?,?) ON CONFLICT(canonical) DO NOTHING`,
			parts[0], parts[2], canonical)
		if err != nil {
			return databaseError(err)
		}
		return tx.QueryRowContext(ctx, `SELECT id,upstream,name,canonical,archived FROM repositories WHERE canonical=?`,
			canonical).Scan(&result.ID, &result.Upstream, &result.Name, &result.Canonical, &result.Archived)
	}, action.Events...)
	return result, err
}

// Repository resolves after scope filtering. Nil scope means all; empty means none.
func (s *Store) Repository(ctx context.Context, selector string, scope []string) (stateport.Repository, error) {
	query, args := scopePredicate("canonical", scope)
	rows, err := s.readers.QueryContext(ctx,
		`SELECT id,upstream,name,canonical,archived FROM repositories WHERE (canonical=? OR name=?) AND `+query, append([]any{selector,
			selector}, args...)...)
	if err != nil {
		return stateport.Repository{}, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	var result stateport.Repository
	count := 0
	for rows.Next() {
		if err := rows.Scan(&result.ID, &result.Upstream, &result.Name, &result.Canonical, &result.Archived); err != nil {
			return result, databaseError(err)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return result, databaseError(err)
	}
	if count == 0 {
		return result, refuse(missing, nil)
	}
	if count > 1 {
		return result, refuse(ambiguous, nil)
	}
	return result, nil
}

func scopePredicate(column string, scope []string) (string, []any) {
	if scope == nil {
		return "1", nil
	}
	if len(scope) == 0 {
		return "0", nil
	}
	placeholders := make([]string, len(scope))
	args := make([]any, len(scope))
	for i, value := range scope {
		placeholders[i] = "?"
		args[i] = value
	}
	return column + " IN (" + strings.Join(placeholders, ",") + ")", args
}

func (s *Store) ArchiveRepository(ctx context.Context, action stateport.Action, id int64) error {
	return s.change(ctx, mutation{action.Actor, "repository.archive", "repository", formatID(id), nil}, func(tx *sql.Tx) error {
		if _, err := repositoryID(ctx, tx, id); err != nil {
			return err
		}
		var count int
		err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE repo_id=? AND (status IN ('queued','running') OR EXISTS(SELECT 1 FROM
obligations WHERE run_id=runs.id AND value<>''))) OR EXISTS(SELECT 1 FROM publications WHERE repo_id=? AND
status='pending')`, id, id).Scan(&count)
		if err != nil {
			return databaseError(err)
		}
		if count > 0 {
			return refuse(repositoryBusy, nil)
		}
		_, err = tx.ExecContext(ctx, `UPDATE repositories SET archived=1 WHERE id=?`, id)
		return databaseError(err)
	}, action.Events...)
}

// CheckRepositoryRemoval applies the removal rule and rolls back every cascade.
func (s *Store) CheckRepositoryRemoval(ctx context.Context, id int64) error {
	tx, done, err := s.beginWrite(ctx)
	if err != nil {
		return err
	}
	defer done()
	return removeRepository(ctx, tx, id)
}

func (s *Store) RemoveRepository(ctx context.Context, action stateport.Action, id int64) error {
	return s.change(ctx, mutation{action.Actor, "repository.remove", "repository", formatID(id), nil}, func(tx *sql.Tx) error {
		return removeRepository(ctx, tx, id)
	}, action.Events...)
}

func removeRepository(ctx context.Context, tx *sql.Tx, id int64) error {
	canonical, err := repositoryID(ctx, tx, id)
	if err != nil {
		return err
	}
	var referenced bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issues WHERE repo=?)`, canonical).Scan(&referenced); err != nil {
		return databaseError(err)
	}
	if referenced {
		return refuse(repositoryBound, nil)
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM repositories WHERE id=?`, id)
	if err != nil {
		return refuse(repositoryBound, err)
	}
	return nil
}

func validRepository(value string) bool { _, err := reponame.ParseRepository(value); return err == nil }

func repositoryID(ctx context.Context, tx *sql.Tx, id int64) (string, error) {
	var canonical string
	err := tx.QueryRowContext(ctx, `SELECT canonical FROM repositories WHERE id=?`, id).Scan(&canonical)
	return canonical, databaseError(err)
}
