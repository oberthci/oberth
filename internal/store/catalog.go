package store

import (
	"context"
	"database/sql"

	"github.com/oberthci/oberth/internal/stateport"
)

// Page bounds configuration lists by their stable string key.
type Page struct {
	After string
	Limit int
}

func validPage(page Page) bool { return page.Limit > 0 && page.Limit <= 200 }

func (s *Store) Upstreams(ctx context.Context, page Page) ([]stateport.Upstream, error) {
	if !validPage(page) {
		return nil, refuse(invalid, nil)
	}
	rows, err := s.readers.QueryContext(ctx,
		`SELECT name,org,kind,base_url,key_name,removed FROM upstreams WHERE removed=0 AND name>? ORDER BY name LIMIT ?`, page.After, page.Limit)
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]stateport.Upstream, 0)
	for rows.Next() {
		var u stateport.Upstream
		if err := rows.Scan(&u.Name, &u.Org, &u.Kind, &u.BaseURL, &u.KeyName, &u.Removed); err != nil {
			return nil, databaseError(err)
		}
		result = append(result, u)
	}
	return result, databaseError(rows.Err())
}

func (s *Store) Repositories(ctx context.Context, filter stateport.CatalogFilter) ([]stateport.Repository, error) {
	if filter.Limit < 1 || filter.Limit > 200 || filter.Before < 0 {
		return nil, refuse(invalid, nil)
	}
	scope, args := scopePredicate("canonical", filter.Repositories)
	query := `SELECT id,upstream,name,canonical,archived FROM repositories WHERE ` + scope
	if filter.Before > 0 {
		query += ` AND id<?`
		args = append(args, filter.Before)
	}
	if !filter.IncludeArchived {
		query += ` AND archived=0`
	}
	rows, err := s.readers.QueryContext(ctx, query+` ORDER BY id DESC LIMIT ?`, append(args, filter.Limit)...)
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]stateport.Repository, 0)
	for rows.Next() {
		var r stateport.Repository
		if err := rows.Scan(&r.ID, &r.Upstream, &r.Name, &r.Canonical, &r.Archived); err != nil {
			return nil, databaseError(err)
		}
		result = append(result, r)
	}
	return result, databaseError(rows.Err())
}

func (s *Store) UnarchiveRepository(ctx context.Context, action stateport.Action, id int64) error {
	return s.change(ctx, mutation{action.Actor, "repository.unarchive", "repository", formatID(id), nil}, func(tx *sql.Tx) error {
		if _, err := repositoryID(ctx, tx, id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE repositories SET archived=0 WHERE id=?`, id)
		return databaseError(err)
	}, action.Events...)
}

// KeyFilter lists installation identities without changing their authority.
type KeyFilter struct {
	Identity string
	Page     Page
}

func (s *Store) Keys(ctx context.Context, filter KeyFilter) ([]stateport.Key, error) {
	if !validPage(filter.Page) {
		return nil, refuse(invalid, nil)
	}
	query := `SELECT fingerprint,identity,admin,scope,kind,park_on_push,revoked,revoked_at,expires_at FROM keys WHERE fingerprint>?`
	args := []any{filter.Page.After}
	if filter.Identity != "" {
		query += ` AND identity=?`
		args = append(args, filter.Identity)
	}
	rows, err := s.readers.QueryContext(ctx, query+` ORDER BY fingerprint LIMIT ?`, append(args, filter.Page.Limit)...)
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]stateport.Key, 0)
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, k)
	}
	return result, databaseError(rows.Err())
}

// stateport.Upstream reads the exact active public connection binding.
func (s *Store) Upstream(ctx context.Context, name string) (stateport.Upstream, error) {
	var u stateport.Upstream
	err := s.readers.QueryRowContext(ctx, `SELECT name,org,kind,base_url,key_name,removed
  FROM upstreams WHERE name=? AND removed=0`, name).Scan(&u.Name, &u.Org, &u.Kind, &u.BaseURL, &u.KeyName, &u.Removed)
	return u, databaseError(err)
}
