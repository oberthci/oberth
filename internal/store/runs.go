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
queue_sequence, COALESCE(admission_sequence, queue_sequence), id, repo_id, ref_kind, ref, sha, actor, release, credentialed, trigger,
phase, job_name, tested_sha, base_sha, failed_burn, failed_step, error,
status, reason, superseded_by, queued_at, started_at, finished_at, created_at, updated_at,
concurrency_group, schedule_entry, ` + basePrioritySQL + `, ` + priorityReasonSQL + `,
COALESCE(` + overridePrioritySQL + `,''), COALESCE((SELECT reason FROM run_priority_overrides WHERE run_id=id),'')`

// ErrRunCursorNotFound lets clients restart paging without exposing a missing run ID.
var ErrRunCursorNotFound = fmt.Errorf("%w: cursor run no longer retained; restart from the first page", ErrNotFound)

// RunsByIDs resolves successor runs for list presentation in one query.
func (s *Store) RunsByIDs(ctx context.Context, ids []string) (map[string]model.Run, error) {
	result := make(map[string]model.Run, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.reader().QueryContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id IN (`+placeholders+`)`, args...) // #nosec G202 -- only placeholders are interpolated.
	if err != nil {
		return nil, fmt.Errorf("list runs by ID: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run by ID: %w", err)
		}
		result[run.ID] = run
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runs by ID: %w", err)
	}
	return result, nil
}

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
	result, err := s.enqueueRunTx(ctx, tx, spec, id, now, 0)
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
	// Credentialed is NOT an exclusion: on branch runs it only records that
	// the ci-secrets tier was mounted, and the requeued copy re-earns it
	// through the scheduler's own pipelineCredentialed admission. Release
	// (tag-tier) work stays excluded.
	if run.RefKind != model.RefBranch || run.Trigger == "promotion" || run.Release {
		return model.Run{}, ErrRequeueIneligible
	}
	// A newer push may already have replayed into the queue while this
	// process was starting; requeueing the older SHA behind it would
	// supersede the newer work backwards. The branch keeps exactly its
	// newest active run.
	var activeSiblings int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM runs
WHERE repo_id = ? AND ref_kind = 'branch' AND ref = ? AND id != ? AND schedule_entry = ?
  AND status IN ('queued', 'running')`, run.RepoID, run.Ref, run.ID, run.ScheduleEntry).Scan(&activeSiblings); err != nil {
		return model.Run{}, fmt.Errorf("check active branch siblings: %w", err)
	}
	if activeSiblings > 0 {
		return model.Run{}, ErrRequeueIneligible
	}
	// The copy starts uncredentialed: credential mounting is a per-run
	// admission decision the scheduler re-derives, never inherited state.
	spec, err := validateRunSpec(model.RunSpec{
		RepoID: run.RepoID, RefKind: run.RefKind, Ref: run.Ref, SHA: run.SHA,
		Actor: run.Actor, Release: run.Release, Credentialed: false,
		Trigger: run.Trigger, ScheduleEntry: run.ScheduleEntry, TestedSHA: run.TestedSHA, BaseSHA: run.BaseSHA,
	})
	if err != nil {
		return model.Run{}, err
	}
	id, err := randomID()
	if err != nil {
		return model.Run{}, fmt.Errorf("generate requeued run id: %w", err)
	}
	result, err := s.enqueueRunTx(ctx, tx, spec, id, now, run.AdmissionSequence)
	if err != nil {
		return model.Run{}, fmt.Errorf("requeue stranded run %s: %w", run.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET reason = 're-queued after restart' WHERE id = ? AND superseded_by = ?`, run.ID, result.ID); err != nil {
		return model.Run{}, fmt.Errorf("record restart requeue reason: %w", err)
	}
	return s.inheritQueuePriorityTx(ctx, tx, run, result.Run)
}

func validateRunSpec(spec model.RunSpec) (model.RunSpec, error) {
	if spec.RepoID <= 0 || !spec.RefKind.Valid() || strings.TrimSpace(spec.Ref) == "" ||
		!validOID(spec.SHA) || strings.TrimSpace(spec.Actor) == "" || strings.TrimSpace(spec.Trigger) == "" {
		return model.RunSpec{}, fmt.Errorf("%w: run fields are invalid", ErrInvalid)
	}
	if len(spec.ScheduleEntry) > 64 || strings.TrimSpace(spec.ScheduleEntry) != spec.ScheduleEntry ||
		(spec.ScheduleEntry != "" && (spec.Trigger != "schedule" || spec.RefKind != model.RefBranch || spec.Release)) {
		return model.RunSpec{}, fmt.Errorf("%w: schedule entry requires a scheduled branch run and at most 64 bytes", ErrInvalid)
	}
	if spec.TestedSHA == "" {
		spec.TestedSHA = spec.SHA
	}
	if !validOID(spec.TestedSHA) || (spec.BaseSHA != "" && !validOID(spec.BaseSHA)) {
		return model.RunSpec{}, fmt.Errorf("%w: tested or base SHA is invalid", ErrInvalid)
	}
	return spec, nil
}

