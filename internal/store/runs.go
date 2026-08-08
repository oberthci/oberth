package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

type rowScanner interface {
	Scan(dest ...any) error
}

const runColumns = `
queue_sequence, id, repo_id, ref_kind, ref, sha, actor, release, trigger,
phase, job_name, tested_sha, base_sha, failed_burn, failed_step, error,
status, reason, superseded_by, queued_at, started_at, finished_at, created_at, updated_at`

func (s *Store) EnqueueRun(ctx context.Context, spec model.RunSpec) (model.EnqueueRunResult, error) {
	spec, err := validateRunSpec(spec)
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	id, err := randomID()
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("generate run id: %w", err)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("begin enqueue run: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := s.enqueueRunTx(ctx, tx, spec, id, now)
	if err != nil {
		return model.EnqueueRunResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("commit enqueue run: %w", err)
	}
	return result, nil
}

func validateRunSpec(spec model.RunSpec) (model.RunSpec, error) {
	if spec.RepoID <= 0 || !spec.RefKind.Valid() || strings.TrimSpace(spec.Ref) == "" ||
		!validOID(spec.SHA) || strings.TrimSpace(spec.Actor) == "" || strings.TrimSpace(spec.Trigger) == "" {
		return model.RunSpec{}, fmt.Errorf("%w: run fields are invalid", ErrInvalid)
	}
	if spec.TestedSHA == "" {
		spec.TestedSHA = spec.SHA
	}
	if !validOID(spec.TestedSHA) || (spec.BaseSHA != "" && !validOID(spec.BaseSHA)) {
		return model.RunSpec{}, fmt.Errorf("%w: tested or base SHA is invalid", ErrInvalid)
	}
	return spec, nil
}

func (s *Store) enqueueRunTx(ctx context.Context, tx *sql.Tx, spec model.RunSpec, id string, now int64) (model.EnqueueRunResult, error) {
	var cancellations []model.RunCancellation
	// Only ordinary branch-push runs supersede one another. Promotion CI uses a
	// synthetic branch-shaped ref, but it is an independent immutable attempt
	// and must neither supersede nor be superseded by a client push. A pending
	// publication also owns its run until the external mutation terminalizes;
	// newer work stays queued behind that durable obligation.
	if spec.RefKind == model.RefBranch && spec.Trigger != "promotion" {
		rows, err := tx.QueryContext(ctx, `
SELECT id, job_name FROM runs
WHERE repo_id = ? AND ref_kind = 'branch' AND ref = ?
  AND trigger != 'promotion'
  AND NOT EXISTS (
      SELECT 1 FROM publications
      WHERE publications.run_id = runs.id AND publications.status = 'pending'
  )
  AND status IN ('queued', 'running') AND (status = 'running' OR job_name != '')
ORDER BY queue_sequence`, spec.RepoID, spec.Ref)
		if err != nil {
			return model.EnqueueRunResult{}, fmt.Errorf("find running superseded runs: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var cancellation model.RunCancellation
			if err := rows.Scan(&cancellation.RunID, &cancellation.JobName); err != nil {
				_ = rows.Close()
				return model.EnqueueRunResult{}, fmt.Errorf("scan running superseded run: %w", err)
			}
			cancellation.SupersededBy = id
			cancellation.Reason = "superseded"
			cancellation.CreatedAt = fromUnixNano(now)
			cancellations = append(cancellations, cancellation)
		}
		if err := rows.Close(); err != nil {
			return model.EnqueueRunResult{}, fmt.Errorf("close running superseded runs: %w", err)
		}
		if err := rows.Err(); err != nil {
			return model.EnqueueRunResult{}, fmt.Errorf("list running superseded runs: %w", err)
		}
	}

	row := tx.QueryRowContext(ctx, `
INSERT INTO runs(
    id, repo_id, ref_kind, ref, sha, actor, release, trigger, phase,
    tested_sha, base_sha, status, queued_at, created_at, updated_at
)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, 'queued', ?, ?, ?)
RETURNING `+runColumns,
		id, spec.RepoID, spec.RefKind, spec.Ref, spec.SHA, spec.Actor, spec.Release,
		spec.Trigger, spec.TestedSHA, spec.BaseSHA, now, now, now)
	value, err := scanRun(row)
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("insert run: %w", err)
	}
	if err := registerRunIssueWork(ctx, tx, value, now); err != nil {
		return model.EnqueueRunResult{}, err
	}
	if spec.RefKind == model.RefBranch && spec.Trigger != "promotion" {
		if _, err := tx.ExecContext(ctx, `
UPDATE runs
SET status = 'interrupted', phase = 'interrupted', reason = ?, superseded_by = ?,
    finished_at = ?, updated_at = ?
WHERE id != ? AND repo_id = ? AND ref_kind = 'branch' AND ref = ?
  AND trigger != 'promotion'
  AND NOT EXISTS (
      SELECT 1 FROM publications
      WHERE publications.run_id = runs.id AND publications.status = 'pending'
  )
  AND status IN ('queued', 'running')`,
			"superseded by newer run", id, now, now, id, spec.RepoID, spec.Ref); err != nil {
			return model.EnqueueRunResult{}, fmt.Errorf("supersede earlier runs: %w", err)
		}
		for _, cancellation := range cancellations {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO run_cancellations(run_id, job_name, superseded_by, reason, created_at)
VALUES(?, ?, ?, 'superseded', ?)`, cancellation.RunID, cancellation.JobName, cancellation.SupersededBy, now); err != nil {
				return model.EnqueueRunResult{}, fmt.Errorf("record run cancellation: %w", err)
			}
		}
	}
	return model.EnqueueRunResult{Run: value, Cancellations: cancellations}, nil
}

func (s *Store) ClaimNextRun(ctx context.Context) (model.Run, error) {
	now := unixNano(s.now())
	value, err := scanRun(s.db.QueryRowContext(ctx, `
UPDATE runs
SET status = 'running', phase = 'running', started_at = ?, updated_at = ?
WHERE queue_sequence = (
    SELECT candidate.queue_sequence
    FROM runs AS candidate
    WHERE candidate.status = 'queued'
      AND NOT EXISTS (
          SELECT 1
          FROM publications AS publication
          WHERE publication.status = 'pending'
            AND publication.repo_id = candidate.repo_id
            AND publication.ref_kind = candidate.ref_kind
            AND publication.ref = candidate.ref
      )
    ORDER BY candidate.queue_sequence
    LIMIT 1
)
RETURNING `+runColumns, now, now))
	if err != nil {
		return model.Run{}, err
	}
	return value, nil
}

func (s *Store) Run(ctx context.Context, id string) (model.Run, error) {
	value, err := scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id))
	if err != nil {
		return model.Run{}, translateNotFound("run", err)
	}
	return value, nil
}

func (s *Store) LatestRunForRef(ctx context.Context, repoID int64, ref string) (model.Run, error) {
	value, err := scanRun(s.db.QueryRowContext(ctx, `
SELECT `+runColumns+` FROM runs
WHERE repo_id = ? AND ref = ? ORDER BY queue_sequence DESC LIMIT 1`, repoID, ref))
	if err != nil {
		return model.Run{}, translateNotFound("run ref", err)
	}
	return value, nil
}

func (s *Store) LatestRunForSHA(ctx context.Context, repoID int64, sha string) (model.Run, error) {
	value, err := scanRun(s.db.QueryRowContext(ctx, `
SELECT `+runColumns+` FROM runs
WHERE repo_id = ? AND sha = ? ORDER BY queue_sequence DESC LIMIT 1`, repoID, sha))
	if err != nil {
		return model.Run{}, translateNotFound("run SHA", err)
	}
	return value, nil
}

// ResolveRun follows Git-like precedence: an exact full object ID, a unique
// abbreviated object ID, then a branch or tag name.
func (s *Store) ResolveRun(ctx context.Context, repoID int64, selector string) (model.Run, error) {
	selector = strings.TrimSpace(selector)
	if repoID <= 0 || selector == "" {
		return model.Run{}, fmt.Errorf("%w: repository and selector are required", ErrInvalid)
	}
	lower := strings.ToLower(selector)
	if isHexPrefix(lower) && len(lower) >= 7 && len(lower) <= 64 {
		if validOID(lower) {
			value, err := s.LatestRunForSHA(ctx, repoID, lower)
			if err == nil {
				return value, nil
			}
			if !errors.Is(err, ErrNotFound) {
				return model.Run{}, err
			}
		}
		var matches int
		if err := s.db.QueryRowContext(ctx, `
SELECT count(DISTINCT sha) FROM runs WHERE repo_id = ? AND sha LIKE ?`, repoID, lower+"%").Scan(&matches); err != nil {
			return model.Run{}, fmt.Errorf("resolve abbreviated SHA: %w", err)
		}
		switch {
		case matches > 1:
			return model.Run{}, fmt.Errorf("%w: %q", ErrAmbiguous, selector)
		case matches == 1:
			value, err := scanRun(s.db.QueryRowContext(ctx, `
SELECT `+runColumns+` FROM runs
WHERE repo_id = ? AND sha LIKE ? ORDER BY queue_sequence DESC LIMIT 1`, repoID, lower+"%"))
			if err != nil {
				return model.Run{}, fmt.Errorf("resolve abbreviated SHA: %w", err)
			}
			return value, nil
		}
	}
	return s.LatestRunForRef(ctx, repoID, selector)
}

func (s *Store) SetRunJobName(ctx context.Context, id, jobName string) (model.Run, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(jobName) == "" {
		return model.Run{}, fmt.Errorf("%w: run ID and job name are required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Run{}, fmt.Errorf("begin set run job name: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	value, err := scanRun(tx.QueryRowContext(ctx, `
UPDATE runs SET job_name = ?, updated_at = ?
WHERE id = ? AND (
    status IN ('queued', 'running') OR
    (status = 'interrupted' AND superseded_by != '')
)
RETURNING `+runColumns, jobName, unixNano(s.now()), id))
	if errors.Is(err, sql.ErrNoRows) {
		var exists int
		if lookupErr := tx.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE id = ?`, id).Scan(&exists); lookupErr != nil {
			return model.Run{}, fmt.Errorf("look up run after rejected job name: %w", lookupErr)
		}
		if exists == 0 {
			return model.Run{}, fmt.Errorf("%w: run", ErrNotFound)
		}
		return model.Run{}, fmt.Errorf("%w: terminal run", ErrInvalidState)
	}
	if err != nil {
		return model.Run{}, fmt.Errorf("set run job name: %w", err)
	}
	if value.Status == model.RunInterrupted && value.SupersededBy != "" {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO run_cancellations(run_id, job_name, superseded_by, reason, created_at)
VALUES(?, ?, ?, 'superseded', ?)
ON CONFLICT(run_id) DO UPDATE SET job_name = excluded.job_name
WHERE run_cancellations.completed_at IS NULL`, value.ID, jobName, value.SupersededBy, unixNano(s.now())); err != nil {
			return model.Run{}, fmt.Errorf("retain superseded run cancellation: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return model.Run{}, fmt.Errorf("commit set run job name: %w", err)
	}
	return value, nil
}

func (s *Store) PendingRunCancellations(ctx context.Context) ([]model.RunCancellation, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT run_id, job_name, superseded_by, reason, created_at, completed_at
FROM run_cancellations WHERE completed_at IS NULL ORDER BY created_at, run_id`)
	if err != nil {
		return nil, fmt.Errorf("list pending run cancellations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var cancellations []model.RunCancellation
	for rows.Next() {
		value, err := scanRunCancellation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan pending run cancellation: %w", err)
		}
		cancellations = append(cancellations, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending run cancellations: %w", err)
	}
	return cancellations, nil
}

func (s *Store) CompleteRunCancellation(ctx context.Context, runID, jobName string) error {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(jobName) == "" {
		return fmt.Errorf("%w: run ID and job name are required", ErrInvalid)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE run_cancellations SET completed_at = ?
WHERE run_id = ? AND job_name = ? AND completed_at IS NULL`, unixNano(s.now()), runID, jobName)
	if err != nil {
		return fmt.Errorf("complete run cancellation: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read completed run cancellation result: %w", err)
	}
	if changed == 1 {
		return nil
	}
	var completed sql.NullInt64
	err = s.db.QueryRowContext(ctx, `
SELECT completed_at FROM run_cancellations WHERE run_id = ? AND job_name = ?`, runID, jobName).Scan(&completed)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: pending run cancellation", ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("look up completed run cancellation: %w", err)
	}
	if completed.Valid {
		return nil
	}
	return fmt.Errorf("%w: pending run cancellation", ErrInvalidState)
}

// CompleteSupersededRunCancellationWithoutJob closes the narrow race in which
// a worker is superseded after claim but exits before publishing its
// deterministic Job name. Callers invoke this only after that exact worker has
// returned; the durable predicates make every earlier or unrelated call a
// no-op.
func (s *Store) CompleteSupersededRunCancellationWithoutJob(ctx context.Context, runID string) error {
	if strings.TrimSpace(runID) == "" {
		return fmt.Errorf("%w: run ID is required", ErrInvalid)
	}
	if _, err := s.db.ExecContext(ctx, `
UPDATE run_cancellations
SET completed_at = ?
WHERE run_id = ? AND job_name = '' AND completed_at IS NULL AND reason = 'superseded'
  AND EXISTS (
      SELECT 1 FROM runs
      WHERE runs.id = run_cancellations.run_id
        AND runs.status = 'interrupted' AND runs.superseded_by != ''
  )`, unixNano(s.now()), runID); err != nil {
		return fmt.Errorf("complete superseded no-Job cancellation for run %s: %w", runID, err)
	}
	return nil
}

func (s *Store) FinishRun(ctx context.Context, id string, result model.RunResult) (model.Run, error) {
	if strings.TrimSpace(id) == "" || !result.Status.Terminal() {
		return model.Run{}, fmt.Errorf("%w: terminal run result is required", ErrInvalid)
	}
	if (result.TestedSHA != "" && !validOID(result.TestedSHA)) ||
		(result.BaseSHA != "" && !validOID(result.BaseSHA)) {
		return model.Run{}, fmt.Errorf("%w: tested or base SHA is invalid", ErrInvalid)
	}
	phase := strings.TrimSpace(result.Phase)
	if phase == "" {
		phase = string(result.Status)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Run{}, fmt.Errorf("begin finish run: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Run{}, fmt.Errorf("%w: run", ErrNotFound)
	}
	if err != nil {
		return model.Run{}, fmt.Errorf("load run for finish: %w", err)
	}
	if current.Trigger == "promotion" && result.Status == model.RunPassed {
		return model.Run{}, fmt.Errorf("%w: passing promotion runs finalize through publication", ErrInvalidState)
	}
	value, err := scanRun(tx.QueryRowContext(ctx, `
UPDATE runs
SET status = ?, phase = ?,
    tested_sha = CASE WHEN ? = '' THEN tested_sha ELSE ? END,
    base_sha = CASE WHEN ? = '' THEN base_sha ELSE ? END,
    failed_burn = ?, failed_step = ?, error = ?, finished_at = ?, updated_at = ?
WHERE id = ? AND status IN ('queued', 'running')
RETURNING `+runColumns,
		result.Status, phase,
		result.TestedSHA, result.TestedSHA,
		result.BaseSHA, result.BaseSHA,
		result.FailedBurn, result.FailedStep, result.Error, now, now, id))
	if errors.Is(err, sql.ErrNoRows) {
		var exists int
		if lookupErr := tx.QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE id = ?`, id).Scan(&exists); lookupErr != nil {
			return model.Run{}, fmt.Errorf("look up run after rejected finish: %w", lookupErr)
		}
		if exists == 0 {
			return model.Run{}, fmt.Errorf("%w: run", ErrNotFound)
		}
		return model.Run{}, fmt.Errorf("%w: run is already terminal", ErrInvalidState)
	}
	if err != nil {
		return model.Run{}, fmt.Errorf("finish run: %w", err)
	}
	if value.Trigger == "promotion" {
		promotion, promotionErr := promotionByRunTx(ctx, tx, value.ID)
		if promotionErr != nil {
			return model.Run{}, fmt.Errorf("load promotion for terminal run: %w", promotionErr)
		}
		promotionStatus := model.PromotionFailed
		switch value.Status {
		case model.RunPassed:
			// Defensive: FinishRun rejects RunPassed for promotion triggers
			// above, so this case is unreachable from current callers. It
			// exists as a safety net for any future caller path that may reach
			// this switch with a passing status.
			promotionStatus = model.PromotionPassed
		case model.RunInterrupted:
			promotionStatus = model.PromotionInterrupted
		}
		promotion, promotionErr = finishPromotionTx(ctx, tx, promotion, promotionStatus, value.ID, value.Error, now)
		if promotionErr != nil {
			return model.Run{}, promotionErr
		}
		if err := projectPromotionIssue(ctx, tx, promotion, now); err != nil {
			return model.Run{}, err
		}
	} else if err := projectRunIssue(ctx, tx, value, result.FailureTail, now); err != nil {
		return model.Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Run{}, fmt.Errorf("commit finish run: %w", err)
	}
	return value, nil
}

func (s *Store) ListRecentRuns(ctx context.Context, filter model.RunListFilter) ([]model.Run, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT `+runColumns+` FROM runs
WHERE (? = 0 OR repo_id = ?) ORDER BY queue_sequence DESC LIMIT ?`,
		filter.RepoID, filter.RepoID, pageLimit(filter.Limit))
	if err != nil {
		return nil, fmt.Errorf("list recent runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var runs []model.Run
	for rows.Next() {
		value, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list recent runs: %w", err)
	}
	return runs, nil
}

func (s *Store) PutStepResult(ctx context.Context, result model.StepResult) (model.StepResult, error) {
	if strings.TrimSpace(result.RunID) == "" || strings.TrimSpace(result.Burn) == "" ||
		strings.TrimSpace(result.Step) == "" || result.Ordinal < 0 || !result.Status.Terminal() ||
		result.LogStart < 0 || result.LogEnd < result.LogStart {
		return model.StepResult{}, fmt.Errorf("%w: step result fields are invalid", ErrInvalid)
	}
	recorded := unixNano(s.now())
	row := s.db.QueryRowContext(ctx, `
INSERT INTO step_results(
    run_id, burn, step, ordinal, status, exit_code, log_start, log_end,
    started_at, finished_at, recorded_at
)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(run_id, burn, step) DO UPDATE SET
    ordinal = excluded.ordinal,
    status = excluded.status,
    exit_code = excluded.exit_code,
    log_start = excluded.log_start,
    log_end = excluded.log_end,
    started_at = excluded.started_at,
    finished_at = excluded.finished_at,
    recorded_at = excluded.recorded_at
RETURNING run_id, burn, step, ordinal, status, exit_code, log_start, log_end,
          started_at, finished_at, recorded_at`,
		result.RunID, result.Burn, result.Step, result.Ordinal, result.Status,
		result.ExitCode, result.LogStart, result.LogEnd, nullableUnixNano(result.StartedAt),
		nullableUnixNano(result.FinishedAt), recorded)
	value, err := scanStepResult(row)
	if err != nil {
		return model.StepResult{}, fmt.Errorf("put step result: %w", err)
	}
	return value, nil
}

func (s *Store) StepResults(ctx context.Context, runID string) ([]model.StepResult, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT run_id, burn, step, ordinal, status, exit_code, log_start, log_end,
       started_at, finished_at, recorded_at
FROM step_results WHERE run_id = ? ORDER BY ordinal, burn, step`, runID)
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
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list step results: %w", err)
	}
	return results, nil
}

