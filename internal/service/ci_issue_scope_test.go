package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// authorFailure is the failure a red pipeline step records, as in production.
func authorFailure() *stepmodel.FailureRecord {
	return &stepmodel.FailureRecord{Class: stepmodel.FailureAuthor, Step: "go-tests", Name: "go-tests", NodeID: "node-1", Retry: stepmodel.RetryHint{Action: stepmodel.RetryNone}}
}

// seedCI seeds one running run of the given ref and trigger.
func seedCI(f *fixture, id, ref string, trigger model.RunTrigger) stateport.Run {
	r := ciRun(id, ref, head, trigger)
	f.store.native.SeedRun(r)
	return r
}

// seedRedMainBase seeds a running push run of main at the base commit, which
// the tested topic run does not share.
func seedRedMainBase(f *fixture) stateport.Run {
	r := ciRun("abcdef01000000000000000000000031", "refs/heads/main", strings.Repeat("b", 40), model.TriggerBranch)
	f.store.native.SeedRun(r)
	return r
}

// ciRun is one running run of the given ref and trigger at commit.
func ciRun(id, ref, commit string, trigger model.RunTrigger) stateport.Run {
	return stateport.Run{RunSummary: stepmodel.RunSummary{ID: id, Repo: repository, Ref: ref, ObjectSHA: commit, CommitSHA: &commit, Status: stepmodel.RunRunning, CreatedAt: at()}, HasAdmission: true,
		Admission: admissioncontract.PreparedView{Plan: admissioncontract.Plan{Exact: true, Entries: []admissioncontract.PlanEntry{{Path: "build"}}}}, Revision: 1,
		Origin: model.RunOrigin{RunCause: model.RunCause{Actor: caller().Identity, Trigger: trigger}}}
}

func finishCI(t *testing.T, f *fixture, id string, status stepmodel.RunStatus) {
	t.Helper()
	o := service.FinishedRun{RunID: id, Status: status}
	if status == stepmodel.RunFailed {
		o.Failure = authorFailure()
	}
	if err := f.s.Finished(t.Context(), o); err != nil {
		t.Fatal(err)
	}
}

func ciIssues(t *testing.T, f *fixture) []model.Issue {
	t.Helper()
	page, err := f.store.native.ScopedIssues(t.Context(), stateport.IssueScope{Global: true}, model.IssueFilter{Kind: model.IssueCI, Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	return page.Issues
}

func TestCIIssuesCoverOnlyDefaultBranchAndSchedules(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, ref string
		trigger   model.RunTrigger
		issues    int
	}{
		{"landing branch push", "refs/heads/land/tester-writer", model.TriggerBranch, 0},
		{"topic branch push", "refs/heads/topic", model.TriggerBranch, 0},
		{"branch named like the default", "refs/heads/main/next", model.TriggerBranch, 0},
		{"tag", "refs/tags/v1.0.0", model.TriggerTag, 0},
		{"default branch push", "refs/heads/main", model.TriggerBranch, 1},
		{"schedule of the default branch", "refs/heads/main", model.TriggerSchedule, 1},
		{"schedule of another branch", "refs/heads/nightly", model.TriggerSchedule, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := setup(t)
			r := seedCI(f, runID, c.ref, c.trigger)
			finishCI(t, f, r.ID, stepmodel.RunFailed)
			rows := ciIssues(t, f)
			if len(rows) != c.issues {
				t.Fatalf("failing %s run made %d CI issues, want %d", c.name, len(rows), c.issues)
			}
			if c.issues == 1 && (rows[0].Branch != c.ref || rows[0].State != model.IssueOpen || rows[0].CIWorkID != r.ID) {
				t.Fatal("CI issue does not name the failing run", rows[0])
			}
		})
	}
}

func TestBranchPassLeavesDefaultBranchIssueOpen(t *testing.T) {
	t.Parallel()
	f := setup(t)
	red := seedCI(f, "abcdef01000000000000000000000011", "refs/heads/main", model.TriggerBranch)
	finishCI(t, f, red.ID, stepmodel.RunFailed)
	branch := seedCI(f, "abcdef01000000000000000000000012", "refs/heads/land/tester-writer", model.TriggerBranch)
	finishCI(t, f, branch.ID, stepmodel.RunPassed)
	if rows := ciIssues(t, f); len(rows) != 1 || rows[0].State != model.IssueOpen || rows[0].CIWorkID != red.ID {
		t.Fatal("a branch pass changed the default branch issue", rows)
	}
	green := seedCI(f, "abcdef01000000000000000000000013", "refs/heads/main", model.TriggerBranch)
	finishCI(t, f, green.ID, stepmodel.RunPassed)
	if rows := ciIssues(t, f); len(rows) != 1 || rows[0].State != model.IssueClosed || rows[0].CIWorkID != green.ID {
		t.Fatal("a default branch pass did not close its issue", rows)
	}
}

