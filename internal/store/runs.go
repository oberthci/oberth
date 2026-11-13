package store

import (
	"context"
	"database/sql"
	"encoding/json"
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
schedule_entry, issue_trailers, source_bindings, failure_cause, assist_requested, assisted, assist_record`

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

func (s *Store) enqueueRunTx(ctx context.Context, tx *sql.Tx, spec model.RunSpec, id string, now int64) (model.EnqueueRunResult, error) {
	if err := requireRepositoryActive(ctx, tx, spec.RepoID); err != nil {
		return model.EnqueueRunResult{}, err
	}

	row := tx.QueryRowContext(ctx, `
INSERT INTO runs(
    id, repo_id, ref_kind, ref, sha, actor, release, credentialed, trigger, phase,
    tested_sha, base_sha, schedule_entry, status, reason, assist_requested, queued_at, created_at, updated_at
)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?, 'queued', ?, ?, ?, ?, ?)
RETURNING `+runColumns,
		id, spec.RepoID, spec.RefKind, spec.Ref, spec.SHA, spec.Actor, spec.Release,
		spec.Credentialed, spec.Trigger, spec.TestedSHA, spec.BaseSHA, spec.ScheduleEntry, spec.Reason, spec.Assist, now, now, now)
	value, err := scanRun(row)
	if err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("insert run: %w", err)
	}
	var cancellations []model.RunCancellation
	var supersededCount int64
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
WHERE id != ? AND repo_id = ? AND ref_kind = 'branch' AND ref = ? AND schedule_entry = ?
  AND trigger != 'promotion'
  AND NOT EXISTS (
      SELECT 1 FROM publications
      WHERE publications.run_id = runs.id AND publications.status = 'pending'
  )
  AND status IN ('queued', 'running') AND (status = 'running' OR job_name != '')
 ORDER BY queue_sequence`, id, spec.RepoID, spec.Ref, spec.ScheduleEntry)
		if err != nil {
			return model.EnqueueRunResult{}, fmt.Errorf("find running superseded runs: %w", err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var cancellation model.RunCancellation
			var phase string
			if err := rows.Scan(&cancellation.RunID, &cancellation.WorkflowName, &phase); err != nil {
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

	admissionSequence := value.QueueSequence
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET admission_sequence = ? WHERE id = ?`, admissionSequence, id); err != nil {
		return model.EnqueueRunResult{}, fmt.Errorf("set run admission sequence: %w", err)
	}
	value.AdmissionSequence = admissionSequence
	if err := registerRunIssueWork(ctx, tx, value, now); err != nil {
		return model.EnqueueRunResult{}, err
	}
	if spec.RefKind == model.RefBranch && spec.Trigger != "promotion" {
		updated, err := tx.ExecContext(ctx, `
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
			"superseded by newer run", id, now, now, id, spec.RepoID, spec.Ref, spec.ScheduleEntry)
		if err != nil {
			return model.EnqueueRunResult{}, fmt.Errorf("supersede earlier runs: %w", err)
		}
		supersededCount, err = updated.RowsAffected()
		if err != nil {
			return model.EnqueueRunResult{}, fmt.Errorf("count superseded runs: %w", err)
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
    created_at = excluded.created_at,
    completed_at = NULL`, cancellation.RunID, cancellation.WorkflowName, cancellation.SupersededBy, cancellation.Reason, now); err != nil {
				return model.EnqueueRunResult{}, fmt.Errorf("record run cancellation: %w", err)
			}
		}
	}
	return model.EnqueueRunResult{Run: value, Cancellations: cancellations, SupersededCount: supersededCount}, nil
}

func (s *Store) ClaimNextRun(ctx context.Context) (model.Run, error) {
	return s.claimNextRun(ctx, "")
}

func (s *Store) claimNextRun(ctx context.Context, id string) (model.Run, error) {
	// An explicit transaction ensures the candidate is known before commit.
	// On a lost commit acknowledgement the scheduler can resolve that exact
	// run instead of claiming a second run and abandoning the first.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Run{}, err
	}
	defer func() { _ = tx.Rollback() }()
	now := unixNano(s.now())
	value, err := scanRun(tx.QueryRowContext(ctx, `
UPDATE runs
SET status = 'running', phase = 'waiting', updated_at = ?
WHERE queue_sequence = (
    SELECT queue_sequence FROM runs WHERE (?='' OR id=?) AND `+claimableRunSQL+`
    ORDER BY `+submissionOrderSQL()+` LIMIT 1
)
RETURNING `+runColumns, now, id, id))
	if err != nil {
		return model.Run{}, translateNotFound("queued run", err)
	}
	if err := tx.Commit(); err != nil {
		return value, fmt.Errorf("commit run claim: %w", err)
	}
	return value, nil
}

// MarkRunSubmissionStarted records when Workflow submission begins. Native
// execution observation determines whether it displays queued or running.
// A cancellation that won before this transition cannot acquire a start time.
func (s *Store) MarkRunSubmissionStarted(ctx context.Context, id string) (model.Run, error) {
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
		return model.Run{}, fmt.Errorf("%w: run is not awaiting submission", ErrInvalidState)
	}
	if err != nil {
		return model.Run{}, fmt.Errorf("mark run submission started: %w", err)
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
UPDATE runs SET error=CASE WHEN phase='infrastructure-pending' THEN '' ELSE error END, phase = 'running', started_at = COALESCE(started_at, ?), updated_at = ?
WHERE id = ? AND status = 'running' AND phase IN ('waiting','infrastructure-pending') AND job_name != ''
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
		return s.readRunContext(ctx, id)
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
		return s.readRunContext(ctx, matches[0])
	default:
		return model.Run{}, fmt.Errorf("%w: run ID prefix %q matches: %s; use the full run ID", ErrAmbiguous, id, strings.Join(matches, ", "))
	}
}

func (s *Store) latestRunForRef(ctx context.Context, repoID int64, ref string) (model.Run, error) {
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
SELECT 1 FROM runs WHERE repo_id = ? AND tested_sha = ? AND status = 'passed' AND assisted = 0
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
	return s.latestRunForRef(ctx, repoID, ref)
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

func (s *Store) SetRunWorkflowName(ctx context.Context, id, workflowName string) (model.Run, error) {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(workflowName) == "" {
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
RETURNING `+runColumns, workflowName, unixNano(s.now()), id))
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
WHERE run_cancellations.completed_at IS NULL`, value.ID, workflowName, value.SupersededBy, unixNano(s.now())); err != nil {
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

func (s *Store) CompleteRunCancellation(ctx context.Context, runID, workflowName string) error {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(workflowName) == "" {
		return fmt.Errorf("%w: run ID and job name are required", ErrInvalid)
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE run_cancellations SET completed_at = ?
WHERE run_id = ? AND job_name = ? AND completed_at IS NULL`, unixNano(s.now()), runID, workflowName)
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
SELECT completed_at FROM run_cancellations WHERE run_id = ? AND job_name = ?`, runID, workflowName).Scan(&completed)
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
// a background task is superseded or cancelled after claim but exits before publishing
// its deterministic Job name. Callers invoke this only after that exact background task has
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
SET status = ?, phase = ?, reason = CASE WHEN ? THEN ? ELSE reason END,
    tested_sha = CASE WHEN ? = '' THEN tested_sha ELSE ? END,
    base_sha = CASE WHEN ? = '' THEN base_sha ELSE ? END,
    failed_burn = ?, failed_step = ?, error = ?, failure_cause = ?, finished_at = ?, updated_at = ?
WHERE id = ? AND status IN ('queued', 'running')
RETURNING `+runColumns,
		result.Status, phase, result.Status == model.RunInterrupted && phase == model.PhaseCancelled && strings.TrimSpace(result.Error) != "", result.Error,
		result.TestedSHA, result.TestedSHA,
		result.BaseSHA, result.BaseSHA,
		result.FailedStage, result.FailedStep, result.Error, result.FailureCause, now, now, id))
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
		_, promotionErr = finishPromotionTx(ctx, tx, promotion, promotionStatus, value.ID, value.Error, now)
		if promotionErr != nil {
			return model.Run{}, promotionErr
		}
	} else {
		if err := s.projectRunIssue(ctx, tx, value, result.FailureTail, now); err != nil {
			return model.Run{}, err
		}
	}
	if err := s.recordTerminalObservationTx(ctx, tx, value, result.TerminalObservation, now); err != nil {
		return model.Run{}, err
	}
	return value, nil
}

