package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func (s *Store) RegisterUpstreamBinding(ctx context.Context, a stateport.Action, u stateport.Upstream) error {
	return s.RegisterUpstream(ctx, a, u)
}

func (s *Store) UpstreamBindings(ctx context.Context) ([]stateport.Upstream, error) {
	out := []stateport.Upstream{}
	page := Page{Limit: 200}
	for {
		rows, err := s.Upstreams(ctx, page)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if len(rows) < page.Limit {
			return out, nil
		}
		page.After = rows[len(rows)-1].Name
	}
}

func (s *Store) RecordAdministration(ctx context.Context, a stateport.Action, v model.AuditAction) error {
	if v.Actor != "" && v.Actor != a.Actor {
		return refuse(forbidden, nil)
	}
	v.Actor = a.Actor
	if a.Reason != "" {
		var details any
		if v.Details != "" {
			details = json.RawMessage(v.Details)
		}
		body, err := json.Marshal(map[string]any{"reason": a.Reason, "details": details})
		if err != nil {
			return refuse(invalid, err)
		}
		v.Details = string(body)
	}
	return s.RecordAction(ctx, v, a.Events...)
}

// AdministrativeAudit filters scope before the exclusive descending cursor/limit.
func (s *Store) AdministrativeAudit(ctx context.Context, scope []string, f stateport.AdministrativeFilter) ([]stateport.AuditRecord, error) {
	query, args, err := administrativeQuery(scope, f)
	if err != nil {
		return nil, err
	}
	tx, err := s.auditReaders.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := auditHead(ctx, tx); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	out := []stateport.AuditRecord{}
	for rows.Next() {
		a, previous, hash, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		if err := s.checkAuditLink(ctx, tx, stateport.AuditRecord{AuditAction: a, PreviousSHA256: previous, SHA256: hash}); err != nil {
			return nil, err
		}
		out = append(out, stateport.AuditRecord{AuditAction: a, SHA256: hash, PreviousSHA256: previous})
	}
	return out, databaseError(rows.Err())
}

func (s *Store) checkAuditLink(ctx context.Context, tx *sql.Tx, v stateport.AuditRecord) error {
	previous := genesis()
	if v.ID > 1 {
		if err := tx.QueryRowContext(ctx, `SELECT hash FROM audit WHERE id=?`, v.ID-1).Scan(&previous); err != nil {
			return refuse(corrupt, err)
		}
	}
	if previous != v.PreviousSHA256 || v.SHA256 != stateport.AuditDigest(s.options.Protocol.AuditDomain, previous, v.AuditAction) {
		return refuse(corrupt, nil)
	}
	return nil
}