func (s *Store) enqueueRunTx(ctx context.Context, tx *sql.Tx, spec model.RunSpec, id string, now, inheritedAdmissionSequence int64) (model.EnqueueRunResult, error) {
	if err := requireRepositoryActive(ctx, tx, spec.RepoID); err != nil {
		return model.EnqueueRunResult{}, err
	}
	var cancellations []model.RunCancellation
	// Supersession stays inside a branch and its schedule entry. Named
	// schedules are independent of other entries and ordinary branch pushes.
	// Promotion CI uses a
	// synthetic branch-shaped ref, but it is an independent immutable attempt
	// and must neither supersede nor be superseded by a client push. A pending
	// publication also owns its run until the external mutation terminalizes;
	// newer work stays queued behind that durable obligation.
	if spec.RefKind == model.RefBranch && spec.Trigger != "promotion" {
		rows, err := tx.QueryContext(ctx, `
SELECT id, job_name, phase FROM runs
WHERE repo_id = ? AND ref_kind = 'branch' AND ref = ? AND schedule_entry = ?
  AND trigger != 'promotion'
  AND NOT EXISTS (
      SELECT 1 FROM publications
      WHERE publications.run_id = runs.id AND publications.status = 'pending'
  )
  AND status IN ('queued', 'running') AND (status = 'running' OR job_name != '')
ORDER BY queue_sequence`, spec.RepoID, spec.Ref, spec.ScheduleEntry)
		if err != nil {
			return model.EnqueueRunResult{}, fmt.Errorf("find running superseded runs: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var cancellation model.RunCancellation
			var phase string
			if err := rows.Scan(&cancellation.RunID, &cancellation.JobName, &phase); err != nil {
				_ = rows.Close()
				return model.EnqueueRunResult{}, fmt.Errorf("scan running superseded run: %w", err)
			}
			cancellation.SupersededBy = id
			cancellation.Reason = "superseded"
			if phase == "infrastructure-pending" {
				cancellation.Reason = "owner_restart"
			}
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
    tested_sha, base_sha, schedule_entry, status, queued_at, created_at, updated_at
)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?, 'queued', ?, ?, ?)
RETURNING `+runColumns,
		id, spec.RepoID, spec.RefKind, spec.Ref, spec.SHA, spec.Actor, spec.Release,
		spec.Credentialed, spec.Trigger, spec.TestedSHA, spec.BaseSHA, spec.ScheduleEntry, now, now, now)
	value, err := scanRun(row)
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("insert run: %w", err)
	}
	admissionSequence := value.QueueSequence
	if inheritedAdmissionSequence > 0 {
		admissionSequence = inheritedAdmissionSequence
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET admission_sequence = ? WHERE id = ?`, admissionSequence, id); err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("set run admission sequence: %w", err)
	}
	value.AdmissionSequence = admissionSequence
	if err := registerRunIssueWork(ctx, tx, value, now); err != nil {
		return model.EnqueueRunResult{}, err
	}
	if spec.RefKind == model.RefBranch && spec.Trigger != "promotion" {
		if _, err := tx.ExecContext(ctx, `
UPDATE runs
SET status = 'interrupted', phase = 'interrupted', reason = ?, superseded_by = ?,
    finished_at = ?, updated_at = ?
WHERE id != ? AND repo_id = ? AND ref_kind = 'branch' AND ref = ? AND schedule_entry = ?
  AND trigger != 'promotion'
  AND NOT EXISTS (
      SELECT 1 FROM publications
      WHERE publications.run_id = runs.id AND publications.status = 'pending'
  )
  AND status IN ('queued', 'running')`,
			"superseded by newer run", id, now, now, id, spec.RepoID, spec.Ref, spec.ScheduleEntry); err != nil {
			return model.EnqueueRunResult{}, fmt.Errorf("supersede earlier runs: %w", err)
		}
		for _, cancellation := range cancellations {
			// Upsert: a run superseded across a restart may already carry an
			// owner_restart obligation (run_id is the primary key). The fresh
			// supersede obligation re-arms completion (retaining late-create
			// cleanup for infrastructure-pending runs) so the
			// stranded Job is still deleted exactly once (issue #270).
			if _, err := tx.ExecContext(ctx, `
INSERT INTO run_cancellations(run_id, job_name, superseded_by, reason, created_at)
VALUES(?, ?, ?, ?, ?)
ON CONFLICT(run_id) DO UPDATE SET
    job_name = excluded.job_name,
    superseded_by = excluded.superseded_by,
    reason = excluded.reason,
    completed_at = NULL`, cancellation.RunID, cancellation.JobName, cancellation.SupersededBy, cancellation.Reason, now); err != nil {
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
SET status = 'running', phase = 'waiting', updated_at = ?
WHERE queue_sequence = (
    SELECT queue_sequence FROM runs WHERE `+claimableRunSQL+`
    ORDER BY `+priorityOrderSQL(fromUnixNano(now))+` LIMIT 1
)
RETURNING `+runColumns, now))
	if err != nil {
		return model.Run{}, err
	}
	return value, nil
}

// MarkRunAdmitted records when the weighted gate grants a run. The run is
// visibly running from this point, including during a trusted VM preflight.
// A cancellation that won before the grant cannot acquire a start time.
func (s *Store) MarkRunAdmitted(ctx context.Context, id string) (model.Run, error) {
	if strings.TrimSpace(id) == "" {
		return model.Run{}, fmt.Errorf("%w: run ID is required", ErrInvalid)
	}
	now := unixNano(s.now())
	value, err := scanRun(s.db.QueryRowContext(ctx, `
UPDATE runs SET started_at = ?, updated_at = ?
WHERE id = ? AND status = 'running' AND started_at IS NULL
RETURNING `+runColumns, now, now, id))
	if errors.Is(err, sql.ErrNoRows) {
		current, lookupErr := s.Run(ctx, id)
		if lookupErr != nil {
			return model.Run{}, lookupErr
		}
		if current.Preempted() {
			return current, nil
		}
		return model.Run{}, fmt.Errorf("%w: run is not awaiting admission", ErrInvalidState)
	}
	if err != nil {
		return model.Run{}, fmt.Errorf("mark run admitted: %w", err)
	}
	return value, nil
}

// MarkRunParked clears the admission time after a trusted VM reservation is
// refused and the weighted gate releases the run's resources. A submitted
// Workflow cannot be parked.
func (s *Store) MarkRunParked(ctx context.Context, id string) (model.Run, error) {
	if strings.TrimSpace(id) == "" {
		return model.Run{}, fmt.Errorf("%w: run ID is required", ErrInvalid)
	}
	now := unixNano(s.now())
	value, err := scanRun(s.db.QueryRowContext(ctx, `
UPDATE runs SET started_at = NULL, updated_at = ?
WHERE id = ? AND status = 'running' AND job_name = '' AND started_at IS NOT NULL
RETURNING `+runColumns, now, id))
	if errors.Is(err, sql.ErrNoRows) {
		current, lookupErr := s.Run(ctx, id)
		if lookupErr != nil {
			return model.Run{}, lookupErr
		}
		if current.Preempted() {
			return current, nil
		}
		return model.Run{}, fmt.Errorf("%w: run is not awaiting VM capacity", ErrInvalidState)
	}
	if err != nil {
		return model.Run{}, fmt.Errorf("mark run parked: %w", err)
	}
	return value, nil
}

// MarkRunSubmitted records Workflow submission and advances the phase to
// running. It preserves the start time granted at admission; older callers
// that submit without a separate grant receive a start time here.
func (s *Store) MarkRunSubmitted(ctx context.Context, id string) (model.Run, error) {
	if strings.TrimSpace(id) == "" {
		return model.Run{}, fmt.Errorf("%w: run ID is required", ErrInvalid)
	}
	now := unixNano(s.now())
	value, err := scanRun(s.db.QueryRowContext(ctx, `
UPDATE runs SET phase = 'running', started_at = COALESCE(started_at, ?), updated_at = ?
WHERE id = ? AND status = 'running' AND phase = 'waiting' AND job_name != ''
RETURNING `+runColumns, now, now, id))
	if errors.Is(err, sql.ErrNoRows) {
		current, lookupErr := s.Run(ctx, id)
		if lookupErr != nil {
			return model.Run{}, lookupErr
		}
		if current.Preempted() {
			return current, nil
		}
		return model.Run{}, fmt.Errorf("%w: run is not awaiting submission", ErrInvalidState)
	}
	if err != nil {
		return model.Run{}, fmt.Errorf("mark run submitted: %w", err)
	}
	return value, nil
}

// RunningRunsWithJobs returns runs in status "running" that have a known
// deterministic Job name, excluding runs owned by a pending publication.
// The scheduler uses this at startup to reconcile runs stranded by a crash.
func (s *Store) RunningRunsWithJobs(ctx context.Context) ([]model.Run, error) {
	rows, err := s.reader().QueryContext(ctx, `
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
	value, err := scanRun(s.reader().QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, id))
	if err != nil {
		return model.Run{}, translateNotFound("run", err)
	}
	return value, nil
}

// RunIDsExist reports which of ids name a run in this database. It reads only
// the runs table's primary key, so it works on every schema a restore can
// verify; `oberth restore` uses it to find Argo Workflows whose runs exist
// only in the database being replaced.
func (s *Store) RunIDsExist(ctx context.Context, ids []string) (map[string]bool, error) {
	exists := make(map[string]bool, len(ids))
	for _, id := range ids {
		var found int
		if err := s.reader().QueryRowContext(ctx, `SELECT count(*) FROM runs WHERE id = ?`, id).Scan(&found); err != nil {
			return nil, fmt.Errorf("look up run %s: %w", id, err)
		}
		exists[id] = found > 0
	}
	return exists, nil
}

// ResolveRunID looks up a full durable ID or a unique hexadecimal prefix of
// at least 8 characters, as printed by push feedback.
// Keep Run exact-only: mutation and scheduler identity lookups never abbreviate.
func (s *Store) ResolveRunID(ctx context.Context, id string) (model.Run, error) {
	return s.ResolveRunIDIn(ctx, id, nil)
}

// ResolveRunIDIn resolves like ResolveRunID among runs of repoIDs only (nil:
// every repository), so a prefix is unique, or ambiguous, within that set
// alone (#073).
func (s *Store) ResolveRunIDIn(ctx context.Context, id string, repoIDs []int64) (model.Run, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if len(id) < 8 || len(id) > 32 || !isHexPrefix(id) {
		return model.Run{}, fmt.Errorf("%w: run ID must be 8 to 32 hexadecimal characters", ErrInvalid)
	}
	if len(id) == 32 && repoIDs == nil {
		return s.Run(ctx, id)
	}
	restriction, restrictionArgs := repoRestriction("repo_id", repoIDs)
	// Hex IDs sort below 'g', so this range uses the existing primary key.
	// Restrict scope before checking ambiguity or returning matching IDs.
	rows, err := s.reader().QueryContext(ctx, `SELECT id FROM runs WHERE id >= ? AND id < ?`+restriction+` ORDER BY id`, // #nosec G202 -- the restriction is a fixed column and ? placeholders from repoRestriction.
		append([]any{id, id + "g"}, restrictionArgs...)...)
	if err != nil {
		return model.Run{}, fmt.Errorf("resolve run ID prefix: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var matches []string
	for rows.Next() {
		var match string
		if err := rows.Scan(&match); err != nil {
			return model.Run{}, fmt.Errorf("scan run ID prefix: %w", err)
		}
		matches = append(matches, match)
	}
	if err := rows.Err(); err != nil {
		return model.Run{}, fmt.Errorf("resolve run ID prefix: %w", err)
	}
	if err := rows.Close(); err != nil {
		return model.Run{}, fmt.Errorf("close run ID prefix: %w", err)
	}
	switch len(matches) {
	case 0:
		if len(id) == 32 {
			return model.Run{}, fmt.Errorf("%w: run", ErrNotFound)
		}
		return model.Run{}, fmt.Errorf("%w: run ID %q", ErrNotFound, id)
	case 1:
		return s.Run(ctx, matches[0])
	default:
		return model.Run{}, fmt.Errorf("%w: run ID prefix %q matches: %s; use the full run ID", ErrAmbiguous, id, strings.Join(matches, ", "))
	}
}

func (s *Store) LatestRunForRef(ctx context.Context, repoID int64, ref string) (model.Run, error) {
	value, err := scanRun(s.reader().QueryRowContext(ctx, `
SELECT `+runColumns+` FROM runs
WHERE repo_id = ? AND ref = ? ORDER BY queue_sequence DESC LIMIT 1`, repoID, ref))
	if err != nil {
		return model.Run{}, translateNotFound("run ref", err)
	}
	return value, nil
}

func (s *Store) LatestRunForSHA(ctx context.Context, repoID int64, sha string) (model.Run, error) {
	value, err := scanRun(s.reader().QueryRowContext(ctx, `
SELECT `+runColumns+` FROM runs
WHERE repo_id = ? AND sha = ? ORDER BY queue_sequence DESC LIMIT 1`, repoID, sha))
	if err != nil {
		return model.Run{}, translateNotFound("run SHA", err)
	}
	return value, nil
}

// HasPassedRunForSHA gates SHA-pinned fragments on a passed run that tested
// that exact commit in the fragment's own registered repository.
func (s *Store) HasPassedRunForSHA(ctx context.Context, name, sha string) (bool, error) {
	if !validOID(sha) || len(sha) != 40 {
		return false, fmt.Errorf("%w: fragment SHA must be 40 lowercase hexadecimal characters", ErrInvalid)
	}
	repo, err := s.RepositoryByName(ctx, name)
	if err != nil {
		return false, err
	}
	var passed bool
	err = s.reader().QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM runs WHERE repo_id = ? AND tested_sha = ? AND status = 'passed'
)`, repo.ID, sha).Scan(&passed)
	if err != nil {
		return false, fmt.Errorf("check passed fragment run: %w", err)
	}
	return passed, nil
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
		if err := s.reader().QueryRowContext(ctx, `
SELECT count(DISTINCT sha) FROM runs WHERE repo_id = ? AND sha LIKE ?`, repoID, lower+"%").Scan(&matches); err != nil {
			return model.Run{}, fmt.Errorf("resolve abbreviated SHA: %w", err)
		}
		switch {
		case matches > 1:
			return model.Run{}, fmt.Errorf("%w: %q", ErrAmbiguous, selector)
		case matches == 1:
			value, err := scanRun(s.reader().QueryRowContext(ctx, `
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

// ResolveWaitRun resolves a SHA within the requested trigger before selecting
// the newest run. Tag runs can be addressed by their immutable tag object or
// their peeled commit; TestedSHA on other ref kinds is not an alias.
func (s *Store) ResolveWaitRun(ctx context.Context, repoID int64, selector, trigger string) (model.Run, error) {
	selector = strings.ToLower(strings.TrimSpace(selector))
	if repoID <= 0 || !isHexPrefix(selector) || len(selector) < 7 || len(selector) > 64 {
		return model.Run{}, fmt.Errorf("%w: repository and full or short SHA are required", ErrInvalid)
	}
	if validOID(selector) {
		run, err := s.latestWaitRunForSHA(ctx, repoID, selector, trigger)
		if err == nil || !errors.Is(err, ErrNotFound) {
			return run, err
		}
	}

	// Count identities, not runs: several annotated tags may peel to the
	// same selected commit, but a prefix spanning distinct objects is unsafe.
	rows, err := s.reader().QueryContext(ctx, `
SELECT sha FROM runs
WHERE repo_id = ? AND (? = '' OR trigger = ?) AND sha LIKE ?
UNION
SELECT tested_sha FROM runs
WHERE repo_id = ? AND (? = '' OR trigger = ?) AND ref_kind = 'tag' AND tested_sha LIKE ?
LIMIT 2`, repoID, trigger, trigger, selector+"%", repoID, trigger, trigger, selector+"%")
	if err != nil {
		return model.Run{}, fmt.Errorf("resolve wait SHA: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var identities []string
	for rows.Next() {
		var identity string
		if err := rows.Scan(&identity); err != nil {
			return model.Run{}, fmt.Errorf("scan wait SHA: %w", err)
		}
		identities = append(identities, identity)
	}
	rowsErr := rows.Err()
	if err := rows.Close(); err != nil {
		return model.Run{}, fmt.Errorf("close wait SHA lookup: %w", err)
	}
	if rowsErr != nil {
		return model.Run{}, fmt.Errorf("resolve wait SHA: %w", rowsErr)
	}
	switch len(identities) {
	case 0:
		return model.Run{}, fmt.Errorf("%w: wait SHA %q", ErrNotFound, selector)
	case 1:
		return s.latestWaitRunForSHA(ctx, repoID, identities[0], trigger)
	default:
		return model.Run{}, fmt.Errorf("%w: %q", ErrAmbiguous, selector)
	}
}

func (s *Store) latestWaitRunForSHA(ctx context.Context, repoID int64, sha, trigger string) (model.Run, error) {
	run, err := scanRun(s.reader().QueryRowContext(ctx, `
SELECT `+runColumns+` FROM runs
WHERE repo_id = ? AND (? = '' OR trigger = ?)
  AND (sha = ? OR (ref_kind = 'tag' AND tested_sha = ?))
ORDER BY queue_sequence DESC LIMIT 1`, repoID, trigger, trigger, sha, sha))
	if err != nil {
		return model.Run{}, translateNotFound("wait SHA", err)
	}
	return run, nil
}

// latestRunForRefWithAmbiguityCheck resolves a bare ref name and returns
// ErrAmbiguous when the same name appears as both a branch and a tag ref.
func (s *Store) latestRunForRefWithAmbiguityCheck(ctx context.Context, repoID int64, ref string) (model.Run, error) {
	var kinds int
	if err := s.reader().QueryRowContext(ctx, `
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
	value, err := scanRun(s.reader().QueryRowContext(ctx, `
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
	rows, err := s.reader().QueryContext(ctx, `
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
    (status = 'interrupted' AND (superseded_by != '' OR phase = 'cancelled'))
)
AND NOT EXISTS (SELECT 1 FROM vm_executions WHERE run_id=runs.id AND execution_kind='kvm-preflight' AND (cleaned=0 OR receipt_json='' OR refusal_json!=''))
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
	if value.Preempted() {
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
	rows, err := s.reader().QueryContext(ctx, `
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
	err = s.reader().QueryRowContext(ctx, `
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
// a worker is superseded or cancelled after claim but exits before publishing
// its deterministic Job name. Callers invoke this only after that exact worker has
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
        AND runs.status = 'interrupted'
        AND (runs.superseded_by != '' OR runs.phase = 'cancelled')
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
	value, err := s.finishRunTx(ctx, tx, id, result, phase, now)
	if err != nil {
		return model.Run{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Run{}, fmt.Errorf("commit finish run: %w", err)
	}
	return value, nil
}

func (s *Store) finishRunTx(ctx context.Context, tx *sql.Tx, id string, result model.RunResult, phase string, now int64) (model.Run, error) {
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
		if err := s.projectPromotionIssue(ctx, tx, promotion, now); err != nil {
			return model.Run{}, err
		}
	} else if err := s.projectRunIssue(ctx, tx, value, result.FailureTail, now); err != nil {
		return model.Run{}, err
	}
	if err := s.recordTerminalObservationTx(ctx, tx, value, result.TerminalObservation, now); err != nil {
		return model.Run{}, err
	}
	return value, nil
}

func (s *Store) ListRecentRuns(ctx context.Context, filter model.RunListFilter) ([]model.Run, error) {
	if !filter.Terminal {
		return s.listFlatRuns(ctx, filter)
	}
	restriction, restrictionArgs := repoRestriction("repo_id", filter.RepoIDs)
	args := append([]any{filter.RepoID, filter.RepoID, filter.Ref, filter.Ref, filter.Before, filter.Before, filter.Terminal}, restrictionArgs...)
	rows, err := s.reader().QueryContext(ctx, `
SELECT `+runColumns+` FROM runs
WHERE (? = 0 OR repo_id = ?) AND (? = '' OR ref = ?) AND (? <= 0 OR queue_sequence < ?)
  AND (? = 0 OR status NOT IN ('queued', 'running'))`+restriction+`
ORDER BY queue_sequence DESC LIMIT ?`, append(args, pageLimit(filter.Limit))...) // #nosec G202 -- the restriction is a fixed column and ? placeholders from repoRestriction.
	if err != nil {
		return nil, fmt.Errorf("list recent runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	runs := make([]model.Run, 0)
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

// listFlatRuns puts the entire bounded queue on the first page. The remaining
// page uses a fixed (admission/end time, ID) cursor: a run admitted between
// page loads cannot move onto an older page and appear twice. Both blocks
// and the cursor are read from one snapshot so admission cannot duplicate a row.
func (s *Store) listFlatRuns(ctx context.Context, filter model.RunListFilter) ([]model.Run, error) {
	tx, err := s.reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin flat run list snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	restriction, restrictionArgs := repoRestriction("repo_id", filter.RepoIDs)
	runs := make([]model.Run, 0)
	if filter.Before == 0 && filter.BeforeRun == "" {
		queued, err := func() ([]model.Run, error) {
			args := append([]any{filter.RepoID, filter.RepoID, filter.Ref, filter.Ref}, restrictionArgs...)
			priorityOrder := priorityOrderSQL(s.now())
			rows, err := tx.QueryContext(ctx, `
WITH queue AS (
  SELECT *, ROW_NUMBER() OVER (ORDER BY `+priorityOrder+`) AS queue_position FROM runs
  WHERE status = 'queued' OR (status = 'running' AND started_at IS NULL)
)
SELECT `+runColumns+`, queue_position FROM queue AS runs
WHERE (? = 0 OR repo_id = ?) AND (? = '' OR ref = ?)`+restriction+`
ORDER BY `+priorityOrder+` LIMIT ?`, append(args, MaxActiveRuns)...) // #nosec G202 -- fixed columns and parameterized restriction.
			if err != nil {
				return nil, fmt.Errorf("list queued runs: %w", err)
			}
			defer func() { _ = rows.Close() }()
			queued := make([]model.Run, 0)
			for rows.Next() {
				var position int64
				value, scanErr := scanRun(rows, &position)
				value.QueuePosition = position
				if scanErr != nil {
					return nil, fmt.Errorf("scan queued run: %w", scanErr)
				}
				queued = append(queued, value)
			}
			if err := rows.Err(); err != nil {
				return nil, fmt.Errorf("list queued runs: %w", err)
			}
			return queued, nil
		}()
		if err != nil {
			return nil, err
		}
		runs = append(runs, queued...)
	}
	var beforeTime int64
	var beforeID string
	if filter.BeforeRun != "" || filter.Before > 0 {
		var cursor model.Run
		var err error
		if filter.BeforeRun != "" {
			cursor, err = scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, filter.BeforeRun))
		} else {
			cursor, err = scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE queue_sequence = ?`, filter.Before))
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRunCursorNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("read run list cursor: %w", err)
		}
		beforeID = cursor.ID
		beforeTime = unixNano(runListTime(cursor))
	}
	args := append([]any{filter.RepoID, filter.RepoID, filter.Ref, filter.Ref,
		beforeID, beforeTime, beforeTime, beforeID}, restrictionArgs...)
	rows, err := tx.QueryContext(ctx, `
SELECT `+runColumns+` FROM runs
WHERE NOT (status = 'queued' OR (status = 'running' AND started_at IS NULL))
  AND (? = 0 OR repo_id = ?) AND (? = '' OR ref = ?)
  AND (? = '' OR COALESCE(started_at, finished_at) < ?
       OR (COALESCE(started_at, finished_at) = ? AND id < ?))`+restriction+`
ORDER BY COALESCE(started_at, finished_at) DESC, id DESC
LIMIT ?`, append(args, pageLimit(filter.Limit))...) // #nosec G202 -- fixed columns and parameterized restriction.
	if err != nil {
		return nil, fmt.Errorf("list admitted runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		value, scanErr := scanRun(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("scan admitted run: %w", scanErr)
		}
		runs = append(runs, value)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list admitted runs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close admitted runs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit flat run list snapshot: %w", err)
	}
	return runs, nil
}

func runListTime(run model.Run) time.Time {
	if run.StartedAt != nil {
		return *run.StartedAt
	}
	if run.FinishedAt != nil {
		return *run.FinishedAt
	}
	return run.QueuedAt
}

// CountRuns counts the retained runs, in total and per repository, so a
// paged list can show how many runs exist beyond the page it holds.
func (s *Store) CountRuns(ctx context.Context) (model.RunCounts, error) {
	counts := model.RunCounts{ByRepo: map[int64]int{}, QueuedByRepo: map[int64]int{}, RunningByRepo: map[int64]int{}}
	rows, err := s.reader().QueryContext(ctx, `SELECT repo_id, COUNT(*),
SUM(status = 'queued' OR (status = 'running' AND started_at IS NULL)),
SUM(status = 'running' AND started_at IS NOT NULL)
FROM runs GROUP BY repo_id`)
	if err != nil {
		return counts, fmt.Errorf("count runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var repoID int64
		var n, queued, running int
		if err := rows.Scan(&repoID, &n, &queued, &running); err != nil {
			return counts, fmt.Errorf("scan run count: %w", err)
		}
		counts.ByRepo[repoID] = n
		counts.Total += n
		counts.QueuedByRepo[repoID], counts.RunningByRepo[repoID] = queued, running
		counts.Queued += queued
		counts.Running += running
	}
	if err := rows.Err(); err != nil {
		return counts, fmt.Errorf("count runs: %w", err)
	}
	return counts, nil
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
	rows, err := s.reader().QueryContext(ctx, `
SELECT `+runColumns+` FROM (
    SELECT *, ROW_NUMBER() OVER (PARTITION BY repo_id ORDER BY queue_sequence DESC) AS rn
    FROM runs
) AS runs WHERE rn <= ?
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
		negativeMeasurement(result.MaxRSSBytes) || negativeMeasurement(result.MemoryPeakBytes) || negativeMeasurement(result.UserCPU) || negativeMeasurement(result.SystemCPU) {
		return model.StepResult{}, fmt.Errorf("%w: step result fields are invalid", ErrInvalid)
	}
	recorded := unixNano(s.now())
	row := s.db.QueryRowContext(ctx, `
INSERT INTO step_results(
    run_id, burn, step, ordinal, status, exit_code, log_start, log_end,
    declared_size, max_rss_bytes, user_cpu_ns, system_cpu_ns, started_at, finished_at, recorded_at, termination_json, memory_peak_bytes
)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(run_id, burn, step) DO UPDATE SET
    ordinal = excluded.ordinal,
    status = excluded.status,
    exit_code = excluded.exit_code,
    log_start = excluded.log_start,
    log_end = excluded.log_end,
    declared_size = excluded.declared_size,
    max_rss_bytes = COALESCE(excluded.max_rss_bytes, step_results.max_rss_bytes),
    memory_peak_bytes = COALESCE(excluded.memory_peak_bytes, step_results.memory_peak_bytes),
    user_cpu_ns = COALESCE(excluded.user_cpu_ns, step_results.user_cpu_ns),
    system_cpu_ns = COALESCE(excluded.system_cpu_ns, step_results.system_cpu_ns),
    started_at = excluded.started_at,
    finished_at = excluded.finished_at,
    recorded_at = excluded.recorded_at,
    termination_json = excluded.termination_json
RETURNING run_id, burn, step, ordinal, status, exit_code, log_start, log_end,
		  declared_size, max_rss_bytes, user_cpu_ns, system_cpu_ns, started_at, finished_at, recorded_at, termination_json, memory_peak_bytes`,
		result.RunID, result.Burn, result.Step, result.Ordinal, result.Status,
		result.ExitCode, result.LogStart, result.LogEnd, result.DeclaredSize,
		result.MaxRSSBytes, result.UserCPU, result.SystemCPU,
		nullableUnixNano(result.StartedAt), nullableUnixNano(result.FinishedAt), recorded, stepDiagnosticJSON(result.Termination, result.RunID), result.MemoryPeakBytes)
	value, err := scanStepResult(row)
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
	rows, err := database.QueryContext(ctx, `
SELECT run_id, burn, step, ordinal, status, exit_code, log_start, log_end,
	   declared_size, max_rss_bytes, user_cpu_ns, system_cpu_ns, started_at, finished_at, recorded_at, termination_json, memory_peak_bytes
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

func scanRun(row rowScanner, extra ...any) (model.Run, error) {
	var value model.Run
	var release, credentialed int64
	var queued, created, updated int64
	var started, finished sql.NullInt64
	dest := []any{
		&value.QueueSequence, &value.AdmissionSequence, &value.ID, &value.RepoID, &value.RefKind, &value.Ref,
		&value.SHA, &value.Actor, &release, &credentialed, &value.Trigger, &value.Phase, &value.JobName,
		&value.TestedSHA, &value.BaseSHA, &value.FailedBurn, &value.FailedStep, &value.Error,
		&value.Status, &value.Reason, &value.SupersededBy, &queued, &started, &finished,
		&created, &updated, &value.ConcurrencyGroup, &value.ScheduleEntry, &value.Priority, &value.PriorityReason, &value.PriorityOverride, &value.PriorityOverrideReason,
	}
	if err := row.Scan(append(dest, extra...)...); err != nil {
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
	var userCPUNanos, systemCPUNanos, rss, peak sql.NullInt64
	var recorded int64
	var diagnostic sql.NullString
	if err := row.Scan(&value.RunID, &value.Burn, &value.Step, &value.Ordinal,
		&value.Status, &value.ExitCode, &value.LogStart, &value.LogEnd,
		&value.DeclaredSize, &rss, &userCPUNanos, &systemCPUNanos,
		&started, &finished, &recorded, &diagnostic, &peak); err != nil {
		return model.StepResult{}, err
	}
	value.Termination = readStepDiagnostic(diagnostic, value.RunID)
	value.MaxRSSBytes = measurementPointer[int64](rss)
	value.MemoryPeakBytes = measurementPointer[int64](peak)
	value.UserCPU = measurementPointer[time.Duration](userCPUNanos)
	value.SystemCPU = measurementPointer[time.Duration](systemCPUNanos)
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
// a run that is already credentialed or no longer active. A submitted VM
// execution must finish observed cleanup before its run can become credentialed.
func (s *Store) SetRunCredentialed(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: run ID is required", ErrInvalid)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin credential transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Serialize this check with SubmitVMExecution's eligibility check. If
	// credential discovery wins first, submission refuses it; if submission
	// wins, this transition refuses until the owned resources are gone.
	var conflicts int
	if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM runs
WHERE id=? AND status IN ('queued','running') AND credentialed=0
  AND (EXISTS (SELECT 1 FROM vm_executions
               WHERE run_id=runs.id AND (submitted=1 OR execution_kind='kvm-preflight') AND cleaned=0)
       OR EXISTS (SELECT 1 FROM vm_suite_executions
                  WHERE run_id=runs.id AND submitted=1 AND cleaned=0))`, id).Scan(&conflicts); err != nil {
		return fmt.Errorf("check credential transition ownership: %w", err)
	}
	if conflicts != 0 {
		return fmt.Errorf("%w: run has a submitted VM execution pending cleanup", ErrInvalidState)
	}
	_, err = tx.ExecContext(ctx, `
UPDATE runs SET credentialed = 1, updated_at = ?
WHERE id = ? AND status IN ('queued', 'running') AND credentialed = 0`,
		unixNano(s.now()), id)
	if err != nil {
		return fmt.Errorf("set run credentialed: %w", err)
	}
	return tx.Commit()
}

// SetRunConcurrencyGroup records the repository-declared concurrency group an
// active run is admitted under (#658). The scheduler calls it after reading the
// group from the run's own immutable checkout and before resource admission,
// so a run waiting on its group already shows which group it waits on. The
// group is a statement about exact reviewed bytes and therefore immutable: a
// second call with a different group is an invalid state transition, while a
// repeat of the same value, or of the empty "no group" value, is a no-op. A
// run that is no longer active is left untouched.
func (s *Store) SetRunConcurrencyGroup(ctx context.Context, id, group string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("%w: run ID is required", ErrInvalid)
	}
	if group == "" {
		return nil
	}
	if !validConcurrencyGroup(group) {
		return fmt.Errorf("%w: concurrency group %q is not 1 to 63 characters of [a-z0-9-]", ErrInvalid, group)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE runs SET concurrency_group = ?, updated_at = ?
WHERE id = ? AND status IN ('queued', 'running') AND concurrency_group = ''`,
		group, unixNano(s.now()), id)
	if err != nil {
		return fmt.Errorf("set run concurrency group: %w", err)
	}
	if changed, err := result.RowsAffected(); err == nil && changed == 1 {
		return nil
	}
	current, err := s.Run(ctx, id)
	if err != nil {
		return err
	}
	if current.ConcurrencyGroup != "" && current.ConcurrencyGroup != group {
		return fmt.Errorf("%w: run %s already records concurrency group %q", ErrInvalidState, id, current.ConcurrencyGroup)
	}
	return nil
}

// validConcurrencyGroup is the store-side copy of the admission rule the
// schema CHECK also enforces; the store package does not import the pipeline
// document package.
func validConcurrencyGroup(group string) bool {
	if group == "" || len(group) > 63 {
		return false
	}
	for index := 0; index < len(group); index++ {
		character := group[index]
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
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

func negativeMeasurement[T ~int64](value *T) bool { return value != nil && *value < 0 }
func measurementPointer[T ~int64](value sql.NullInt64) *T {
	if !value.Valid {
		return nil
	}
	n := T(value.Int64)
	return &n
}
