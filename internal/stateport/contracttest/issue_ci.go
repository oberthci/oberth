package contracttest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type ciIssuePort interface {
	Issues(context.Context, []string, model.IssueFilter) ([]model.Issue, error)
	UpdateIssue(context.Context, stateport.Action, model.IssuePatch) (model.Issue, error)
	ChangeIssueLock(context.Context, stateport.Action, stateport.IssueLockRequest) error
}

func checkCIIssueBudget(t *testing.T, f RunFixture) {
	p := f.Port.(ciIssuePort)
	a := stateport.Action{Actor: "operator"}
	red := acceptOneRun(t, f, RunCase{ID: "ci-budget-red"})
	failure := &stepmodel.FailureRecord{Class: stepmodel.FailureAuthor, Step: strings.Repeat("<", 30000), Retry: stepmodel.RetryHint{Action: stepmodel.RetryNone}}
	mustRun(t, f.Port.Finish(t.Context(), a, stateport.RunUpdate{ID: red.ID, Revision: red.Revision, Status: stepmodel.RunFailed, Failure: failure, ProjectCI: true}))
	issue := readNativeIssue(t, p, red.Repo)
	mustRun(t, p.ChangeIssueLock(t.Context(), a, stateport.IssueLockRequest{ID: issue.ID, Owner: a.Actor}))
	issue = readNativeIssue(t, p, red.Repo)
	issue.Revision++
	issue.WorkStatus = model.IssueReview
	raw, _ := json.Marshal(issue)
	_, err := p.UpdateIssue(t.Context(), a, model.IssuePatch{ID: issue.ID, Title: new(strings.Repeat("x", model.IssueBytes-len(raw)+len(issue.Title))), WorkStatus: new(model.IssueReview)})
	mustRun(t, err)
	green := acceptOneRun(t, f, RunCase{ID: "ci-budget-green"})
	mustRun(t, f.Port.Finish(t.Context(), a, stateport.RunUpdate{ID: green.ID, Revision: green.Revision, Status: stepmodel.RunPassed, ProjectCI: true}))
	got := readNativeIssue(t, p, green.Repo)
	if got.State != model.IssueClosed || got.CIWorkID != green.ID || got.WorkStatus != model.IssueReview || !strings.Contains(got.Body, "run_get "+red.ID) {
		t.Fatal("native outcome lost its bounded run summary", got.ID)
	}
	stored, err := f.Port.Run(t.Context(), red.ID)
	mustRun(t, err)
	if stored.Failure == nil || stored.Failure.Step != failure.Step {
		t.Fatal("native failure detail was discarded")
	}
}

func readNativeIssue(t *testing.T, p ciIssuePort, repo string) model.Issue {
	t.Helper()
	rows, err := p.Issues(t.Context(), nil, model.IssueFilter{Repo: repo, Kind: model.IssueCI, State: "all", Limit: 10})
	mustRun(t, err)
	if len(rows) != 1 {
		t.Fatal("native issue missing")
	}
	raw, err := json.Marshal(rows[0])
	if err != nil || len(raw) > model.IssueBytes {
		t.Fatal("native projection is unreadable", len(raw), err)
	}
	return rows[0]
}

func checkCIRedKeepsHumanInputs(t *testing.T, f RunFixture) {
	p := f.Port.(ciIssuePort)
	a := stateport.Action{Actor: "operator"}
	red := acceptOneRun(t, f, RunCase{ID: "ci-human-red"})
	mustRun(t, f.Port.Finish(t.Context(), a, stateport.RunUpdate{ID: red.ID, Revision: red.Revision, Status: stepmodel.RunFailed, ProjectCI: true}))
	issue := readNativeIssue(t, p, red.Repo)
	title := "flaky login test, agent investigating"
	_, err := p.UpdateIssue(t.Context(), a, model.IssuePatch{ID: issue.ID, Title: &title})
	mustRun(t, err)
	mustRun(t, p.ChangeIssueLock(t.Context(), a, stateport.IssueLockRequest{ID: issue.ID, Owner: a.Actor}))
	before := readNativeIssue(t, p, red.Repo)
	next := acceptOneRun(t, f, RunCase{ID: "ci-human-red-again"})
	mustRun(t, f.Port.Finish(t.Context(), a, stateport.RunUpdate{ID: next.ID, Revision: next.Revision, Status: stepmodel.RunFailed, ProjectCI: true}))
	got := readNativeIssue(t, p, next.Repo)
	if got.Title != title || got.Lock == nil || before.Lock == nil || *got.Lock != *before.Lock {
		t.Fatal("failing run lost human title or lock", got.Title, got.Lock)
	}
	if got.Occurrences != before.Occurrences+1 || got.CIWorkID != next.ID {
		t.Fatal("failing run did not advance the CI projection")
	}
}

func checkCIPassedKeepsHumanInputs(t *testing.T, f RunFixture, title string) {
	t.Helper()
	p := f.Port.(ciIssuePort)
	a := stateport.Action{Actor: "operator"}
	red := acceptOneRun(t, f, RunCase{ID: "ci-human-failed-before-pass"})
	mustRun(t, f.Port.Finish(t.Context(), a, stateport.RunUpdate{ID: red.ID, Revision: red.Revision, Status: stepmodel.RunFailed, ProjectCI: true}))
	issue := readNativeIssue(t, p, red.Repo)
	_, err := p.UpdateIssue(t.Context(), a, model.IssuePatch{ID: issue.ID, Title: &title})
	mustRun(t, err)
	mustRun(t, p.ChangeIssueLock(t.Context(), a, stateport.IssueLockRequest{ID: issue.ID, Owner: a.Actor}))
	before := readNativeIssue(t, p, red.Repo)
	green := acceptOneRun(t, f, RunCase{ID: "ci-human-passed"})
	mustRun(t, f.Port.Finish(t.Context(), a, stateport.RunUpdate{ID: green.ID, Revision: green.Revision, Status: stepmodel.RunPassed, ProjectCI: true}))
	got := readNativeIssue(t, p, green.Repo)
	if got.Title != title || got.Lock == nil || before.Lock == nil || *got.Lock != *before.Lock {
		t.Fatal("passing run lost human title or lock", got.Title, got.Lock)
	}
	if got.ID != before.ID || got.State != model.IssueClosed || got.CIWorkID != green.ID || got.Occurrences != before.Occurrences {
		t.Fatal("passing run did not close the existing native CI issue")
	}
}
