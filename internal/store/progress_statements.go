package store

import (
	"context"
	"database/sql"
	"errors"
)

type progressStatements struct{ queries map[string]*sql.Stmt }

func prepareProgressStatements(ctx context.Context, writer *sql.DB) (*progressStatements, error) {
	result := &progressStatements{queries: map[string]*sql.Stmt{}}
	for _, query := range []string{
		runReadPrefix + "admission" + runReadSuffix,
		`SELECT value FROM native_steps WHERE run_id=?`,
		`INSERT INTO native_steps VALUES(?,?) ON CONFLICT(run_id) DO UPDATE SET value=excluded.value`,
		`UPDATE runs SET status=?,summary=? WHERE id=?`,
		`SELECT coalesce((SELECT seq FROM sqlite_sequence WHERE name='events'),0)+1`,
		progressCountsSQL,
		`SELECT id,value FROM events WHERE dedupe=?`,
		`INSERT INTO events(repo,kind,dedupe,at,value) VALUES(?,?,?,?,?)`,
	} {
		stmt, err := writer.PrepareContext(ctx, query)
		if err != nil {
			return nil, errors.Join(databaseError(err), result.close())
		}
		result.queries[query] = stmt
	}
	return result, nil
}

func (q *progressStatements) close() error {
	var errs []error
	for _, stmt := range q.queries {
		errs = append(errs, stmt.Close())
	}
	return errors.Join(errs...)
}

// Prepared SQL retains no row values; every call still uses the current transaction.
func (s *Store) progressRow(ctx context.Context, tx *sql.Tx, query string, args ...any) *sql.Row {
	if s.progress != nil {
		if stmt := s.progress.queries[query]; stmt != nil {
			return tx.StmtContext(ctx, stmt).QueryRowContext(ctx, args...)
		}
	}
	return tx.QueryRowContext(ctx, query, args...)
}

func (s *Store) progressExec(ctx context.Context, tx *sql.Tx, query string, args ...any) (sql.Result, error) {
	if s.progress != nil {
		if stmt := s.progress.queries[query]; stmt != nil {
			return tx.StmtContext(ctx, stmt).ExecContext(ctx, args...)
		}
	}
	return tx.ExecContext(ctx, query, args...)
}
