package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

// ChangeGrant commits the exact authority tuple and one coherent generation.
func (s *Store) ChangeGrant(ctx context.Context, a stateport.Action, g stateport.GrantChange) (stateport.AuthoritySnapshot, error) {
	if !g.Kind.Valid() || !validRepository(g.Repository) || len(g.Capability) > maxCapabilityBytes {
		return stateport.AuthoritySnapshot{}, refuse(invalid, nil)
	}
	var snapshot stateport.AuthoritySnapshot
	err := s.change(ctx, mutation{a.Actor, "authority.changed", "authority", g.Repository, map[string]any{"reason": a.Reason, "grant": g}}, func(tx *sql.Tx) error {
		id, err := repositoryNumber(ctx, tx, g.Repository)
		if err != nil {
			return err
		}
		if g.Kind == stateport.GrantPVC {
			if err := claimGrant(ctx, tx, id, g); err != nil {
				return err
			}
		}
		if err := writeGrant(ctx, tx, id, g); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO authority_revision VALUES(1,1) ON CONFLICT(singleton) DO UPDATE SET revision=revision+1`); err != nil {
			return databaseError(err)
		}
		snapshot, err = grantSnapshot(ctx, tx, nil)
		return err
	}, a.Events...)
	if err != nil {
		return stateport.AuthoritySnapshot{}, err
	}
	return snapshot, nil
}

func claimGrant(ctx context.Context, tx *sql.Tx, id int64, g stateport.GrantChange) error {
	if g.Path == "" || g.Tier != "ci" && g.Tier != "release" {
		return refuse(invalid, nil)
	}
	var owner int64
	var tier string
	err := tx.QueryRowContext(ctx, `SELECT repo_id,tier FROM pvc_owners WHERE claim=?`, g.Path).Scan(&owner, &tier)
	if err == nil {
		if owner != id || tier != g.Tier {
			return refuse(claimBound, nil)
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return databaseError(err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO pvc_owners VALUES(?,?,?)`, g.Path, id, g.Tier)
	return databaseError(err)
}

func writeGrant(ctx context.Context, tx *sql.Tx, id int64, g stateport.GrantChange) error {
	args := []any{g.Kind, id, g.File, g.Step, g.Path, g.Tier}
	if g.Dismiss {
		old, err := storedGrant(ctx, tx, args)
		if g.Dismiss && err == nil && (!old.Dismiss || old.Revoke) {
			reason, remedy := stateport.GrantDismissalRefusal()
			return publicerr.New(publicerr.InvalidInput, reason, remedy, nil)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return databaseError(err)
		}
	}

	_, err := tx.ExecContext(ctx, `INSERT INTO authority_grants VALUES(?,?,?,?,?,?,?) ON CONFLICT(kind,repo_id,file,step,path,tier) DO UPDATE SET value=excluded.value`, append(args, encode(g))...)
	return databaseError(err)
}

func storedGrant(ctx context.Context, tx *sql.Tx, args []any) (stateport.GrantChange, error) {
	var body string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM authority_grants WHERE kind=? AND repo_id=? AND file=? AND step=? AND path=? AND tier=?`, args...).Scan(&body); err != nil {
		return stateport.GrantChange{}, err
	}
	var g stateport.GrantChange
	if json.Unmarshal([]byte(body), &g) != nil {
		return g, refuse(corrupt, nil)
	}
	return g, nil
}

func grantSnapshot(ctx context.Context, tx *sql.Tx, scope []string) (stateport.AuthoritySnapshot, error) {
	out := stateport.AuthoritySnapshot{Grants: []stateport.GrantChange{}}
	if err := tx.QueryRowContext(ctx, `SELECT coalesce((SELECT revision FROM authority_revision WHERE singleton=1),0)`).Scan(&out.Revision); err != nil {
		return out, databaseError(err)
	}
	where, args := scopePredicate("r.canonical", scope)
	rows, err := tx.QueryContext(ctx, `SELECT g.value FROM authority_grants g JOIN repositories r ON r.id=g.repo_id WHERE `+where+` ORDER BY g.kind,r.canonical,g.file,g.step,g.path,g.tier`, args...)
	if err != nil {
		return out, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var body string
		var g stateport.GrantChange
		if err := rows.Scan(&body); err != nil {
			return out, databaseError(err)
		}
		if err := json.Unmarshal([]byte(body), &g); err != nil {
			return out, refuse(corrupt, err)
		}
		out.Grants = append(out.Grants, g)
	}
	return out, databaseError(rows.Err())
}

func (s *Store) Grants(ctx context.Context, scope []string) (stateport.AuthoritySnapshot, error) {
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return stateport.AuthoritySnapshot{}, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	return grantSnapshot(ctx, tx, scope)
}
