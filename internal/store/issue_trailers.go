package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/model"
)

func (s *Store) recordIssueTrailerWork(ctx context.Context, tx *sql.Tx, final model.PublicationFinalization) error {
	p := final.Publication
	if p.Status != model.PublicationDelivered || p.RefKind != model.RefBranch {
		return nil
	}
	var branch string
	if err := tx.QueryRowContext(ctx, `SELECT default_branch FROM repositories WHERE id=?`, p.RepoID).Scan(&branch); err != nil {
		return err
	}
	if p.Ref != branch {
		return nil
	}
	runID := final.Run.ID
	if final.Promotion.ID != "" {
		if final.Promotion.Status != model.PromotionPassed {
			return nil
		}
		if runID == "" {
			runID = final.Promotion.RunID
		}
		if runID == "" {
			err := tx.QueryRowContext(ctx, `SELECT id FROM runs WHERE repo_id=? AND sha=? AND tested_sha=sha AND status='passed' AND ref_kind='branch' ORDER BY queue_sequence DESC LIMIT 1`, p.RepoID, p.ResultSHA).Scan(&runID)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	} else if final.Run.Status != model.RunPassed || (final.Run.Trigger != "push" && final.Run.Trigger != "branch") || final.Run.RefKind != model.RefBranch || final.Run.Ref != branch {
		return nil
	}
	// A fast-forward promotion reuses its source's successful run.
	var status model.RunStatus
	if err := tx.QueryRowContext(ctx, `SELECT status FROM runs WHERE id=?`, runID).Scan(&status); err != nil {
		return err
	}
	if status != model.RunPassed {
		return fmt.Errorf("%w: trailer evidence requires a passed run", ErrInvalidState)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO issue_trailer_publications(publication_id,repo_id,branch,sha,previous_sha,run_id)
 VALUES(?,?,?,?,COALESCE((SELECT sha FROM issue_trailer_publications WHERE repo_id=? AND branch=? ORDER BY sequence DESC LIMIT 1),''),?)`, p.ID, p.RepoID, branch, p.ResultSHA, p.RepoID, branch, runID)
	return err
}

func (s *Store) PendingIssueTrailerWork(ctx context.Context) ([]model.IssueTrailerWork, error) {
	// Interleave each repository's oldest work before taking another row from
	// that repository. An unavailable cache cannot fill the entire retry batch.
	rows, err := s.reader().QueryContext(ctx, `WITH pending AS (
 SELECT *,ROW_NUMBER() OVER (PARTITION BY repo_id ORDER BY sequence) AS repo_position
 FROM issue_trailer_publications WHERE done=0
)
 SELECT publication_id,repo_id,branch,sha,previous_sha,run_id FROM pending
 ORDER BY repo_position,sequence LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var result []model.IssueTrailerWork
	for rows.Next() {
		var w model.IssueTrailerWork
		if err := rows.Scan(&w.PublicationID, &w.RepoID, &w.Branch, &w.SHA, &w.PreviousSHA, &w.RunID); err != nil {
			return nil, err
		}
		result = append(result, w)
	}
	return result, rows.Err()
}

// CompleteIssueTrailerWork applies the evidence, close and receipt atomically.
// Scope and live locks are checked inside that same writer transaction.
func (s *Store) CompleteIssueTrailerWork(ctx context.Context, publicationID string, changes []model.CommitIssueTrailer) error {
	return s.completeIssueTrailerWork(ctx, publicationID, changes, "")
}

// SkipIssueTrailerWork records immutable rejected input and completes the
// receipt atomically. No issue evidence or state is changed.
func (s *Store) SkipIssueTrailerWork(ctx context.Context, publicationID, reason string) error {
	if strings.TrimSpace(reason) == "" || len(reason) > 1024 || strings.ContainsRune(reason, '\x00') {
		return fmt.Errorf("%w: invalid trailer skip reason", ErrInvalid)
	}
	return s.completeIssueTrailerWork(ctx, publicationID, nil, reason)
}

func (s *Store) completeIssueTrailerWork(ctx context.Context, publicationID string, changes []model.CommitIssueTrailer, skipReason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var w model.IssueTrailerWork
	var done bool
	var upstreamID int64
	var repoName string
	var upstream model.Upstream
	err = tx.QueryRowContext(ctx, `SELECT w.publication_id,w.repo_id,w.branch,w.sha,w.previous_sha,w.run_id,w.done,r.upstream_id,r.name,u.name,u.base_url
 FROM issue_trailer_publications w JOIN repositories r ON r.id=w.repo_id JOIN upstreams u ON u.id=r.upstream_id WHERE w.publication_id=?`, publicationID).
		Scan(&w.PublicationID, &w.RepoID, &w.Branch, &w.SHA, &w.PreviousSHA, &w.RunID, &done, &upstreamID, &repoName, &upstream.Name, &upstream.BaseURL)
	if err != nil {
		return translateNotFound("issue trailer work", err)
	}
	if done {
		return nil
	}
	repo := upstream.QualifiedRepo(repoName)
	now := unixNano(s.now().UTC())
	if skipReason != "" {
		details, err := json.Marshal(map[string]string{
			"repo": repo, "sha": w.SHA, "run_id": w.RunID, "branch": w.Branch,
			"publication_id": publicationID, "reason": skipReason,
		})
		if err != nil {
			return err
		}
		if _, err := s.appendAuditAction(ctx, tx, model.AuditActionSpec{
			Actor: "oberth", Action: "issue.trailer.skipped", ResourceType: "publication",
			ResourceID: publicationID, Details: string(details),
		}, now); err != nil {
			return err
		}
	}
	// One line per issue and commit, with close taking precedence over Refs
	// on that commit. Later commits still supply their own reference evidence.
	merged := make([]model.CommitIssueTrailer, 0, len(changes))
	type reference struct {
		issueID int64
		sha     string
	}
	seen := make(map[reference]int)
	for _, change := range changes {
		if change.IssueID <= 0 || !gitoid.Valid(change.SHA) {
			return fmt.Errorf("%w: invalid issue trailer", ErrInvalid)
		}
		key := reference{change.IssueID, change.SHA}
		if index, ok := seen[key]; ok {
			if change.Close && !merged[index].Close {
				merged[index] = change
			}
			continue
		}
		seen[key] = len(merged)
		merged = append(merged, change)
	}
	for _, change := range merged {
		// This is the issue_get scoped-caller rule: repository-backed issues by
		// their repository's upstream; manual issues by scope_upstream_id. Global
		// manual issues have no upstream and cannot match.
		issue, err := scanIssue(tx.QueryRowContext(ctx, `SELECT `+issueColumns+` FROM issues WHERE id=? AND
   COALESCE((SELECT upstream_id FROM repositories WHERE id=issues.repo_id),scope_upstream_id)=?`, change.IssueID, upstreamID))
		if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if issue.State != model.IssueOpen {
			continue
		}
		details := map[string]any{"repo": repo, "sha": change.SHA, "run_id": w.RunID, "branch": w.Branch, "publication_id": publicationID}
		var locked bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM issue_locks WHERE issue_id=? AND expires_at>?)`, issue.ID, now).Scan(&locked); err != nil {
			return err
		}
		if locked {
			if err := s.appendIssueAudit(ctx, tx, "oberth", "issue.trailer.locked", issue.ID, details, now); err != nil {
				return err
			}
			continue
		}
		line := fmt.Sprintf("%s %s run %s: green on %s", repo, change.SHA[:12], w.RunID, w.Branch)
		if _, err := tx.ExecContext(ctx, `UPDATE issues SET body=CASE WHEN body='' THEN ? ELSE body||char(10)||? END, updated_at=? WHERE id=?`, line, line, now, issue.ID); err != nil {
			return err
		}
		details["appended_bytes"] = len(line)
		if err := s.appendIssueAudit(ctx, tx, "oberth", "issue.append", issue.ID, details, now); err != nil {
			return err
		}
		if change.Close {
			if _, err := tx.ExecContext(ctx, `UPDATE issues SET state='closed',closed_at=?,updated_at=? WHERE id=?`, now, now, issue.ID); err != nil {
				return err
			}
			if err := s.appendIssueAudit(ctx, tx, "oberth", "issue.close", issue.ID, details, now); err != nil {
				return err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE issue_trailer_publications SET done=1 WHERE publication_id=?`, strings.TrimSpace(publicationID)); err != nil {
		return err
	}
	return tx.Commit()
}
