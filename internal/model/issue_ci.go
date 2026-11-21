package model

import (
	"strings"
	"time"
)

// CIIssueOutcome supplies admitted native selectors and a recorded run outcome.
// Failure detail stays in the run; the issue carries a bounded summary.
type CIIssueOutcome struct {
	Repo, Ref, RunID, Body string
	Passed                 bool
	At                     time.Time
}

// CITextBytes bounds native CI text independently of export edit headroom.
const CITextBytes = 4096

// WithCIOutcome preserves human ownership and bounded titles on native CI issues.
func (v Issue) WithCIOutcome(p CIIssueOutcome) Issue {
	status := v.WorkStatus
	if !status.Valid() {
		status = IssueTodo
	}
	created := v.CreatedAt
	if created.IsZero() {
		created = p.At
	}
	body := p.Body
	if p.Passed {
		body = v.Body
	}
	if len(body) > CITextBytes {
		runID := p.RunID
		if p.Passed {
			runID = v.CIWorkID
		}
		body = "Read run_get " + runID + " for the complete failure details."
	}
	title := v.Title
	if title == "" || len(title) > CITextBytes {
		title = "CI failed"
	}
	out := Issue{ID: v.ID, Revision: v.Revision + 1, Kind: IssueCI, Title: title, Lock: v.Lock,
		Body: body, State: IssueOpen, WorkStatus: status, Repo: p.Repo, Branch: p.Ref,
		Upstream: strings.SplitN(p.Repo, "/", 2)[0], Occurrences: v.Occurrences,
		CIOrigin: "ci", CIWorkID: p.RunID, CreatedAt: created, UpdatedAt: p.At}
	if p.Passed {
		out.State = IssueClosed
		out.ClosedAt = &p.At
	} else {
		out.Occurrences++
	}
	return out
}
