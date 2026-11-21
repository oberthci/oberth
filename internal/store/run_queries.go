package store

import (
	"context"
	"database/sql"
	"unicode/utf8"

	"github.com/oberthci/oberth/internal/stateport"
)

func (s *Store) latestRun(ctx context.Context, filter stateport.RunFilter) (stateport.Run, error) {
	filter.Limit = 1
	rows, err := s.Runs(ctx, filter)
	if err != nil {
		return stateport.Run{}, err
	}
	if len(rows) == 0 {
		return stateport.Run{}, refuse(missing, nil)
	}
	return rows[0], nil
}

func (s *Store) RunByRef(ctx context.Context, repositories []string, ref string) (stateport.Run, error) {
	if !validRef(ref) {
		return stateport.Run{}, refuse(invalid, nil)
	}
	return s.latestRun(ctx, stateport.RunFilter{Repositories: repositories, Ref: ref})
}

// ResolveRun applies canonical repository scope before testing prefix uniqueness.
func (s *Store) ResolveRun(ctx context.Context, repositories []string, selector string) (stateport.Run, error) {
	if selector == "" || !utf8.ValidString(selector) {
		return stateport.Run{}, refuse(invalid, nil)
	}
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return stateport.Run{}, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	query, args := resolveQuery(repositories, selector)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return stateport.Run{}, databaseError(err)
	}
	ids := make([]string, 0, 2)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return stateport.Run{}, databaseError(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return stateport.Run{}, databaseError(err)
	}
	if err := rows.Close(); err != nil {
		return stateport.Run{}, databaseError(err)
	}
	if len(ids) == 0 {
		return stateport.Run{}, refuse(missing, nil)
	}
	if ids[0] != selector && len(ids) > 1 {
		return stateport.Run{}, refuse(ambiguous, nil)
	}
	return loadRun(ctx, tx, ids[0])
}

// SQLite's BINARY collation orders UTF-8 IDs by bytes.
func prefixEnd(prefix string) string {
	end := []byte(prefix)
	end[len(end)-1]++
	return string(end)
}

func resolveQuery(repositories []string, selector string) (string, []any) {
	scope, args := scopePredicate("r.canonical", repositories)
	// Keep the prefix seek first when repository scope is selective.
	query := `SELECT runs.id FROM runs CROSS JOIN repositories r ON r.id=runs.repo_id WHERE ` + scope +
		` AND runs.id>=? AND runs.id<? ORDER BY runs.id LIMIT 2`
	return query, append(args, selector, prefixEnd(selector))
}
