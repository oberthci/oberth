package store

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

const admissionHoldColumns = `on_hold,reason,actor,allow_tag,allow_release,allow_promotion,expires_at,changed_at`

// Only durable claims use this restriction. Read projections retain every
// queued run and its original global priority/FIFO position.
const admissionHoldAllowedSQL = `NOT EXISTS (SELECT 1 FROM admission_holds WHERE singleton=1 AND on_hold=1 AND expires_at>? AND NOT ((allow_tag=1 AND runs.ref_kind='tag') OR (allow_release=1 AND runs.release=1) OR (allow_promotion=1 AND runs.trigger='promotion')))`

func scanAdmissionHold(row rowScanner) (model.AdmissionHold, error) {
	hold := model.AdmissionHold{Allow: []string{}}
	var tag, release, promotion bool
	var expires, changed int64
	if err := row.Scan(&hold.On, &hold.Reason, &hold.Actor, &tag, &release, &promotion, &expires, &changed); err != nil {
		return hold, err
	}
	for _, item := range []struct {
		class   string
		allowed bool
	}{{"tag", tag}, {"release", release}, {"promotion", promotion}} {
		if item.allowed {
			hold.Allow = append(hold.Allow, item.class)
		}
	}
	if expires != 0 {
		hold.ExpiresAt = fromUnixNano(expires)
	}
	if changed != 0 {
		hold.ChangedAt = fromUnixNano(changed)
	}
	return hold, nil
}
func (s *Store) AdmissionHold(ctx context.Context) (model.AdmissionHold, error) {
	return scanAdmissionHold(s.reader().QueryRowContext(ctx, `SELECT `+admissionHoldColumns+` FROM admission_holds WHERE singleton=1`))
}
func (s *Store) SetAdmissionHold(ctx context.Context, actor string, request model.AdmissionHoldRequest) (model.AdmissionHold, error) {
	request, err := request.Normalize()
	if err != nil {
		return model.AdmissionHold{}, fmt.Errorf("%w: admission hold: %w", ErrInvalid, err)
	}
	if strings.TrimSpace(actor) == "" {
		return model.AdmissionHold{}, fmt.Errorf("%w: admission hold requires an actor", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.AdmissionHold{}, err
	}
	defer func() { _ = tx.Rollback() }()
	previous, err := scanAdmissionHold(tx.QueryRowContext(ctx, `SELECT `+admissionHoldColumns+` FROM admission_holds WHERE singleton=1`))
	if err != nil {
		return model.AdmissionHold{}, err
	}
	now := s.now().UTC()
	hold := model.AdmissionHold{On: request.On, Reason: request.Reason, Allow: request.Allow, Actor: actor, ChangedAt: now}
	var expires int64
	if request.On {
		hold.ExpiresAt = now.Add(request.Duration)
		expires = unixNano(hold.ExpiresAt)
	}
	_, err = tx.ExecContext(ctx, `UPDATE admission_holds SET on_hold=?,reason=?,actor=?,allow_tag=?,allow_release=?,allow_promotion=?,expires_at=?,changed_at=? WHERE singleton=1`, hold.On, hold.Reason, actor, slices.Contains(hold.Allow, "tag"), slices.Contains(hold.Allow, "release"), slices.Contains(hold.Allow, "promotion"), expires, unixNano(now))
	if err != nil {
		return model.AdmissionHold{}, err
	}
	details, err := json.Marshal(map[string]any{"previous": previous, "hold": hold})
	if err != nil {
		return model.AdmissionHold{}, err
	}
	if _, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: "admission.hold", ResourceType: "admission", ResourceID: "hold", Details: string(details)}, unixNano(now)); err != nil {
		return model.AdmissionHold{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.AdmissionHold{}, fmt.Errorf("commit admission hold: %w", err)
	}
	return hold, nil
}
