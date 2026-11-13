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

const stepResultColumns = `run_id,node_id,display_name,template,node_type,boundary_id,phase,message,pod_name,
 burn,step,ordinal,status,exit_code,started_at,finished_at,recorded_at,termination_json,credentialed,grant_evidence_json,replay_evidence_complete`

// Admission receipts are append-only and emitted before Workflow create.
// Prefer any credentialed receipt: a later admission cannot declassify work.
const stepAdmissionSourceSQL = ` FROM audit_actions a JOIN runs r ON r.id=a.resource_id,
 json_each(CASE WHEN json_valid(a.details) THEN a.details ELSE '{}' END,'$.step_credentials') e
 WHERE a.resource_type='run' AND a.resource_id=?
 AND a.action IN ('ci.argo.submit.binding','release.argo.submit.binding')
 AND json_extract(a.details,'$.engine')='argo'
 AND json_extract(a.details,'$.workflow')=r.job_name`

const stepAdmissionSQL = stepAdmissionSourceSQL + `
 AND json_extract(e.value,'$.template')=?
 AND json_type(e.value,'$.credentialed') IN ('true','false')
 AND json_type(e.value,'$.grants')='array'
 ORDER BY CASE WHEN json_type(e.value,'$.known')='true' THEN 1 ELSE 0 END, json_extract(e.value,'$.credentialed') DESC,a.id ASC LIMIT 1`

func validateStepResult(result model.StepResult) error {
	field := ""
	switch {
	case strings.TrimSpace(result.RunID) == "":
		field = "run_id"
	case strings.TrimSpace(result.NodeID) == "":
		field = "node_id"
	case result.Ordinal < 0:
		field = "ordinal"
	case !result.Status.Valid() || result.Status == model.StepPending:
		field = "status"
	}
	if field != "" {
		return fmt.Errorf("%w: step result invalid field %s", ErrInvalid, field)
	}
	return nil
}

func (s *Store) PutStepResult(ctx context.Context, result model.StepResult) (model.StepResult, error) {
	if err := validateStepResult(result); err != nil {
		return model.StepResult{}, err
	}
	return s.putStepResult(ctx, s.db, result)
}

