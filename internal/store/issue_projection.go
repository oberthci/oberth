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

const (
	ciOriginBranch    = "branch"
	ciOriginPromotion = "promotion"
	ciOutcomePassed   = "passed"
	ciOutcomeFailed   = "failed"
)

type ciIssueWork struct {
	Sequence    int64
	RepoID      int64
	Branch      string
	Origin      string
	RunID       string
	PromotionID string
	Actor       string
}

type ciIssueProjection struct {
	Work       ciIssueWork
	Title      string
	Body       string
	Resolution string
}

func registerRunIssueWork(ctx context.Context, tx *sql.Tx, run model.Run, now int64) error {
	if run.RefKind != model.RefBranch || run.Trigger == "promotion" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
INSERT INTO ci_issue_work(repo_id, branch, origin, run_id, actor, created_at)
VALUES(?, ?, 'branch', ?, ?, ?)`, run.RepoID, run.Ref, run.ID, run.Actor, now)
	if err != nil {
		return fmt.Errorf("register branch issue work: %w", err)
	}
	return nil
}

func registerPromotionIssueWork(ctx context.Context, tx *sql.Tx, promotion model.Promotion, now int64) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO ci_issue_work(repo_id, branch, origin, promotion_id, actor, created_at)
VALUES(?, ?, 'promotion', ?, ?, ?)`, promotion.RepoID, promotion.SourceBranch, promotion.ID, promotion.Actor, now)
	if err != nil {
		return fmt.Errorf("register promotion issue work: %w", err)
	}
	return nil
}

func runIssueWork(ctx context.Context, tx *sql.Tx, runID string) (ciIssueWork, error) {
	return scanCIIssueWork(tx.QueryRowContext(ctx, `
SELECT sequence, repo_id, branch, origin, run_id, promotion_id, actor
FROM ci_issue_work WHERE run_id = ?`, runID))
}

func promotionIssueWork(ctx context.Context, tx *sql.Tx, promotionID string) (ciIssueWork, error) {
	return scanCIIssueWork(tx.QueryRowContext(ctx, `
SELECT sequence, repo_id, branch, origin, run_id, promotion_id, actor
FROM ci_issue_work WHERE promotion_id = ?`, promotionID))
}

func scanCIIssueWork(row rowScanner) (ciIssueWork, error) {
	var value ciIssueWork
	var runID, promotionID sql.NullString
	if err := row.Scan(&value.Sequence, &value.RepoID, &value.Branch, &value.Origin,
		&runID, &promotionID, &value.Actor); err != nil {
		return ciIssueWork{}, err
	}
	value.RunID, value.PromotionID = runID.String, promotionID.String
	return value, nil
}

func projectRunIssue(ctx context.Context, tx *sql.Tx, run model.Run, failureTail string, now int64) error {
	work, err := runIssueWork(ctx, tx, run.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load run issue work: %w", err)
	}
	outcome := ciOutcomeFailed
	if run.Status == model.RunPassed {
		outcome = ciOutcomePassed
	}
	phase := strings.TrimSpace(run.FailedBurn)
	if phase == "" {
		phase = strings.TrimSpace(run.Phase)
	}
	if phase == "" {
		phase = "run"
	}
	title := fmt.Sprintf("CI red: %s — %s failed", work.Branch, phase)
	if run.Status == model.RunInterrupted {
		title = fmt.Sprintf("CI red: %s — interrupted", work.Branch)
	}
	projection := ciIssueProjection{
		Work:       work,
		Title:      title,
		Body:       runIssueBody(run, failureTail),
		Resolution: fmt.Sprintf("resolved by %s (run %s)", run.TestedSHA, run.ID),
	}
	return applyCIIssueProjection(ctx, tx, projection, outcome, now)
}

func runIssueBody(run model.Run, failureTail string) string {
	sha := strings.TrimSpace(run.TestedSHA)
	if sha == "" {
		sha = strings.TrimSpace(run.SHA)
	}
	header := fmt.Sprintf("run %s, sha %s", run.ID, sha)
	if burn, step := strings.TrimSpace(run.FailedBurn), strings.TrimSpace(run.FailedStep); burn != "" || step != "" {
		header += fmt.Sprintf("\nfailed: %s / %s", burn, step)
	}
	sections := []string{header}
	if runError := strings.TrimSpace(run.Error); runError != "" {
		sections = append(sections, runError)
	}
	if tail := strings.TrimSpace(failureTail); tail != "" {
		sections = append(sections, tail)
	}
	if step := strings.TrimSpace(run.FailedStep); sha != "" && step != "" {
		sections = append(sections, fmt.Sprintf("full step log: logs %s %s", sha, step))
	}
	return strings.Join(sections, "\n\n")
}

