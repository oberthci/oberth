package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/oberthci/oberth/internal/model"
)

const prioritySubmissionMigrationVersion = 39
const prioritySubmissionSchema = `
CREATE TABLE IF NOT EXISTS priority_grants (
 identity TEXT PRIMARY KEY,
 max_class TEXT NOT NULL CHECK(max_class IN ('high','highest')),
 expires INTEGER NOT NULL,
 granted_by TEXT NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS run_priority_requests (
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 requested TEXT NOT NULL,
 accepted TEXT NOT NULL CHECK(accepted IN ('normal','high','highest')),
 reason TEXT NOT NULL,
 actor TEXT NOT NULL,
 refusal TEXT NOT NULL
) WITHOUT ROWID;
INSERT OR IGNORE INTO run_priority_requests(run_id,requested,accepted,reason,actor,refusal)
 SELECT id,'',` + legacyBasePrioritySQL + `,` + legacyPriorityReasonSQL + `,actor,'' FROM runs
 WHERE status IN ('queued','running') AND (` + legacyBasePrioritySQL + `) IN ('high','highest');`

const requestedPrioritySQL = `(SELECT accepted FROM run_priority_requests WHERE run_id=id AND requested!='' AND accepted IN ('high','highest') AND refusal='')`

// Explicit priority, including an admin bump, survives later pushes. RC runs
// retain the administrator's existing live classification.
const supersessionEligibleSQL = `NOT EXISTS (SELECT 1 FROM run_priority_overrides WHERE run_id=runs.id) AND NOT EXISTS (SELECT 1 FROM run_priority_requests WHERE run_id=runs.id AND accepted IN ('low','promotion','high','highest')) AND (` + basePrioritySQL + `) IN ('standard','low')`

func (s *Store) SetPriorityGrant(ctx context.Context, actor string, grant model.PriorityGrant) (model.PriorityGrant, error) {
	grant.MaxClass = grant.MaxClass.Canonical()
	if strings.TrimSpace(grant.Identity) == "" || len(grant.Identity) > 256 || strings.IndexFunc(grant.Identity, unicode.IsControl) >= 0 || (grant.MaxClass != model.PriorityPromotion && grant.MaxClass != model.PriorityHigh) || (!grant.Expires.After(s.now()) || grant.Expires.UnixNano() <= s.now().UnixNano()) {
		return model.PriorityGrant{}, fmt.Errorf("%w: identity, promotion/high max_class and future expires required", ErrInvalid)
	}
	grant.GrantedBy = actor
	err := s.priorityGrantMutation(ctx, actor, "priority.grant", grant.Identity, grant, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO priority_grants(identity,max_class,expires,granted_by) VALUES(?,?,?,?) ON CONFLICT(identity) DO UPDATE SET max_class=excluded.max_class,expires=excluded.expires,granted_by=excluded.granted_by`, grant.Identity, grant.MaxClass, unixNano(grant.Expires), actor)
		return err
	})
	return grant, err
}

func (s *Store) RevokePriorityGrant(ctx context.Context, actor, identity string) error {
	if strings.TrimSpace(identity) == "" {
		return fmt.Errorf("%w: identity required", ErrInvalid)
	}
	return s.priorityGrantMutation(ctx, actor, "priority.grant.revoke", identity, map[string]string{"identity": identity}, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM priority_grants WHERE identity=?`, identity)
		return err
	})
}