// PutStepResults has no node-count ceiling. Each transaction writes at most
// 128 rows, so a large native fan-out never holds the writer for the whole run.
func (s *Store) PutStepResults(ctx context.Context, runID string, results []model.StepResult) error {
	for _, result := range results {
		if result.RunID != "" && result.RunID != runID {
			return fmt.Errorf("%w: step result run does not match snapshot", ErrInvalid)
		}
		result.RunID = runID
		if err := validateStepResult(result); err != nil {
			return err
		}
	}
	if len(results) == 1 {
		result := results[0]
		result.RunID = runID
		_, err := s.PutStepResult(ctx, result)
		return err
	}
	const batchSize = 128
	for start := 0; start < len(results); start += batchSize {
		if err := s.putStepBatch(ctx, runID, results[start:min(start+batchSize, len(results))]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) putStepBatch(ctx context.Context, runID string, results []model.StepResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin step snapshot batch: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, result := range results {
		result.RunID = runID
		if _, err := s.putStepResult(ctx, tx, result); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit step snapshot batch: %w", err)
	}
	return nil
}

func (s *Store) putStepResult(ctx context.Context, database interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, result model.StepResult) (model.StepResult, error) {
	credentialTemplate := ""
	if result.LocalTemplate {
		credentialTemplate = result.Template
	}
	row := database.QueryRowContext(ctx, `INSERT INTO step_results (`+stepResultColumns+`)
SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,
 (SELECT CASE WHEN json_type(e.value,'$.known')='true' THEN json_extract(e.value,'$.credentialed') ELSE NULL END`+stepAdmissionSQL+`),
 (SELECT json_extract(e.value,'$.grants')`+stepAdmissionSQL+`),
 (SELECT COALESCE(MIN(CASE WHEN json_type(e.value,'$.complete')='true' THEN 1 ELSE 0 END),0)`+stepAdmissionSourceSQL+`)
WHERE EXISTS (SELECT 1 FROM runs WHERE id=? AND status IN ('queued','running'))
ON CONFLICT(run_id,node_id) DO UPDATE SET
 display_name=excluded.display_name,template=excluded.template,node_type=excluded.node_type,
 boundary_id=excluded.boundary_id,phase=excluded.phase,message=excluded.message,pod_name=COALESCE(NULLIF(excluded.pod_name,''),step_results.pod_name),
 burn=excluded.burn,step=excluded.step,ordinal=excluded.ordinal,status=excluded.status,exit_code=excluded.exit_code,
 started_at=excluded.started_at,finished_at=excluded.finished_at,recorded_at=excluded.recorded_at,
 termination_json=COALESCE(excluded.termination_json,step_results.termination_json),
 replay_evidence_complete=excluded.replay_evidence_complete,
 credentialed=CASE WHEN step_results.credentialed=1 OR excluded.credentialed=1 THEN 1 ELSE excluded.credentialed END,
 grant_evidence_json=CASE WHEN step_results.credentialed=1 THEN step_results.grant_evidence_json ELSE excluded.grant_evidence_json END
RETURNING `+stepResultColumns,
		result.RunID, result.NodeID, result.DisplayName, result.Template, result.NodeType, result.BoundaryID, result.Phase, result.Message, result.PodName,
		result.Stage, result.Step, result.Ordinal, result.Status, result.ExitCode,
		nullableUnixNano(result.StartedAt), nullableUnixNano(result.FinishedAt), unixNano(s.now()), stepDiagnosticJSON(result.Termination, result.RunID), result.RunID, credentialTemplate, result.RunID, credentialTemplate, result.RunID, result.RunID)
	value, err := scanStepResult(row)
	if errors.Is(err, sql.ErrNoRows) {
		return model.StepResult{}, fmt.Errorf("%w: terminal or missing run cannot accept step evidence", ErrInvalidState)
	}
	if err != nil {
		return model.StepResult{}, fmt.Errorf("put step result: %w", err)
	}
	return value, nil
}

func (s *Store) StepResults(ctx context.Context, runID string) ([]model.StepResult, error) {
	return stepResultsFrom(ctx, s.reader(), runID)
}

func stepResultsFrom(ctx context.Context, database interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, runID string) ([]model.StepResult, error) {
	rows, err := database.QueryContext(ctx, `SELECT `+stepResultColumns+` FROM step_results WHERE run_id=? ORDER BY ordinal,node_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("list step results: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var results []model.StepResult
	for rows.Next() {
		value, err := scanStepResult(rows)
		if err != nil {
			return nil, fmt.Errorf("scan step result: %w", err)
		}
		results = append(results, value)
	}
	return results, rows.Err()
}

func scanStepResult(row rowScanner) (model.StepResult, error) {
	var value model.StepResult
	var started, finished sql.NullInt64
	var recorded int64
	var diagnostic, grants sql.NullString
	var credentialed sql.NullBool
	if err := scanRowNotFound(row, "StepResult", &value.RunID, &value.NodeID, &value.DisplayName, &value.Template, &value.NodeType,
		&value.BoundaryID, &value.Phase, &value.Message, &value.PodName, &value.Stage, &value.Step, &value.Ordinal,
		&value.Status, &value.ExitCode, &started, &finished, &recorded, &diagnostic, &credentialed, &grants, &value.ReplayEvidenceComplete); err != nil {
		return model.StepResult{}, err
	}
	value.Termination = readStepDiagnostic(diagnostic, value.RunID)
	value.Credentialed = credentialed.Valid && credentialed.Bool
	value.CredentialEvidenceKnown = credentialed.Valid && grants.Valid && json.Unmarshal([]byte(grants.String), &value.GrantEvidence) == nil
	for _, grant := range value.GrantEvidence {
		value.Secrets = value.Secrets || strings.HasPrefix(grant, "secret:")
	}
	value.StartedAt, value.FinishedAt = nullableTime(started), nullableTime(finished)
	value.RecordedAt = fromUnixNano(recorded)
	return value, nil
}
