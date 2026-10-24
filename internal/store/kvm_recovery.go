package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"

	"github.com/oberthci/oberth/internal/model"
)

const kvmRestartRetryAction = "kvm.restart-retry"

// KVMRestartRetryReason marks a parent whose fresh attempt is linked through
// superseded_by; presentation shows it as re-queued, not as needing a retry.
const KVMRestartRetryReason = "retried once after trusted preflight owner restart"

// RecoverKVMPreflight settles an undelivered, cleanup-qualified parent and at
// most one fresh execution of its exact intent. Interruption, retry and audit
// lineage commit together; neither a successful old receipt nor an exec stream
// is adopted. AdmissionSequence survives ordinary restart re-fires, so it also
// bounds retries when the replacement restarts before reserving its own VM.
func (s *Store) RecoverKVMPreflight(ctx context.Context, runID string) (model.Run, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Run{}, err
	}
	defer func() { _ = tx.Rollback() }()
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id=?`, runID))
	if err != nil {
		return model.Run{}, err
	}
	if run.Status != model.RunRunning {
		return model.Run{}, nil
	}
	execution, err := scanKVMExecution(tx.QueryRowContext(ctx, `SELECT `+kvmExecutionColumns+` FROM vm_executions WHERE run_id=? AND execution_kind='kvm-preflight'`, runID))
	if err != nil {
		return model.Run{}, err
	}
	if !execution.Cleaned || run.JobName != "" || execution.Intent.RunID != run.ID || execution.Intent.RepoID != run.RepoID || execution.Intent.CandidateSHA != run.TestedSHA {
		return model.Run{}, ErrInvalidState
	}
	var publications int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM publications WHERE run_id=?`, run.ID).Scan(&publications); err != nil {
		return model.Run{}, err
	}
	if publications != 0 {
		return model.Run{}, ErrInvalidState
	}
	now := unixNano(s.now())
	// Legacy cleaned intents can lack a refusal. They still receive the negative
	// recovery audit below; immutable positive/negative execution evidence stays.
	run, err = s.finishRunTx(ctx, tx, run.ID, model.RunResult{Status: model.RunInterrupted, Error: "trusted preflight interrupted by owner restart"}, "interrupted", now)
	if err != nil {
		return model.Run{}, err
	}
	var next model.Run
	reason := "retry limit reached"
	var used int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_actions WHERE action=? AND json_extract(details,'$.admission_sequence')=?`, kvmRestartRetryAction, run.AdmissionSequence).Scan(&used); err != nil {
		return model.Run{}, err
	}
	if execution.Refusal != nil && execution.Refusal.Code != "canceled" {
		used = 1
		reason = "trusted preflight refused before owner restart"
	}
	if used == 0 {
		// Validation failures terminalize without retry; observation/storage failures
		// roll the whole settlement back so recovery remains pending.
		if err = s.kvmRetryAllowedTx(ctx, tx, run, now); err == nil {
			next, err = s.enqueueKVMRetryTx(ctx, tx, run, now)
			if err != nil {
				return model.Run{}, err
			}
			reason = KVMRestartRetryReason
		} else if errors.Is(err, ErrInvalidState) || errors.Is(err, ErrInvalid) || errors.Is(err, ErrNotFound) {
			reason = "automatic trusted preflight retry refused"
		} else {
			return model.Run{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE runs SET reason=?,superseded_by=? WHERE id=?`, reason, next.ID, run.ID); err != nil {
		return model.Run{}, err
	}
	details, err := json.Marshal(map[string]any{"outcome": "interrupted", "reason": "owner_restart", "retry_run_id": next.ID, "admission_sequence": run.AdmissionSequence})
	if err != nil {
		return model.Run{}, err
	}
	if _, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: run.Actor, Action: "kvm.restart-interrupted", ResourceType: "run", ResourceID: run.ID, Details: string(details)}, now); err != nil {
		return model.Run{}, err
	}
	if next.ID != "" {
		details, _ = json.Marshal(map[string]any{"source_run_id": run.ID, "admission_sequence": run.AdmissionSequence})
		if _, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: run.Actor, Action: kvmRestartRetryAction, ResourceType: "run", ResourceID: next.ID, Details: string(details)}, now); err != nil {
			return model.Run{}, err
		}
	}
	if err = tx.Commit(); err != nil {
		return model.Run{}, err
	}
	return next, nil
}

