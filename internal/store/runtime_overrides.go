package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// Runtime overrides are server-owned rows (#1497). Helm and flags provide only
// the boot default, so no Kubernetes object is shared between an upgrade and
// an administrator change. Every change is audited in its own transaction.

const maxRuntimeOverrideReason = 512

func validRuntimeOverrideReason(reason string) bool {
	return strings.TrimSpace(reason) != "" && len(reason) <= maxRuntimeOverrideReason
}

const releaseWIFOverrideSelect = `SELECT repo,revoked,grant_json,reason,actor,updated_at FROM release_wif_overrides`

func scanReleaseWIFOverride(scan func(...any) error) (model.ReleaseWIFOverride, error) {
	var value model.ReleaseWIFOverride
	var grant string
	var updated int64
	if err := scan(&value.Repo, &value.Revoked, &grant, &value.Reason, &value.Actor, &updated); err != nil {
		return value, err
	}
	if !value.Revoked {
		value.Grant = json.RawMessage(grant)
	}
	value.UpdatedAt = fromUnixNano(updated)
	return value, nil
}

// ReleaseWIFOverrides returns every runtime release WIF decision by repository.
func (s *Store) ReleaseWIFOverrides(ctx context.Context) ([]model.ReleaseWIFOverride, error) {
	return releaseWIFOverrides(ctx, s.reader())
}

func releaseWIFOverrides(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]model.ReleaseWIFOverride, error) {
	rows, err := q.QueryContext(ctx, releaseWIFOverrideSelect+` ORDER BY repo`)
	if err != nil {
		return nil, fmt.Errorf("read release WIF overrides: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var values []model.ReleaseWIFOverride
	for rows.Next() {
		value, err := scanReleaseWIFOverride(rows.Scan)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// SetReleaseWIFOverride grants or revokes one repository's release WIF entry
// and records the previous override and the previous and new effective
// entries in the same transaction. The caller validates the grant against the
// boot capability map before calling.
func (s *Store) SetReleaseWIFOverride(ctx context.Context, actor string, spec model.ReleaseWIFOverrideSpec) (model.ReleaseWIFOverride, error) {
	if strings.TrimSpace(actor) == "" || len(spec.Repo) < 5 || len(spec.Repo) > 512 || !validRuntimeOverrideReason(spec.Reason) {
		return model.ReleaseWIFOverride{}, fmt.Errorf("%w: actor, repository and a 1..512 byte reason are required", ErrInvalid)
	}
	grant := "{}"
	if !spec.Revoked {
		var compact bytes.Buffer
		if len(spec.Grant) == 0 || json.Compact(&compact, spec.Grant) != nil || compact.Len() > 16384 || compact.Bytes()[0] != '{' || compact.String() == "{}" {
			return model.ReleaseWIFOverride{}, fmt.Errorf("%w: release WIF grant must be a nonempty JSON object of at most 16 KiB", ErrInvalid)
		}
		grant = compact.String()
	}
	action := "release_wif.grant"
	if spec.Revoked {
		action = "release_wif.revoke"
	}
	return s.changeReleaseWIFOverride(ctx, actor, spec, action, func(tx *sql.Tx, now int64) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO release_wif_overrides(repo,revoked,grant_json,reason,actor,updated_at) VALUES(?,?,?,?,?,?)
ON CONFLICT(repo) DO UPDATE SET revoked=excluded.revoked,grant_json=excluded.grant_json,reason=excluded.reason,actor=excluded.actor,updated_at=excluded.updated_at`,
			spec.Repo, spec.Revoked, grant, spec.Reason, actor, now)
		return err
	}, func(details map[string]any) {
		if !spec.Revoked {
			details["grant"] = json.RawMessage(grant)
		}
	})
}

// ClearReleaseWIFOverride removes the runtime decision so the boot default
// applies again. The audit record retains the cleared override. Only Repo,
// Reason and the effective entries of spec are used.
func (s *Store) ClearReleaseWIFOverride(ctx context.Context, actor string, spec model.ReleaseWIFOverrideSpec) (model.ReleaseWIFOverride, error) {
	if strings.TrimSpace(actor) == "" || spec.Repo == "" || !validRuntimeOverrideReason(spec.Reason) {
		return model.ReleaseWIFOverride{}, fmt.Errorf("%w: actor, repository and a 1..512 byte reason are required", ErrInvalid)
	}
	return s.changeReleaseWIFOverride(ctx, actor, spec, "release_wif.reset", func(tx *sql.Tx, _ int64) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM release_wif_overrides WHERE repo=?`, spec.Repo)
		if err != nil {
			return err
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			if err == nil {
				err = fmt.Errorf("%w: repository has no runtime release WIF override", ErrInvalidState)
			}
			return err
		}
		return nil
	}, nil)
}

func (s *Store) changeReleaseWIFOverride(ctx context.Context, actor string, spec model.ReleaseWIFOverrideSpec, action string, change func(*sql.Tx, int64) error, describe func(map[string]any)) (model.ReleaseWIFOverride, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ReleaseWIFOverride{}, err
	}
	defer func() { _ = tx.Rollback() }()
	repo := spec.Repo
	previous, err := scanReleaseWIFOverride(tx.QueryRowContext(ctx, releaseWIFOverrideSelect+` WHERE repo=?`, repo).Scan)
	hadPrevious := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return model.ReleaseWIFOverride{}, err
	}
	now := unixNano(s.now())
	if err := change(tx, now); err != nil {
		return model.ReleaseWIFOverride{}, err
	}
	details := map[string]any{"repo": repo, "reason": spec.Reason}
	if hadPrevious {
		details["previous"] = previous
	}
	// The effective entries cover a Helm grant the change replaced or removed,
	// which no override row records. An absent entry is recorded as null.
	for key, value := range map[string]json.RawMessage{"previous_effective": spec.PreviousEffective, "effective": spec.Effective} {
		if len(value) == 0 || !json.Valid(value) {
			value = json.RawMessage("null")
		}
		details[key] = value
	}
	if describe != nil {
		describe(details)
	}
	body, err := json.Marshal(details)
	if err != nil {
		return model.ReleaseWIFOverride{}, err
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: actor, Action: action, ResourceType: "release_wif", ResourceID: repo, Details: string(body)}, now); err != nil {
		return model.ReleaseWIFOverride{}, err
	}
	result := previous
	if action != "release_wif.reset" {
		result, err = scanReleaseWIFOverride(tx.QueryRowContext(ctx, releaseWIFOverrideSelect+` WHERE repo=?`, repo).Scan)
		if err != nil {
			return model.ReleaseWIFOverride{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return model.ReleaseWIFOverride{}, err
	}
	return result, nil
}

// AdmissionCapacityOverride returns the runtime ceiling a v0.19 or older
// server stored, or nil. Since v0.20.0 nothing applies or sets it; the server
// reads it once at startup to warn that it no longer has an effect.
func (s *Store) AdmissionCapacityOverride(ctx context.Context) (*model.AdmissionCapacityOverride, error) {
	return admissionCapacityOverride(ctx, s.reader())
}

func admissionCapacityOverride(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (*model.AdmissionCapacityOverride, error) {
	var value model.AdmissionCapacityOverride
	var updated int64
	err := q.QueryRowContext(ctx, `SELECT max_concurrent_jobs,reason,actor,updated_at FROM admission_capacity_override WHERE singleton=1`).Scan(&value.MaxConcurrentJobs, &value.Reason, &value.Actor, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read admission capacity override: %w", err)
	}
	value.UpdatedAt = fromUnixNano(updated)
	return &value, nil
}