func projectPromotionIssue(ctx context.Context, tx *sql.Tx, promotion model.Promotion, now int64) error {
	work, err := promotionIssueWork(ctx, tx, promotion.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load promotion issue work: %w", err)
	}
	outcome := ciOutcomeFailed
	if promotion.Status == model.PromotionPassed {
		outcome = ciOutcomePassed
	}
	projection := ciIssueProjection{
		Work:  work,
		Title: fmt.Sprintf("Promotion red: %s → %s failed", promotion.SourceBranch, promotion.TargetRef),
		Body: fmt.Sprintf(
			"promotion %s, source %s, target %s, result %s\n\n%s",
			promotion.ID, promotion.SourceSHA, promotion.TargetRef, promotion.ResultSHA, strings.TrimSpace(promotion.Error),
		),
		Resolution: fmt.Sprintf("resolved by promotion %s", promotion.ID),
	}
	return applyCIIssueProjection(ctx, tx, projection, outcome, now)
}

func applyCIIssueProjection(ctx context.Context, tx *sql.Tx, projection ciIssueProjection, outcome string, now int64) error {
	result, err := tx.ExecContext(ctx, `
INSERT INTO ci_issue_projections(
    repo_id, branch, origin, work_sequence, work_id, outcome, title, body, actor, updated_at
)
VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(repo_id, branch, origin) DO UPDATE SET
    work_sequence = excluded.work_sequence,
    work_id = excluded.work_id,
    outcome = excluded.outcome,
    title = excluded.title,
    body = excluded.body,
    actor = excluded.actor,
    updated_at = excluded.updated_at
WHERE ci_issue_projections.work_sequence < excluded.work_sequence`,
		projection.Work.RepoID, projection.Work.Branch, projection.Work.Origin,
		projection.Work.Sequence, ciIssueWorkID(projection.Work), outcome,
		projection.Title, projection.Body, projection.Work.Actor, now)
	if err != nil {
		return fmt.Errorf("advance CI issue projection: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read CI issue projection result: %w", err)
	}
	if changed == 0 {
		return nil
	}

	desired, desiredErr := scanDesiredCIIssue(tx.QueryRowContext(ctx, `
SELECT work_sequence, work_id, origin, title, body, actor
FROM ci_issue_projections
WHERE repo_id = ? AND branch = ? AND outcome = 'failed'
ORDER BY work_sequence DESC LIMIT 1`, projection.Work.RepoID, projection.Work.Branch))
	open, openErr := scanIssue(tx.QueryRowContext(ctx, `
SELECT `+issueColumns+` FROM issues
WHERE repo_id = ? AND kind = 'ci' AND branch = ? AND state = 'open'`,
		projection.Work.RepoID, projection.Work.Branch))
	if openErr != nil && !errors.Is(openErr, sql.ErrNoRows) {
		return fmt.Errorf("load projected CI issue: %w", openErr)
	}
	if errors.Is(desiredErr, sql.ErrNoRows) {
		if errors.Is(openErr, sql.ErrNoRows) {
			return nil
		}
		resolution := projection.Resolution
		if strings.TrimSpace(resolution) == "" {
			resolution = fmt.Sprintf("resolved by %s %s", projection.Work.Origin, ciIssueWorkID(projection.Work))
		}
		closed, err := scanIssue(tx.QueryRowContext(ctx, `
UPDATE issues SET state = 'closed', closed_at = ?, updated_at = ?,
    body = CASE WHEN body = '' THEN ? ELSE body || char(10) || char(10) || ? END
WHERE id = ? AND state = 'open'
RETURNING `+issueColumns, now, now, resolution, resolution, open.ID))
		if err != nil {
			return fmt.Errorf("close projected CI issue: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM issue_locks WHERE issue_id = ?`, closed.ID); err != nil {
			return fmt.Errorf("release resolved CI issue lock: %w", err)
		}
		if err := appendIssueAudit(ctx, tx, projection.Work.Actor, "issue.ci.close", closed.ID,
			map[string]any{"branch": projection.Work.Branch, "resolution": resolution}, now); err != nil {
			return err
		}
		return nil
	}
	if desiredErr != nil {
		return fmt.Errorf("load desired CI issue projection: %w", desiredErr)
	}

	var issue model.Issue
	if errors.Is(openErr, sql.ErrNoRows) {
		issue, err = scanIssue(tx.QueryRowContext(ctx, `
INSERT INTO issues(
    repo_id, kind, branch, title, body, state, occurrences,
    ci_origin, ci_work_sequence, ci_work_id, created_at, updated_at
)
VALUES(?, 'ci', ?, ?, ?, 'open', 1, ?, ?, ?, ?, ?)
RETURNING `+issueColumns,
			projection.Work.RepoID, projection.Work.Branch, desired.Title, desired.Body,
			desired.Origin, desired.Sequence, desired.WorkID, now, now))
		if err != nil {
			return fmt.Errorf("open projected CI issue: %w", err)
		}
		if err := appendIssueAudit(ctx, tx, desired.Actor, "issue.ci.open", issue.ID,
			map[string]any{"branch": projection.Work.Branch, "origin": desired.Origin, "work_id": desired.WorkID}, now); err != nil {
			return err
		}
	} else {
		if open.CIOrigin == desired.Origin && open.CIWorkSequence == desired.Sequence && open.CIWorkID == desired.WorkID {
			return nil
		}
		increment := int64(0)
		if outcome == ciOutcomeFailed && desired.Sequence == projection.Work.Sequence && desired.Origin == projection.Work.Origin {
			increment = 1
		}
		issue, err = scanIssue(tx.QueryRowContext(ctx, `
UPDATE issues SET title = ?, body = ?, occurrences = occurrences + ?,
    ci_origin = ?, ci_work_sequence = ?, ci_work_id = ?, updated_at = ?
WHERE id = ? AND state = 'open'
RETURNING `+issueColumns,
			desired.Title, desired.Body, increment, desired.Origin, desired.Sequence, desired.WorkID, now, open.ID))
		if err != nil {
			return fmt.Errorf("update projected CI issue: %w", err)
		}
		if err := appendIssueAudit(ctx, tx, desired.Actor, "issue.ci.project", issue.ID,
			map[string]any{"branch": projection.Work.Branch, "origin": desired.Origin, "work_id": desired.WorkID}, now); err != nil {
			return err
		}
	}
	return lockProjectedCIIssue(ctx, tx, issue.ID, desired.Actor, now)
}

func ciIssueWorkID(work ciIssueWork) string {
	if work.PromotionID != "" {
		return work.PromotionID
	}
	return work.RunID
}

type desiredCIIssue struct {
	Sequence int64
	WorkID   string
	Origin   string
	Title    string
	Body     string
	Actor    string
}

func scanDesiredCIIssue(row rowScanner) (desiredCIIssue, error) {
	var value desiredCIIssue
	err := row.Scan(&value.Sequence, &value.WorkID, &value.Origin, &value.Title, &value.Body, &value.Actor)
	return value, err
}

func lockProjectedCIIssue(ctx context.Context, tx *sql.Tx, issueID int64, actor string, now int64) error {
	expires := fromUnixNano(now).Add(IssueLockTTL)
	var owner string
	err := tx.QueryRowContext(ctx, `
INSERT INTO issue_locks(issue_id, owner, acquired_at, expires_at) VALUES(?, ?, ?, ?)
ON CONFLICT(issue_id) DO UPDATE SET
    owner = excluded.owner,
    acquired_at = excluded.acquired_at,
    expires_at = excluded.expires_at
WHERE issue_locks.expires_at <= ? OR issue_locks.owner = excluded.owner
RETURNING owner`, issueID, actor, now, unixNano(expires), now).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lock projected CI issue: %w", err)
	}
	return appendIssueAudit(ctx, tx, actor, "issue.lock.acquire", issueID,
		map[string]any{"expires_at": expires.Format(time.RFC3339Nano), "source": "ci_projection"}, now)
}