func scanRun(row rowScanner) (model.Run, error) {
	var value model.Run
	var release int64
	var queued, created, updated int64
	var started, finished sql.NullInt64
	if err := row.Scan(
		&value.QueueSequence, &value.ID, &value.RepoID, &value.RefKind, &value.Ref,
		&value.SHA, &value.Actor, &release, &value.Trigger, &value.Phase, &value.JobName,
		&value.TestedSHA, &value.BaseSHA, &value.FailedBurn, &value.FailedStep, &value.Error,
		&value.Status, &value.Reason, &value.SupersededBy, &queued, &started, &finished,
		&created, &updated,
	); err != nil {
		return model.Run{}, err
	}
	value.Release = release != 0
	value.QueuedAt = fromUnixNano(queued)
	value.StartedAt, value.FinishedAt = nullableTime(started), nullableTime(finished)
	value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	return value, nil
}

func scanStepResult(row rowScanner) (model.StepResult, error) {
	var value model.StepResult
	var started, finished sql.NullInt64
	var recorded int64
	if err := row.Scan(&value.RunID, &value.Burn, &value.Step, &value.Ordinal,
		&value.Status, &value.ExitCode, &value.LogStart, &value.LogEnd,
		&started, &finished, &recorded); err != nil {
		return model.StepResult{}, err
	}
	value.StartedAt, value.FinishedAt = nullableTime(started), nullableTime(finished)
	value.RecordedAt = fromUnixNano(recorded)
	return value, nil
}

func scanRunCancellation(row rowScanner) (model.RunCancellation, error) {
	var value model.RunCancellation
	var created int64
	var completed sql.NullInt64
	if err := row.Scan(&value.RunID, &value.JobName, &value.SupersededBy, &value.Reason, &created, &completed); err != nil {
		return model.RunCancellation{}, err
	}
	value.CreatedAt = fromUnixNano(created)
	value.CompletedAt = nullableTime(completed)
	return value, nil
}

func nullableUnixNano(value *time.Time) any {
	if value == nil {
		return nil
	}
	return unixNano(*value)
}

func isHexPrefix(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