// A passing promotion closes the default branch issue; a failed one leaves it
// as the last default branch run left it.
func TestPromotionPassClosesAndFailureLeavesDefaultBranchIssue(t *testing.T) {
	t.Parallel()
	for _, status := range []stepmodel.RunStatus{stepmodel.RunFailed, stepmodel.RunPassed} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			f := setup(t)
			red := seedCI(f, "abcdef01000000000000000000000021", "refs/heads/main", model.TriggerBranch)
			finishCI(t, f, red.ID, stepmodel.RunFailed)
			promoted := promotionRun(t, f)
			if promoted.Ref != "refs/heads/main" {
				t.Fatal("promotion run does not target the default branch", promoted.Ref)
			}
			finishCI(t, f, promoted.ID, status)
			want, work, occurrences := model.IssueOpen, red.ID, int64(1)
			if status == stepmodel.RunPassed {
				want, work = model.IssueClosed, promoted.ID
			}
			if rows := ciIssues(t, f); len(rows) != 1 || rows[0].State != want || rows[0].CIWorkID != work || rows[0].Occurrences != occurrences {
				t.Fatal("promotion run moved the default branch issue wrongly", rows)
			}
		})
	}
}

func TestFailedPromotionCreatesNoCIIssue(t *testing.T) {
	t.Parallel()
	f := setup(t)
	finishCI(t, f, promotionRun(t, f).ID, stepmodel.RunFailed)
	if rows := ciIssues(t, f); len(rows) != 0 {
		t.Fatal("a failed promotion created a CI issue", rows)
	}
}

// A fast-forward promotion of a fix closes the red default-branch issue once
// its publication is delivered: main is then exactly the tested commit. The
// tested run predates the red run, which tested an older main.
func TestFastForwardPromotionClosesDefaultBranchIssueAfterPublication(t *testing.T) {
	t.Parallel()
	f := setup(t)
	source := f.seed(stepmodel.RunPassed)
	red := seedRedMainBase(f)
	finishCI(t, f, red.ID, stepmodel.RunFailed)
	if out := promote(t, f); out.RunID != source.ID {
		t.Fatal("fast-forward promotion started a promotion run", out)
	}
	if rows := ciIssues(t, f); len(rows) != 1 || rows[0].State != model.IssueOpen {
		t.Fatal("issue closed before main moved", rows)
	}
	deliverPending(t, f)
	if rows := ciIssues(t, f); len(rows) != 1 || rows[0].State != model.IssueClosed || rows[0].CIWorkID != source.ID {
		t.Fatal("delivered fast-forward left the default branch issue", rows)
	}
}

// A fast-forward whose publication fails leaves main, and its issue, as they were.
func TestFailedFastForwardPublicationClosesNothing(t *testing.T) {
	t.Parallel()
	f := setup(t)
	source := f.seed(stepmodel.RunPassed)
	red := seedRedMainBase(f)
	finishCI(t, f, red.ID, stepmodel.RunFailed)
	f.moving.Push = func(context.Context, model.Publication) (gitport.PublicationReceipt, error) {
		return gitport.PublicationReceipt{}, errors.New("forge refused the push")
	}
	out := promote(t, f)
	if out.RunID != source.ID {
		t.Fatal("fast-forward promotion started a promotion run", out)
	}
	deliverPending(t, f)
	if p, err := f.store.Publication(t.Context(), "publication:"+out.ID); err != nil || p.Status != model.PublicationFailed {
		t.Fatal("publication did not fail", p, err)
	}
	if rows := ciIssues(t, f); len(rows) != 1 || rows[0].State != model.IssueOpen || rows[0].CIWorkID != red.ID {
		t.Fatal("failed fast-forward changed the default branch issue", rows)
	}
}

// deliverPending fires the armed publication and requires its worker to settle.
func deliverPending(t *testing.T, f *fixture) {
	t.Helper()
	if !f.timers.Fire() {
		t.Fatal("publication not armed")
	}
	workerOK(t, f)
}

// promotionRun promotes the seeded passing topic run onto a merged result,
// which starts a promotion run of the default branch.
func promotionRun(t *testing.T, f *fixture) stateport.Run {
	t.Helper()
	f.seed(stepmodel.RunPassed)
	merged := strings.Repeat("d", 40)
	f.moving.Prepare = func(_ context.Context, q gitport.PromotionRequest) (gitport.Promotion, error) {
		return gitport.Promotion{Result: gitport.Ref{Name: q.Target.Name, ObjectSHA: merged, CommitSHA: merged}}, nil
	}
	out := promote(t, f)
	r, err := f.store.Run(t.Context(), out.RunID)
	if err != nil || r.Origin.Trigger != model.TriggerPromotion {
		t.Fatal("promotion started no promotion run", r, err)
	}
	return r
}
