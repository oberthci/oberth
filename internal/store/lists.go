package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func recordQuery(filter stateport.RecordFilter, table string) (string, []any, error) {
	if filter.Limit < 1 || filter.Limit > 200 {
		return "", nil, refuse(invalid, nil)
	}

	scope, args := scopePredicate("r.canonical", filter.Repositories)
	columns := "p.value"
	if table == "promotions" {
		columns += ",p.caller"
	}
	query := "SELECT " + columns + " FROM " + table + " p JOIN repositories r ON r.id=p.repo_id WHERE " + scope
	position, cursor := "p.id", "?"
	columnsOrder := []string{"p.id"}
	if table == "publications" {
		// Stored times are UTC. Dropping Z orders fractional seconds exactly,
		// including whole seconds, without SQLite's millisecond rounding.
		stamp := "rtrim(json_extract(p.value,'$.created_at'),'Z')"
		position = "(" + stamp + ",p.id)"
		cursor = "(SELECT rtrim(json_extract(value,'$.created_at'),'Z'),id FROM publications WHERE id=?)"
		columnsOrder = []string{stamp, "p.id"}
	}
	if filter.After != "" {
		query += " AND " + position + ">" + cursor
		args = append(args, filter.After)
	}
	if filter.Before != "" {
		query += " AND " + position + "<" + cursor
		args = append(args, filter.Before)
	}
	if filter.Status != "" {
		query += " AND p.status=?"
		args = append(args, filter.Status)
	}
	if filter.SourceSHA != "" {
		query += " AND json_extract(p.value,'$.source_sha')=?"
		args = append(args, filter.SourceSHA)
	}
	order := " DESC"
	if filter.After != "" {
		order = " ASC"
	}
	for i := range columnsOrder {
		columnsOrder[i] += order
	}
	return query + " ORDER BY " + strings.Join(columnsOrder, ",") + " LIMIT ?", append(args, filter.Limit), nil
}

func readRecords[T any](ctx context.Context, db *sql.DB, query string, args []any) ([]T, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]T, 0)
	for rows.Next() {
		var body string
		var value T
		if err := rows.Scan(&body); err != nil {
			return nil, databaseError(err)
		}
		if err := json.Unmarshal([]byte(body), &value); err != nil {
			return nil, refuse(corrupt, err)
		}
		result = append(result, value)
	}
	return result, databaseError(rows.Err())
}

func (s *Store) Publications(ctx context.Context, filter stateport.RecordFilter) ([]model.Publication, error) {
	if filter.Status != "" && !model.PublicationStatus(filter.Status).Valid() {
		return nil, refuse(invalid, nil)
	}
	query, args, err := recordQuery(filter, "publications")
	if err != nil {
		return nil, err
	}
	stored, err := readRecords[storedPublication](ctx, s.readers, query, args)
	if err != nil {
		return nil, err
	}
	result := make([]model.Publication, len(stored))
	for i, p := range stored {
		result[i] = p.publication()
	}
	return result, nil
}

func (s *Store) PendingPublications(ctx context.Context, filter stateport.RecordFilter) ([]model.Publication, error) {
	filter.Status = string(model.PublicationPending)
	return s.Publications(ctx, filter)
}

// SecretGrantRecord is a durable approved tuple, including revocation state.
type SecretGrantRecord struct {
	ID int64
	workflowpolicy.GrantKey
	Revoked bool
}

func (s *Store) SecretGrants(ctx context.Context, filter stateport.CatalogFilter) ([]SecretGrantRecord, error) {
	if filter.Limit < 1 || filter.Limit > 200 || filter.Before < 0 {
		return nil, refuse(invalid, nil)
	}
	scope, args := scopePredicate("r.canonical", filter.Repositories)
	query := `SELECT g.id,r.canonical,g.file,g.step,g.path,g.audience,g.revoked FROM secret_grants g
  JOIN repositories r ON r.id=g.repo_id WHERE ` + scope
	if filter.Before > 0 {
		query += ` AND g.id<?`
		args = append(args, filter.Before)
	}
	rows, err := s.readers.QueryContext(ctx, query+` ORDER BY g.id DESC LIMIT ?`, append(args, filter.Limit)...)
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]SecretGrantRecord, 0)
	for rows.Next() {
		var g SecretGrantRecord
		if err := rows.Scan(&g.ID, &g.Repository, &g.File, &g.Step, &g.Path, &g.Audience, &g.Revoked); err != nil {
			return nil, databaseError(err)
		}
		result = append(result, g)
	}
	return result, databaseError(rows.Err())
}

func (s *Store) Capabilities(ctx context.Context, filter stateport.CatalogFilter) ([]CapabilityGrant, error) {
	query, args, err := configurationQuery(filter, "capabilities")
	if err != nil {
		return nil, err
	}
	rows, err := s.readers.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	result := make([]CapabilityGrant, 0)
	for rows.Next() {
		var g CapabilityGrant
		var body string
		if err := rows.Scan(&g.Repository, &body, &g.Revoke); err != nil {
			return nil, databaseError(err)
		}
		g.JSON = []byte(body)
		result = append(result, g)
	}
	return result, databaseError(rows.Err())
}

func configurationQuery(filter stateport.CatalogFilter, table string) (string, []any, error) {
	if filter.Limit < 1 || filter.Limit > 200 || filter.Before < 0 {
		return "", nil, refuse(invalid, nil)
	}
	scope, args := scopePredicate("r.canonical", filter.Repositories)
	query := `SELECT r.canonical,c.value,c.revoked FROM ` + table + ` c JOIN repositories r ON r.id=c.repo_id WHERE ` + scope
	if filter.Before > 0 {
		query += ` AND r.id<?`
		args = append(args, filter.Before)
	}
	return query + ` ORDER BY r.id DESC LIMIT ?`, append(args, filter.Limit), nil
}
