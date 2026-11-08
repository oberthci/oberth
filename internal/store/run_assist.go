package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/model"
)

// AppendRunAssist records one hold action on a run and, for a continue,
// marks the run assisted. The record and the audit row commit together so a
// run can never be continued without the evidence exclusion that follows.
func (s *Store) AppendRunAssist(ctx context.Context, request model.RunAssistRequest) (model.Run, error) {
	event := request.Event
	if strings.TrimSpace(request.RunID) == "" || strings.TrimSpace(event.Actor) == "" || strings.TrimSpace(event.Step) == "" {
		return model.Run{}, fmt.Errorf("%w: run ID, actor and step are required", ErrInvalid)
	}
	switch event.Kind {
	case model.RunAssistExec, model.RunAssistPatch, model.RunAssistContinue, model.RunAssistAbandon:
	default:
		return model.Run{}, fmt.Errorf("%w: unknown assist action %q", ErrInvalid, event.Kind)
	}
	now := s.now()
	if event.At.IsZero() {
		event.At = now.UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Run{}, fmt.Errorf("begin run assist: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, request.RunID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Run{}, fmt.Errorf("%w: run", ErrNotFound)
	}
	if err != nil {
		return model.Run{}, fmt.Errorf("load run for assist: %w", err)
	}
	if !current.AssistRequested {
		return model.Run{}, fmt.Errorf("%w: run %s did not ask for assisted continuation", ErrInvalidState, current.ID)
	}
	if request.Authorize != nil {
		if err := request.Authorize(current); err != nil {
			return model.Run{}, err
		}
	}
	record := model.RunAssistRecord{}
	if current.Assist != nil {
		record = *current.Assist
	}
	if event.Kind == model.RunAssistContinue {
		event.Assisted = record.Touched(event.Step)
	}
	record.Events = append(record.Events, event)
	encoded, err := json.Marshal(record)
	if err != nil {
		return model.Run{}, err
	}
	if len(encoded) > model.MaximumRunAssistRecordBytes {
		return model.Run{}, fmt.Errorf("%w: run assist record exceeds limit", ErrInvalid)
	}
	assisted := current.Assisted || event.Kind == model.RunAssistContinue
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET assist_record = ?, assisted = ?, updated_at = ? WHERE id = ?`, string(encoded), assisted, unixNano(now), current.ID); err != nil {
		return model.Run{}, fmt.Errorf("record run assist: %w", err)
	}
	details, err := json.Marshal(map[string]any{
		"run_id": current.ID, "repo_id": current.RepoID, "ref": current.Ref, "sha": current.SHA, "step": event.Step, "pod": event.Pod,
		"kind": event.Kind, "patch_sha256": event.PatchSHA256, "patch_bytes": event.PatchBytes, "files": event.Files,
		"argv": event.Argv, "exit_code": event.ExitCode, "assisted": assisted,
	})
	if err != nil {
		return model.Run{}, err
	}
	if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{Actor: event.Actor, Action: "run.assist." + event.Kind,
		ResourceType: "run", ResourceID: current.ID, Details: string(details)}, unixNano(now)); err != nil {
		return model.Run{}, fmt.Errorf("audit run assist: %w", err)
	}
	updated, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, current.ID))
	if err != nil {
		return model.Run{}, fmt.Errorf("reload assisted run: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return model.Run{}, fmt.Errorf("commit run assist: %w", err)
	}
	return updated, nil
}
