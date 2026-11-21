package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// PreparePromotion commits public evidence and private caller authority together.
func (s *Store) PreparePromotion(ctx context.Context, a stateport.Action, p model.Promotion) error {
	if p.ID == "" || !validRepository(p.Repo) || p.Actor != a.Actor || p.Caller.Identity != a.Actor || p.ResultSHA == "" {
		return refuse(invalid, nil)
	}
	p = p.Clone()
	p.Revision = 1
	p.Status = model.PromotionPending
	return s.change(ctx, mutation{a.Actor, "promotion.prepare", "promotion", p.ID, map[string]string{"reason": a.Reason}}, func(tx *sql.Tx) error {
		id, err := repositoryNumber(ctx, tx, p.Repo)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO promotions(id,repo_id,revision,status,caller,value) VALUES(?,?,?,?,?,?)`, p.ID, id, p.Revision, p.Status, encode(p.Caller), encode(p))
		if err != nil {
			return refuse(conflict, err)
		}
		return nil
	}, a.Events...)
}
func readPromotion(ctx context.Context, tx *sql.Tx, id string) (model.Promotion, error) {
	var p model.Promotion
	var body, caller string
	if err := tx.QueryRowContext(ctx, `SELECT value,caller FROM promotions WHERE id=?`, id).Scan(&body, &caller); err != nil {
		return p, databaseError(err)
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		return p, refuse(corrupt, err)
	}
	if err := json.Unmarshal([]byte(caller), &p.Caller); err != nil {
		return p, refuse(corrupt, err)
	}
	return p, nil
}
func savePromotion(ctx context.Context, tx *sql.Tx, p model.Promotion) error {
	var run any
	if p.RunID != "" {
		run = p.RunID
	}
	_, err := tx.ExecContext(ctx, `UPDATE promotions SET run_id=?,revision=?,status=?,value=? WHERE id=?`, run, p.Revision, p.Status, encode(p), p.ID)
	return databaseError(err)
}
func (s *Store) Promotion(ctx context.Context, id string) (model.Promotion, error) {
	tx, err := s.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.Promotion{}, databaseError(err)
	}
	defer func() { _ = tx.Rollback() }()
	return readPromotion(ctx, tx, id)
}
func (s *Store) PromotionForRun(ctx context.Context, id string) (model.Promotion, error) {
	var key string
	if err := s.readers.QueryRowContext(ctx, `SELECT id FROM promotions WHERE run_id=?`, id).Scan(&key); err != nil {
		return model.Promotion{}, databaseError(err)
	}
	return s.Promotion(ctx, key)
}
func (s *Store) FinishPromotion(ctx context.Context, a stateport.Action, q stateport.PromotionUpdate) error {
	return s.change(ctx, mutation{a.Actor, "promotion.finish", "promotion", q.ID, map[string]string{"reason": a.Reason}}, func(tx *sql.Tx) error {
		if err := s.finishPromotion(ctx, tx, q); err != nil {
			return err
		}
		if q.Publication != nil {
			return s.preparePublication(ctx, tx, *q.Publication, a)
		}
		return nil
	}, a.Events...)
}
func (s *Store) finishPromotion(ctx context.Context, tx *sql.Tx, q stateport.PromotionUpdate) error {
	p, err := readPromotion(ctx, tx, q.ID)
	if err != nil {
		return err
	}
	if p.Revision != q.Revision || q.RunID != "" && p.RunID != "" && q.RunID != p.RunID {
		return refuse(staleRevision, nil)
	}
	if !q.Status.Valid() {
		return refuse(invalid, nil)
	}
	p.Revision++
	p.Status = q.Status
	p.Reason = q.Reason
	p.Detail = q.Detail
	if q.RunID != "" {
		p.RunID = q.RunID
	}
	return savePromotion(ctx, tx, p)
}
func (s *Store) linkPromotion(ctx context.Context, tx *sql.Tx, a stateport.Acceptance, r stateport.Run) error {
	if a.PromotionID == "" {
		return nil
	}
	p, err := readPromotion(ctx, tx, a.PromotionID)
	if err != nil {
		return err
	}
	if p.Repo != r.Repo || p.ResultSHA != r.ObjectSHA || r.CommitSHA == nil || *r.CommitSHA != p.ResultSHA || r.Origin.Trigger != model.TriggerPromotion {
		return refuse(invalid, nil)
	}
	if p.RunID != "" && p.RunID != r.ID {
		return refuse(conflict, nil)
	}
	p.RunID = r.ID
	p.Revision++
	return savePromotion(ctx, tx, p)
}
func (s *Store) Promotions(ctx context.Context, f stateport.RecordFilter) ([]model.Promotion, error) {
	query, args, err := recordQuery(f, "promotions")
	if err != nil {
		return nil, err
	}
	rows, err := s.readers.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, databaseError(err)
	}
	defer func() { _ = rows.Close() }()
	out := []model.Promotion{}
	for rows.Next() {
		var body, caller string
		var p model.Promotion
		if err := rows.Scan(&body, &caller); err != nil {
			return nil, databaseError(err)
		}
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			return nil, refuse(corrupt, err)
		}
		if err := json.Unmarshal([]byte(caller), &p.Caller); err != nil {
			return nil, refuse(corrupt, err)
		}
		out = append(out, p)
	}
	return out, databaseError(rows.Err())
}