func (s *Store) kvmRetryAllowedTx(ctx context.Context, tx *sql.Tx, run model.Run, now int64) error {
	if err := requireRepositoryActive(ctx, tx, run.RepoID); err != nil {
		return err
	}
	if run.Trigger == "plan" || run.Trigger == "apply" {
		return ErrInvalidState
	}
	var siblings int
	// A newer terminal push must not be replaced either. Operator-created manual
	// attempts have their own admission order; automatic recovery keeps this one.
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE repo_id=? AND ref_kind=? AND ref=? AND id!=? AND schedule_entry=? AND (status IN ('queued','running') OR queue_sequence>?)`, run.RepoID, run.RefKind, run.Ref, run.ID, run.ScheduleEntry, run.QueueSequence).Scan(&siblings); err != nil {
		return err
	}
	if siblings != 0 {
		return ErrInvalidState
	}
	if run.RefKind == model.RefTag {
		return requireUnpublishedTagTx(ctx, tx, run)
	}
	if run.Trigger != "promotion" {
		return nil
	}
	promotion, err := promotionByRunTx(ctx, tx, run.ID)
	if err != nil {
		return err
	}
	spec := preflightPromotionSpec(promotion)
	if _, err = checkPromotionRetryTx(ctx, tx, spec); err != nil {
		return err
	}
	if err = requireKVMRetryPromotionAuthority(ctx, tx, promotion); err != nil {
		return err
	}
	if spec.GrantID != "" {
		return checkPromotionGrantReservableTx(ctx, tx, spec, now)
	}
	return nil
}

func preflightPromotionSpec(p model.Promotion) model.PromotionSpec {
	return model.PromotionSpec{RepoID: p.RepoID, SourceBranch: p.SourceBranch, SourceSHA: p.SourceSHA, TargetRef: p.TargetRef, PreviousSHA: p.PreviousSHA, ResultSHA: p.ResultSHA, Actor: p.Actor, GrantID: p.GrantID, RetryOf: p.ID, RetryMode: model.PromotionRetryAuto}
}

func (s *Store) enqueueKVMRetryTx(ctx context.Context, tx *sql.Tx, run model.Run, now int64) (model.Run, error) {
	spec, err := validateRunSpec(model.RunSpec{RepoID: run.RepoID, RefKind: run.RefKind, Ref: run.Ref, SHA: run.SHA, Actor: run.Actor, Release: run.Release, Credentialed: run.Release && run.Credentialed, Trigger: run.Trigger, ScheduleEntry: run.ScheduleEntry, TestedSHA: run.TestedSHA, BaseSHA: run.BaseSHA})
	if err != nil {
		return model.Run{}, err
	}
	id, err := randomID()
	if err != nil {
		return model.Run{}, err
	}
	enqueued, err := s.enqueueRunTx(ctx, tx, spec, id, now, run.AdmissionSequence)
	if err != nil {
		return model.Run{}, err
	}
	if run.Trigger == "promotion" {
		original, err := promotionByRunTx(ctx, tx, run.ID)
		if err != nil {
			return model.Run{}, err
		}
		spec := preflightPromotionSpec(original)
		retry, err := checkPromotionRetryTx(ctx, tx, spec)
		if err != nil {
			return model.Run{}, err
		}
		promotionID, err := randomID()
		if err != nil {
			return model.Run{}, err
		}
		promotion, err := insertPromotion(ctx, tx, spec, promotionID, id, spec.GrantID, now)
		if err != nil {
			return model.Run{}, err
		}
		if err = s.appendPromotionAdmissionAudit(ctx, tx, promotion, now); err != nil {
			return model.Run{}, err
		}
		if err = s.appendPromotionRetryAudit(ctx, tx, promotion, retry, now); err != nil {
			return model.Run{}, err
		}
		if spec.GrantID != "" {
			if err = s.appendPromotionGrantReserveAudit(ctx, tx, promotion, original.ID, now); err != nil {
				return model.Run{}, err
			}
		}
	}
	return enqueued.Run, nil
}

// A promotion retry retains the exact original plan and grant. It must still
// have a live, consistently scoped actor; direct authority cannot be recreated
// from an old admin flag. Delivery retains its existing target-head CAS check.
//
// This deliberately diverges from service.currentPromotionAuthority (the
// moved-target retry, #1097): there every uplink with the identity votes. Here
// only uplinks with an activated, unrevoked token credential vote, at least one
// must remain, and scope coverage is checked in this transaction. Recovery runs
// without a request or any later admission check, so a dormant or revoked
// credential must not keep an unattended trusted retry alive.
func requireKVMRetryPromotionAuthority(ctx context.Context, tx *sql.Tx, promotion model.Promotion) error {
	uplinks, err := matchUplinksByIdentity(ctx, tx, promotion.Actor, "")
	if err != nil {
		return err
	}
	var upstream string
	if err = tx.QueryRowContext(ctx, `SELECT u.name FROM repositories r JOIN upstreams u ON u.id=r.upstream_id WHERE r.id=?`, promotion.RepoID).Scan(&upstream); err != nil {
		return err
	}
	var scopes []string
	active := 0
	for _, uplink := range uplinks {
		var live bool
		if err = tx.QueryRowContext(ctx, `SELECT activated_at IS NOT NULL AND revoked_at IS NULL FROM token_credentials WHERE id=?`, uplink.TokenCredentialID).Scan(&live); err != nil {
			return err
		}
		if !live {
			continue
		}
		current, err := loadUplinkScopes(ctx, tx, uplink.ID)
		if err != nil {
			return err
		}
		if (promotion.GrantID == "" && !uplink.Admin) || !model.ScopeCovers(current, upstream) || (active != 0 && !slices.Equal(scopes, current)) {
			return ErrInvalidState
		}
		scopes = current
		active++
	}
	if active == 0 {
		return ErrInvalidState
	}
	return nil
}
