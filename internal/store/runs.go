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
queue_sequence, id, repo_id, ref_kind, ref, sha, actor, release, credentialed, trigger,
phase, job_name, tested_sha, base_sha, failed_burn, failed_step, error,
status, reason, superseded_by, queued_at, started_at, finished_at, created_at, updated_at`

// EnqueueRun creates a standalone run without a durable receive event
// association. Production callers go through EnqueueReceiveEvent which binds
// the receive idempotency key atomically; this wrapper is retained for test
// setup where fabricating a receive event is unnecessary overhead.
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

// ErrRequeueIneligible marks a stranded run that restart recovery must
// terminalize instead of re-firing: promotion CI, tag or release or
// credentialed work, a run owned by a pending publication, or a branch that
// already has newer active work (issue #270).
var ErrRequeueIneligible = errors.New("store: stranded run is not eligible for restart requeue")

// RequeueStrandedRun supersedes one still-running stranded run with a fresh
// queued copy of the same spec (issue #270). The supersede inside the enqueue
// marks the old run interrupted with a link to its replacement and records
// the durable Job cancellation obligation; the scheduler's cancellation pass
// executes that deletion before any new claim can start the replacement.
func (s *Store) RequeueStrandedRun(ctx context.Context, runID string) (model.Run, error) {
	if strings.TrimSpace(runID) == "" {
		return model.Run{}, fmt.Errorf("%w: run ID is required", ErrInvalid)
	}
	now := unixNano(s.now())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Run{}, fmt.Errorf("begin stranded run requeue: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	run, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, runID))
	if errors.Is(err, sql.ErrNoRows) {
		return model.Run{}, fmt.Errorf("%w: run", ErrNotFound)
	}
	if err != nil {
		return model.Run{}, fmt.Errorf("load stranded run: %w", err)
	}
	if run.Status != model.RunRunning {
		return model.Run{}, ErrRequeueIneligible
	}
	var pendingPublications int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM publications WHERE run_id = ? AND status = 'pending'`, run.ID).Scan(&pendingPublications); err != nil {
		return model.Run{}, fmt.Errorf("check stranded run publications: %w", err)
	}
	if pendingPublications > 0 {
		return model.Run{}, ErrRequeueIneligible
	}
	requeued, err := s.requeueStrandedRunTx(ctx, tx, run, now)
	if err != nil {
		return model.Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Run{}, fmt.Errorf("commit stranded run requeue: %w", err)
	}
	return requeued, nil
}