func (s *Store) priorityGrantMutation(ctx context.Context, actor, action, identity string, details any, change func(*sql.Tx) error) error {
	if strings.TrimSpace(actor) == "" {
		return fmt.Errorf("%w: actor required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err = change(tx); err != nil {
		return err
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return err
	}
	if _, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: action, ResourceType: "priority_grant", ResourceID: identity, Details: string(encoded)}, unixNano(s.now())); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ListPriorityGrants(ctx context.Context) ([]model.PriorityGrant, error) {
	rows, err := s.reader().QueryContext(ctx, `SELECT identity,max_class,expires,granted_by FROM priority_grants ORDER BY identity`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	grants := []model.PriorityGrant{}
	for rows.Next() {
		var g model.PriorityGrant
		var expires int64
		if err = rows.Scan(&g.Identity, &g.MaxClass, &expires, &g.GrantedBy); err != nil {
			return nil, err
		}
		g.Expires = fromUnixNano(expires)
		g.MaxClass = g.MaxClass.Canonical()
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

// Resolve permission in the same transaction as insertion and supersession.
// Refusal is visible metadata only; the accepted push keeps its automatic class.
func (s *Store) submissionPriorityTx(ctx context.Context, tx *sql.Tx, spec model.RunSpec, now int64) (model.RunPriority, string, string, error) {
	if spec.Priority == "" {
		return "", "", "", nil
	}
	accepted := spec.Priority.Canonical()
	reason := strings.TrimSpace(spec.PriorityReason)
	if reason == "" {
		reason = "submission priority request"
	}
	refusal := ""
	if len(reason) > 512 || strings.IndexFunc(reason, unicode.IsControl) >= 0 {
		reason = "submission priority request"
		refusal = "priority refused: reason must be 1..512 bytes without control characters"
	}
	if !accepted.Valid() {
		refusal = "priority refused: choose low, standard, promotion or high"
	}
	admin, err := priorityAdminTx(ctx, tx, spec)
	if err != nil {
		return "", "", "", err
	}
	if !admin && accepted.Rank() > model.PriorityStandard.Rank() && refusal == "" {
		var maximum model.RunPriority
		var expires int64
		err := tx.QueryRowContext(ctx, `SELECT max_class,expires FROM priority_grants WHERE identity=?`, spec.Actor).Scan(&maximum, &expires)
		if err != nil && err != sql.ErrNoRows {
			return "", "", "", err
		}
		if err == sql.ErrNoRows {
			refusal = "priority refused: no admin grant for requesting identity"
		} else if expires <= now {
			refusal = "priority refused: admin grant expired"
		} else if accepted.Rank() > maximum.Rank() {
			refusal = "priority refused: requested class exceeds admin grant"
		}
	}
	if refusal != "" {
		accepted = model.PriorityNormal
	}
	return accepted, reason, refusal, nil
}

func (s *Store) recordSubmissionPriorityTx(ctx context.Context, tx *sql.Tx, spec model.RunSpec, id string, accepted model.RunPriority, reason, refusal string, now int64) error {
	if accepted == "" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO run_priority_requests(run_id,requested,accepted,reason,actor,refusal) VALUES(?,?,?,?,?,?)`, id, spec.Priority, accepted, reason, spec.Actor, refusal); err != nil {
		return err
	}
	details, _ := json.Marshal(map[string]any{"requested": spec.Priority, "priority": accepted, "reason": reason, "refusal": refusal, "supersede": spec.Supersede, "requester_fingerprint": spec.RequesterFingerprint})
	_, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: spec.Actor, Action: "run.priority.request", ResourceType: "run", ResourceID: id, Details: string(details)}, now)
	return err
}

func priorityAdminTx(ctx context.Context, tx *sql.Tx, spec model.RunSpec) (bool, error) {
	if spec.UnscopedAdmin != nil {
		return *spec.UnscopedAdmin, nil
	}
	if spec.RequesterFingerprint == "" {
		return false, nil
	}
	var admin bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM uplinks u JOIN token_credentials c ON c.id=u.token_credential_id WHERE u.identity=? AND u.fingerprint=? AND u.admin=1 AND (c.revoked_at IS NULL OR EXISTS(SELECT 1 FROM pending_token_credentials WHERE token_credential_id=c.id)) AND NOT EXISTS(SELECT 1 FROM uplink_scopes WHERE uplink_id=u.id))`, spec.Actor, spec.RequesterFingerprint).Scan(&admin)
	return admin, err
}

// Record automatic priority solely as lasting supersession protection. Live RC
// and tag classification still use the existing administrator configuration.
func protectAutomaticPrioritiesTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO run_priority_requests(run_id,requested,accepted,reason,actor,refusal)
 SELECT id,'',`+basePrioritySQL+`,`+priorityReasonSQL+`,actor,'' FROM runs
 WHERE status IN ('queued','running') AND (`+basePrioritySQL+`) IN ('promotion','high')`)
	return err
}

// RequestRunPriority applies sync's optional priority to its existing waiting
// build. Sync itself publishes an existing ref and does not create a build.
func (s *Store) RequestRunPriority(ctx context.Context, actor, id string, request model.PriorityRequest, admitted bool) (model.Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Run{}, err
	}
	defer func() { _ = tx.Rollback() }()
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id=?`, id))
	if err != nil {
		return model.Run{}, err
	}
	spec := model.RunSpec{PriorityRequest: request, Actor: actor}
	now := unixNano(s.now())
	accepted, reason, refusal, err := s.submissionPriorityTx(ctx, tx, spec, now)
	if err != nil {
		return model.Run{}, err
	}
	if admitted || !run.Status.Active() || run.StartedAt != nil {
		refusal = "priority refused: sync build is already admitted or terminal"
	}
	// An unsuccessful sync priority request cannot lower an accepted priority.
	if refusal == "" && accepted.Rank() > run.Priority.Rank() {
		if _, err = tx.ExecContext(ctx, `DELETE FROM run_priority_requests WHERE run_id=?`, id); err != nil {
			return model.Run{}, err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM run_priority_overrides WHERE run_id=?`, id); err != nil {
			return model.Run{}, err
		}
		if err = s.recordSubmissionPriorityTx(ctx, tx, spec, id, accepted, reason, "", now); err != nil {
			return model.Run{}, err
		}
	} else {
		details, _ := json.Marshal(map[string]any{"requested": request.Priority, "priority": run.Priority, "reason": reason, "refusal": refusal})
		if _, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: "run.priority.request", ResourceType: "run", ResourceID: id, Details: string(details)}, now); err != nil {
			return model.Run{}, err
		}
	}
	run, err = scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id=?`, id))
	if err != nil {
		return model.Run{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Run{}, err
	}
	run.PriorityRefusal = refusal
	return run, nil
}