func (s *Store) ListRecentRuns(ctx context.Context, filter model.RunListFilter) ([]model.Run, error) {
	if filter.LatestPerRef {
		return s.listLatestRefRuns(ctx, filter)
	}
	if !filter.Terminal {
		return s.listFlatRuns(ctx, filter)
	}
	statusSQL, statusArgs, err := runStatusRestriction(filter.Status)
	if err != nil {
		return nil, err
	}
	restriction, restrictionArgs := runRestriction(filter)
	restriction += statusSQL
	restrictionArgs = append(restrictionArgs, statusArgs...)
	args := append([]any{filter.RepoID, filter.RepoID, filter.Ref, filter.Ref, filter.Before, filter.Before, filter.Terminal}, restrictionArgs...)
	rows, err := s.reader().QueryContext(ctx, `
SELECT `+runReadColumns+` FROM runs
WHERE (? = 0 OR repo_id = ?) AND (? = '' OR ref = ?) AND (? <= 0 OR queue_sequence < ?)
  AND (? = 0 OR status NOT IN ('queued', 'running'))`+restriction+supersededRunRestriction(filter)+`
ORDER BY queue_sequence DESC LIMIT ?`, append(args, pageLimit(filter.Limit))...) // #nosec G202 -- the restriction is a fixed column and ? placeholders from repoRestriction.
	if err != nil {
		return nil, fmt.Errorf("list recent runs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	runs := make([]model.Run, 0)
	for rows.Next() {
		value, err := scanReadRun(rows)
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

// listFlatRuns reads one list in three groups, top to bottom: the bounded
// queue in scheduler claim order (effective priority, then FIFO); running runs, most recently
// started first; then terminal runs, most recently finished first. The first
// page carries every queued and running run; only the terminal rows count
// against the page limit, so a limit never drops a queued or running run. Older
// pages use a fixed (end time, ID) cursor over terminal runs: a run finishing
// between page loads cannot move onto an older page and appear twice. All
// groups and the cursor are read from one snapshot.
func (s *Store) listFlatRuns(ctx context.Context, filter model.RunListFilter) ([]model.Run, error) {
	tx, err := s.reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin flat run list snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	statusSQL, statusArgs, err := runStatusRestriction(filter.Status)
	if err != nil {
		return nil, err
	}
	restriction, restrictionArgs := runRestriction(filter)
	restriction += statusSQL
	restrictionArgs = append(restrictionArgs, statusArgs...)
	scopeArgs := append([]any{filter.RepoID, filter.RepoID, filter.Ref, filter.Ref}, restrictionArgs...)
	collect := func(what string, rows *sql.Rows, err error) ([]model.Run, error) {
		if err != nil {
			return nil, fmt.Errorf("list %s runs: %w", what, err)
		}
		defer func() { _ = rows.Close() }()
		runs := make([]model.Run, 0)
		for rows.Next() {
			value, scanErr := scanReadRun(rows)
			if scanErr != nil {
				return nil, fmt.Errorf("scan %s run: %w", what, scanErr)
			}
			runs = append(runs, value)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("list %s runs: %w", what, err)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("close %s runs: %w", what, err)
		}
		return runs, nil
	}
	runs := make([]model.Run, 0)
	if filter.Before == 0 && filter.BeforeRun == "" {
		priorityOrder := submissionOrderSQL()
		// Keep the queue in the same effective-priority and FIFO order as ClaimNextRun.
		rows, err := tx.QueryContext(ctx, `
SELECT `+runReadColumns+` FROM runs
WHERE (`+queuedRunSQL+`) AND (? = 0 OR repo_id = ?) AND (? = '' OR ref = ?)`+restriction+supersededRunRestriction(filter)+`
ORDER BY `+priorityOrder+` LIMIT ?`, append(scopeArgs, MaxActiveRuns)...) // #nosec G202 -- fixed columns and parameterized restriction.
		queued, err := collect("queued", rows, err)
		if err != nil {
			return nil, err
		}
		runs = append(runs, queued...)
		rows, err = tx.QueryContext(ctx, `
SELECT `+runReadColumns+` FROM runs
WHERE status = 'running' AND started_at IS NOT NULL
  AND (? = 0 OR repo_id = ?) AND (? = '' OR ref = ?)`+restriction+supersededRunRestriction(filter)+`
ORDER BY started_at DESC, id DESC LIMIT ?`, append(scopeArgs, MaxActiveRuns)...) // #nosec G202 -- fixed columns and parameterized restriction.
		running, err := collect("running", rows, err)
		if err != nil {
			return nil, err
		}
		runs = append(runs, running...)
	}
	var beforeTime int64
	var beforeID string
	if filter.BeforeRun != "" || filter.Before > 0 {
		cursor, err := s.runListCursor(ctx, tx, filter)
		if err != nil {
			return nil, err
		}
		beforeID = cursor.ID
		beforeTime = unixNano(runEndTime(cursor))
	}
	args := append([]any{beforeID, beforeTime, beforeTime, beforeID}, scopeArgs...)
	rows, err := tx.QueryContext(ctx, `
SELECT `+runReadColumns+` FROM runs
WHERE status NOT IN ('queued', 'running')
  AND (? = '' OR `+runEndTimeSQL+` < ? OR (`+runEndTimeSQL+` = ? AND id < ?))
  AND (? = 0 OR repo_id = ?) AND (? = '' OR ref = ?)`+restriction+supersededRunRestriction(filter)+`
ORDER BY `+runEndTimeSQL+` DESC, id DESC
LIMIT ?`, append(args, pageLimit(filter.Limit))...) // #nosec G202 -- fixed columns and parameterized restriction.
	terminal, err := collect("terminal", rows, err)
	if err != nil {
		return nil, err
	}
	runs = append(runs, terminal...)
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit flat run list snapshot: %w", err)
	}
	return runs, nil
}

// queuedRunSQL selects the runs still waiting for admission: durable queued
// runs and claimed runs whose Workflow has not started.
const queuedRunSQL = `(status = 'queued' OR (status = 'running' AND started_at IS NULL))`

// runEndTimeSQL orders terminal runs. Every terminal transition records
// finished_at; the fallbacks keep the order total for any older row.
const runEndTimeSQL = `COALESCE(finished_at, started_at, queued_at)`

func runEndTime(run model.Run) time.Time {
	if run.FinishedAt != nil {
		return *run.FinishedAt
	}
	if run.StartedAt != nil {
		return *run.StartedAt
	}
	return run.QueuedAt
}

// runListCursor resolves the page cursor, a run ID or a run sequence, to the
// run itself; a pruned cursor asks the caller to restart from the first page.
func (s *Store) runListCursor(ctx context.Context, tx *sql.Tx, filter model.RunListFilter) (model.Run, error) {
	var cursor model.Run
	var err error
	if filter.BeforeRun != "" {
		cursor, err = scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id = ?`, filter.BeforeRun))
	} else {
		cursor, err = scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE queue_sequence = ?`, filter.Before))
	}
	if errors.Is(err, sql.ErrNoRows) {
		return model.Run{}, ErrRunCursorNotFound
	}
	if err != nil {
		return model.Run{}, fmt.Errorf("read run list cursor: %w", err)
	}
	return cursor, nil
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
	// Report only a fixed field name, never result values or diagnostics.
	// Keep the store's terminal, identity and bounds checks at this boundary.
	invalidField := ""
	switch {
	case strings.TrimSpace(result.RunID) == "":
		invalidField = "run_id"
	case strings.TrimSpace(result.Stage) == "":
		invalidField = "stage"
	case strings.TrimSpace(result.Step) == "":
		invalidField = "step"
	case result.Ordinal < 0:
		invalidField = "ordinal"
	case !result.Status.Terminal():
		invalidField = "status"
	case result.LogStart < 0:
		invalidField = "log_start"
	case result.LogEnd < result.LogStart:
		invalidField = "log_end"

	}
	if invalidField != "" {
		return model.StepResult{}, fmt.Errorf("%w: step result invalid field %s", ErrInvalid, invalidField)
	}

	recorded := unixNano(s.now())
	row := s.db.QueryRowContext(ctx, `
INSERT INTO step_results(
    run_id, burn, step, ordinal, status, exit_code, log_start, log_end,
    started_at, finished_at, recorded_at, termination_json
)
SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
WHERE EXISTS (SELECT 1 FROM runs WHERE id = ? AND status IN ('queued','running'))
ON CONFLICT(run_id, burn, step) DO UPDATE SET
    ordinal = excluded.ordinal,
    status = excluded.status,
    exit_code = excluded.exit_code,
    log_start = excluded.log_start,
    log_end = excluded.log_end,
    started_at = excluded.started_at,
    finished_at = excluded.finished_at,
    recorded_at = excluded.recorded_at,
    termination_json = excluded.termination_json
RETURNING run_id, burn, step, ordinal, status, exit_code, log_start, log_end,
		  started_at, finished_at, recorded_at, termination_json`,
		result.RunID, result.Stage, result.Step, result.Ordinal, result.Status,
		result.ExitCode, result.LogStart, result.LogEnd,
		nullableUnixNano(result.StartedAt), nullableUnixNano(result.FinishedAt), recorded, stepDiagnosticJSON(result.Termination, result.RunID), result.RunID)
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
	rows, err := database.QueryContext(ctx, `
SELECT run_id, burn, step, ordinal, status, exit_code, log_start, log_end,
	   started_at, finished_at, recorded_at, termination_json
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
	var issueTrailers, sourceBindings, assistRecord string
	var release, credentialed, assistRequested, assisted int64
	var queued, created, updated int64
	var started, finished sql.NullInt64
	dest := []any{
		&value.QueueSequence, &value.AdmissionSequence, &value.ID, &value.RepoID, &value.RefKind, &value.Ref,
		&value.SHA, &value.Actor, &release, &credentialed, &value.Trigger, &value.Phase, &value.WorkflowName,
		&value.TestedSHA, &value.BaseSHA, &value.FailedStage, &value.FailedStep, &value.Error,
		&value.Status, &value.Reason, &value.SupersededBy, &queued, &started, &finished,
		&created, &updated, &value.ScheduleEntry, &issueTrailers, &sourceBindings, &value.FailureCause, &assistRequested, &assisted, &assistRecord,
	}
	if err := scanRowNotFound(row, "Run", append(dest, extra...)...); err != nil {
		return model.Run{}, err
	}
	if issueTrailers != "" {
		value.IssueTrailersRecorded = true
		if len(issueTrailers) > 64<<10 {
			return model.Run{}, fmt.Errorf("%w: run issue trailer metadata exceeds limit", ErrInvalidState)
		}
		if err := json.Unmarshal([]byte(issueTrailers), &value.IssueTrailers); err != nil {
			return model.Run{}, fmt.Errorf("decode run issue trailers: %w", err)
		}
	}
	if sourceBindings != "" {
		if len(sourceBindings) > 8192 {
			return model.Run{}, fmt.Errorf("%w: source bindings exceed limit", ErrInvalidState)
		}
		if err := json.Unmarshal([]byte(sourceBindings), &value.SourceBindings); err != nil {
			return model.Run{}, err
		}
	}
	if assistRecord != "" {
		if len(assistRecord) > model.MaximumRunAssistRecordBytes {
			return model.Run{}, fmt.Errorf("%w: run assist record exceeds limit", ErrInvalidState)
		}
		var record model.RunAssistRecord
		if err := json.Unmarshal([]byte(assistRecord), &record); err != nil {
			return model.Run{}, fmt.Errorf("decode run assist record: %w", err)
		}
		value.Assist = &record
	}
	value.AssistRequested = assistRequested != 0
	value.Assisted = assisted != 0

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
	var recorded int64
	var diagnostic sql.NullString
	if err := scanRowNotFound(row, "StepResult", &value.RunID, &value.Stage, &value.Step, &value.Ordinal,
		&value.Status, &value.ExitCode, &value.LogStart, &value.LogEnd,
		&started, &finished, &recorded, &diagnostic); err != nil {
		return model.StepResult{}, err
	}
	value.Termination = readStepDiagnostic(diagnostic, value.RunID)
	value.StartedAt, value.FinishedAt = nullableTime(started), nullableTime(finished)
	value.RecordedAt = fromUnixNano(recorded)
	return value, nil
}

func scanRunCancellation(row rowScanner) (model.RunCancellation, error) {
	var value model.RunCancellation
	var created int64
	var completed sql.NullInt64
	if err := scanRowNotFound(row, "RunCancellation", &value.RunID, &value.WorkflowName, &value.SupersededBy, &value.Reason, &created, &completed); err != nil {
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin credential transition: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `
UPDATE runs SET credentialed = 1, updated_at = ?
WHERE id = ? AND status IN ('queued', 'running') AND credentialed = 0`,
		unixNano(s.now()), id)
	if err != nil {
		return fmt.Errorf("set run credentialed: %w", err)
	}
	return tx.Commit()
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