// requeueStrandedRunTx enqueues a fresh copy of a stranded run inside the
// caller's transaction. The caller has already excluded pending-publication
// owners; this helper owns the trigger-class and newer-work guards.
func (s *Store) requeueStrandedRunTx(ctx context.Context, tx *sql.Tx, run model.Run, now int64) (model.Run, error) {
	if run.RefKind != model.RefBranch || run.Trigger == "promotion" || run.Release || run.Credentialed {
		return model.Run{}, ErrRequeueIneligible
	}
	// A newer push may already have replayed into the queue while this
	// process was starting; requeueing the older SHA behind it would
	// supersede the newer work backwards. The branch keeps exactly its
	// newest active run.
	var activeSiblings int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM runs
WHERE repo_id = ? AND ref_kind = 'branch' AND ref = ? AND id != ?
  AND status IN ('queued', 'running')`, run.RepoID, run.Ref, run.ID).Scan(&activeSiblings); err != nil {
		return model.Run{}, fmt.Errorf("check active branch siblings: %w", err)
	}
	if activeSiblings > 0 {
		return model.Run{}, ErrRequeueIneligible
	}
	spec, err := validateRunSpec(model.RunSpec{
		RepoID: run.RepoID, RefKind: run.RefKind, Ref: run.Ref, SHA: run.SHA,
		Actor: run.Actor, Release: run.Release, Credentialed: run.Credentialed,
		Trigger: run.Trigger, TestedSHA: run.TestedSHA, BaseSHA: run.BaseSHA,
	})
	if err != nil {
		return model.Run{}, err
	}
	id, err := randomID()
	if err != nil {
		return model.Run{}, fmt.Errorf("generate requeued run id: %w", err)
	}
	result, err := s.enqueueRunTx(ctx, tx, spec, id, now)
	if err != nil {
		return model.Run{}, fmt.Errorf("requeue stranded run %s: %w", run.ID, err)
	}
	return result.Run, nil
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
    id, repo_id, ref_kind, ref, sha, actor, release, credentialed, trigger, phase,
    tested_sha, base_sha, status, queued_at, created_at, updated_at
)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, 'queued', ?, ?, ?)
RETURNING `+runColumns,
		id, spec.RepoID, spec.RefKind, spec.Ref, spec.SHA, spec.Actor, spec.Release,
		spec.Credentialed, spec.Trigger, spec.TestedSHA, spec.BaseSHA, now, now, now)
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
			// Upsert: a run superseded across a restart may already carry an
			// owner_restart obligation (run_id is the primary key). The fresh
			// supersede obligation replaces it and re-arms completion so the
			// stranded Job is still deleted exactly once (issue #270).
			if _, err := tx.ExecContext(ctx, `
INSERT INTO run_cancellations(run_id, job_name, superseded_by, reason, created_at)
VALUES(?, ?, ?, 'superseded', ?)
ON CONFLICT(run_id) DO UPDATE SET
    job_name = excluded.job_name,
    superseded_by = excluded.superseded_by,
    reason = excluded.reason,
    completed_at = NULL`, cancellation.RunID, cancellation.JobName, cancellation.SupersededBy, now); err != nil {
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

// RunningRunsWithJobs returns runs in status "running" that have a known
// deterministic Job name, excluding runs owned by a pending publication.
// The scheduler uses this at startup to reconcile runs stranded by a crash.
func (s *Store) RunningRunsWithJobs(ctx context.Context) ([]model.Run, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT `+runColumns+` FROM runs
WHERE status = 'running' AND job_name != ''
  AND NOT EXISTS (
      SELECT 1 FROM publications
      WHERE publications.run_id = runs.id AND publications.status = 'pending'
  )
ORDER BY queue_sequence`)
	if err != nil {
		return nil, fmt.Errorf("list running runs with jobs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var runs []model.Run
	for rows.Next() {
		value, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan running run with job: %w", err)
		}
		runs = append(runs, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list running runs with jobs: %w", err)
	}
	return runs, nil
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
// abbreviated object ID, then a branch or tag name. When a bare name matches
// runs on both branch and tag ref kinds, ErrAmbiguous is returned so the
// caller can request a specific kind.
func (s *Store) ResolveRun(ctx context.Context, repoID int64, selector string) (model.Run, error) {
	selector = strings.TrimSpace(selector)
	if repoID <= 0 || selector == "" {
		return model.Run{}, fmt.Errorf("%w: repository and selector are required", ErrInvalid)
	}
	// Recognize refs/heads/<name> and refs/tags/<name> disambiguators
	// before hex/SHA logic — a refs/ prefix can never be a valid SHA.
	if after, ok := strings.CutPrefix(selector, "refs/heads/"); ok {
		if after == "" {
			return model.Run{}, fmt.Errorf("%w: empty ref name after refs/heads/ prefix", ErrInvalid)
		}
		return s.ResolveRunByRefKind(ctx, repoID, after, model.RefBranch)
	}
	if after, ok := strings.CutPrefix(selector, "refs/tags/"); ok {
		if after == "" {
			return model.Run{}, fmt.Errorf("%w: empty ref name after refs/tags/ prefix", ErrInvalid)
		}
		return s.ResolveRunByRefKind(ctx, repoID, after, model.RefTag)
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
	return s.latestRunForRefWithAmbiguityCheck(ctx, repoID, selector)
}

// latestRunForRefWithAmbiguityCheck resolves a bare ref name and returns
// ErrAmbiguous when the same name appears as both a branch and a tag ref.
func (s *Store) latestRunForRefWithAmbiguityCheck(ctx context.Context, repoID int64, ref string) (model.Run, error) {
	var kinds int
	if err := s.db.QueryRowContext(ctx, `
SELECT count(DISTINCT ref_kind) FROM runs WHERE repo_id = ? AND ref = ?`, repoID, ref).Scan(&kinds); err != nil {
		return model.Run{}, fmt.Errorf("check ref kind ambiguity: %w", err)
	}
	if kinds > 1 {
		return model.Run{}, fmt.Errorf("%w: ref %q exists as both branch and tag; use the full refs/heads/ or refs/tags/ prefix to disambiguate", ErrAmbiguous, ref)
	}
	return s.LatestRunForRef(ctx, repoID, ref)
}

// ResolveRunByRefKind resolves a ref name constrained to a specific ref kind,
// avoiding cross-kind ambiguity.
func (s *Store) ResolveRunByRefKind(ctx context.Context, repoID int64, ref string, kind model.RefKind) (model.Run, error) {
	ref = strings.TrimSpace(ref)
	if repoID <= 0 || ref == "" || !kind.Valid() {
		return model.Run{}, fmt.Errorf("%w: repository, ref, and kind are required", ErrInvalid)
	}
	value, err := scanRun(s.db.QueryRowContext(ctx, `
SELECT `+runColumns+` FROM runs
WHERE repo_id = ? AND ref = ? AND ref_kind = ? ORDER BY queue_sequence DESC LIMIT 1`, repoID, ref, kind))
	if err != nil {
		return model.Run{}, translateNotFound("run ref kind", err)
	}
	return value, nil
}

// DistinctBranchRefsForSHA returns the set of distinct Ref values across
// ordinary branch-trigger runs for one (repository, SHA) pair. Promotion,
// release, plan, and apply triggers are excluded — these carry synthetic refs
// and are not candidates for sync's branch resolution.
func (s *Store) DistinctBranchRefsForSHA(ctx context.Context, repoID int64, sha string) ([]string, error) {
	if repoID <= 0 || !validOID(sha) {
		return nil, fmt.Errorf("%w: repository and full SHA are required", ErrInvalid)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT ref FROM runs
WHERE repo_id = ? AND sha = ? AND ref_kind = 'branch'
  AND trigger NOT IN ('promotion', 'release', 'plan', 'apply')
ORDER BY ref`, repoID, sha)
	if err != nil {
		return nil, fmt.Errorf("distinct branch refs for SHA: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var refs []string
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return nil, fmt.Errorf("scan distinct branch ref: %w", err)
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
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
WHERE (? = 0 OR repo_id = ?) AND (? = '' OR ref = ?)
ORDER BY queue_sequence DESC LIMIT ?`,
		filter.RepoID, filter.RepoID, filter.Ref, filter.Ref, pageLimit(filter.Limit))
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

// ListLatestRunsPerRepo returns the most recent N runs for every repository
// using a SQL window function. The result is bounded by repos x perRepo rows
// (e.g. 20 repos x 12 = 240) and is always correct regardless of total run
// count, unlike filtering a global top-N window which silently drops repos
// whose latest run fell outside that window.
func (s *Store) ListLatestRunsPerRepo(ctx context.Context, perRepo int) ([]model.Run, error) {
	if perRepo <= 0 {
		perRepo = 12
	}
	if perRepo > 50 {
		perRepo = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT `+runColumns+` FROM (
    SELECT *, ROW_NUMBER() OVER (PARTITION BY repo_id ORDER BY queue_sequence DESC) AS rn
    FROM runs
) WHERE rn <= ?
ORDER BY queue_sequence DESC`, perRepo)
	if err != nil {
		return nil, fmt.Errorf("list latest runs per repo: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var runs []model.Run
	for rows.Next() {
		value, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan latest run per repo: %w", err)
		}
		runs = append(runs, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list latest runs per repo: %w", err)
	}
	return runs, nil
}

func (s *Store) PutStepResult(ctx context.Context, result model.StepResult) (model.StepResult, error) {
	if strings.TrimSpace(result.RunID) == "" || strings.TrimSpace(result.Burn) == "" ||
		strings.TrimSpace(result.Step) == "" || result.Ordinal < 0 || !result.Status.Terminal() ||
		result.LogStart < 0 || result.LogEnd < result.LogStart || result.DeclaredSize == "" || !result.DeclaredSize.Valid() ||
		result.MaxRSSBytes < 0 || result.UserCPU < 0 || result.SystemCPU < 0 {
		return model.StepResult{}, fmt.Errorf("%w: step result fields are invalid", ErrInvalid)
	}
	recorded := unixNano(s.now())
	row := s.db.QueryRowContext(ctx, `
INSERT INTO step_results(
    run_id, burn, step, ordinal, status, exit_code, log_start, log_end,
    declared_size, max_rss_bytes, user_cpu_ns, system_cpu_ns, started_at, finished_at, recorded_at
)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(run_id, burn, step) DO UPDATE SET
    ordinal = excluded.ordinal,
    status = excluded.status,
    exit_code = excluded.exit_code,
    log_start = excluded.log_start,
    log_end = excluded.log_end,
    declared_size = excluded.declared_size,
    max_rss_bytes = excluded.max_rss_bytes,
    user_cpu_ns = excluded.user_cpu_ns,
    system_cpu_ns = excluded.system_cpu_ns,
    started_at = excluded.started_at,
    finished_at = excluded.finished_at,
    recorded_at = excluded.recorded_at
RETURNING run_id, burn, step, ordinal, status, exit_code, log_start, log_end,
		  declared_size, max_rss_bytes, user_cpu_ns, system_cpu_ns, started_at, finished_at, recorded_at`,
		result.RunID, result.Burn, result.Step, result.Ordinal, result.Status,
		result.ExitCode, result.LogStart, result.LogEnd, result.DeclaredSize,
		result.MaxRSSBytes, result.UserCPU.Nanoseconds(), result.SystemCPU.Nanoseconds(),
		nullableUnixNano(result.StartedAt), nullableUnixNano(result.FinishedAt), recorded)
	value, err := scanStepResult(row)
	if err != nil {
		return model.StepResult{}, fmt.Errorf("put step result: %w", err)
	}
	return value, nil
}

func (s *Store) StepResults(ctx context.Context, runID string) ([]model.StepResult, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT run_id, burn, step, ordinal, status, exit_code, log_start, log_end,
	   declared_size, max_rss_bytes, user_cpu_ns, system_cpu_ns, started_at, finished_at, recorded_at
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
	var release, credentialed int64
	var queued, created, updated int64
	var started, finished sql.NullInt64
	if err := row.Scan(
		&value.QueueSequence, &value.ID, &value.RepoID, &value.RefKind, &value.Ref,
		&value.SHA, &value.Actor, &release, &credentialed, &value.Trigger, &value.Phase, &value.JobName,
		&value.TestedSHA, &value.BaseSHA, &value.FailedBurn, &value.FailedStep, &value.Error,
		&value.Status, &value.Reason, &value.SupersededBy, &queued, &started, &finished,
		&created, &updated,
	); err != nil {
		return model.Run{}, err
	}
	value.Release = release != 0
	value.Credentialed = credentialed != 0
	value.QueuedAt = fromUnixNano(queued)
	value.StartedAt, value.FinishedAt = nullableTime(started), nullableTime(finished)
	value.CreatedAt, value.UpdatedAt = fromUnixNano(created), fromUnixNano(updated)
	return value, nil
}

func scanStepResult(row rowScanner) (model.StepResult, error) {
	var value model.StepResult
	var started, finished sql.NullInt64
	var userCPUNanos, systemCPUNanos int64
	var recorded int64
	if err := row.Scan(&value.RunID, &value.Burn, &value.Step, &value.Ordinal,
		&value.Status, &value.ExitCode, &value.LogStart, &value.LogEnd,
		&value.DeclaredSize, &value.MaxRSSBytes, &userCPUNanos, &systemCPUNanos,
		&started, &finished, &recorded); err != nil {
		return model.StepResult{}, err
	}
	value.UserCPU = time.Duration(userCPUNanos)
	value.SystemCPU = time.Duration(systemCPUNanos)
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

// SetRunCredentialed marks a running run as credentialed. It is called by the
// scheduler when the pipeline document declares secret-store paths, after the
// pipeline has been read from the immutable checkout. The update is a no-op on
// a run that is already credentialed or no longer active.
func (s *Store) SetRunCredentialed(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: run ID is required", ErrInvalid)
	}
	_, err := s.db.ExecContext(ctx, `
UPDATE runs SET credentialed = 1, updated_at = ?
WHERE id = ? AND status IN ('queued', 'running') AND credentialed = 0`,
		unixNano(s.now()), id)
	if err != nil {
		return fmt.Errorf("set run credentialed: %w", err)
	}
	return nil
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
