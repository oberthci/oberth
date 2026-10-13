package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
)

// WriteTrustedTestReceipt creates or updates a trusted test receipt.
// The receipt identity fields (run, profile, candidate, suite, guest, contract)
// are immutable once written; only the verdict fields can be updated.
func (s *Store) WriteTrustedTestReceipt(ctx context.Context, receipt vmrunner.TrustedTestReceipt) error {
	if err := vmrunner.ValidateReceipt(receipt); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// Verify the run exists and get its actor for audit.
	var actor string
	if err := tx.QueryRowContext(ctx, `SELECT actor FROM runs WHERE id=?`, receipt.RunID).Scan(&actor); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: run %s not found", ErrNotFound, receipt.RunID)
		}
		return err
	}

	now := unixNano(s.now())

	// Try insert first; on conflict update only verdict fields.
	result, err := tx.ExecContext(ctx, `
		INSERT INTO trusted_test_receipts(
			run_id, profile, candidate_sha, suite_identity, guest_digest,
			contract_digest, verdict, pod_uid, container_id, exit_code,
			restart_count, termination_hash, refusal_reason, created_at, updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(run_id) DO UPDATE SET
			verdict=excluded.verdict,
			pod_uid=excluded.pod_uid,
			container_id=excluded.container_id,
			exit_code=excluded.exit_code,
			restart_count=excluded.restart_count,
			termination_hash=excluded.termination_hash,
			refusal_reason=excluded.refusal_reason,
			updated_at=excluded.updated_at
		WHERE trusted_test_receipts.profile=excluded.profile
		  AND trusted_test_receipts.candidate_sha=excluded.candidate_sha
		  AND trusted_test_receipts.suite_identity=excluded.suite_identity
		  AND trusted_test_receipts.guest_digest=excluded.guest_digest
		  AND trusted_test_receipts.contract_digest=excluded.contract_digest`,
		receipt.RunID, receipt.Profile, receipt.CandidateSHA, receipt.SuiteIdentity,
		receipt.GuestDigest, receipt.ContractDigest, string(receipt.Verdict),
		receipt.PodUID, receipt.ContainerID, receipt.ExitCode,
		receipt.RestartCount, receipt.TerminationHash, receipt.RefusalReason,
		now, now,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("%w: trusted test receipt identity fields are immutable", ErrInvalidState)
	}

	// Audit the receipt — details must be JSON for the audit chain.
	auditDetails, err := json.Marshal(map[string]any{
		"profile":         receipt.Profile,
		"candidate_sha":   receipt.CandidateSHA,
		"suite_identity":  receipt.SuiteIdentity,
		"guest_digest":    receipt.GuestDigest,
		"contract_digest": receipt.ContractDigest,
		"verdict":         receipt.Verdict,
		"receipt_digest":  vmrunner.ReceiptDigest(receipt),
	})
	if err != nil {
		return fmt.Errorf("marshal trusted test receipt audit details: %w", err)
	}
	_, err = s.appendAuditAction(ctx, tx, model.AuditActionSpec{
		Actor:        actor,
		Action:       "trusted_test." + string(receipt.Verdict),
		ResourceType: "run",
		ResourceID:   receipt.RunID,
		Details:      string(auditDetails),
	}, now)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// TrustedTestReceipt returns the receipt for a run, or ErrNotFound.
func (s *Store) TrustedTestReceipt(ctx context.Context, runID string) (vmrunner.TrustedTestReceipt, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT run_id, profile, candidate_sha, suite_identity, guest_digest,
			contract_digest, verdict, pod_uid, container_id, exit_code,
			restart_count, termination_hash, refusal_reason, created_at, updated_at
		FROM trusted_test_receipts WHERE run_id=?`, runID)
	return scanTrustedTestReceipt(row)
}

func scanTrustedTestReceipt(row rowScanner) (vmrunner.TrustedTestReceipt, error) {
	var r vmrunner.TrustedTestReceipt
	var verdict string
	var created, updated int64
	if err := row.Scan(
		&r.RunID, &r.Profile, &r.CandidateSHA, &r.SuiteIdentity,
		&r.GuestDigest, &r.ContractDigest, &verdict,
		&r.PodUID, &r.ContainerID, &r.ExitCode, &r.RestartCount,
		&r.TerminationHash, &r.RefusalReason, &created, &updated,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return r, ErrNotFound
		}
		return r, err
	}
	r.Verdict = vmrunner.TrustedTestVerdict(verdict)
	r.CreatedAt = fromUnixNano(created)
	r.UpdatedAt = fromUnixNano(updated)
	return r, nil
}
