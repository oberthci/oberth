package contracttest

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// checkPublishedCI: a delivered publication with ProjectCI makes its passing
// run the published ref's newest CI result, though the run predates the red
// one; an undelivered one moves nothing; a run created before the delivery
// no longer moves the issue, and one created after it does.
func checkPublishedCI(t *testing.T, f RunFixture) {
	p := moving(t, f)
	a := stateport.Action{Actor: "operator"}
	tested := acceptOneRun(t, f, RunCase{ID: "ci-forward-tested", Ref: "refs/heads/topic"})
	mustRun(t, f.Port.Finish(t.Context(), a, stateport.RunUpdate{ID: tested.ID, Revision: tested.Revision, Status: stepmodel.RunPassed}))
	red := acceptOneRun(t, f, RunCase{ID: "ci-forward-red", Commit: strings.Repeat("b", 40)})
	mustRun(t, f.Port.Finish(t.Context(), a, stateport.RunUpdate{ID: red.ID, Revision: red.Revision, Status: stepmodel.RunFailed, ProjectCI: true}))
	late := acceptOneRun(t, f, RunCase{ID: "ci-forward-late", Commit: strings.Repeat("c", 40)})
	tested, err := f.Port.Run(t.Context(), tested.ID)
	mustRun(t, err)
	settleForward(t, p, forwardPublication(tested, "ci-forward-failed"), false)
	requireCIIssue(t, f, model.IssueOpen, red.ID)
	settleForward(t, p, forwardPublication(tested, "ci-forward-delivered"), true)
	requireCIIssue(t, f, model.IssueClosed, tested.ID)
	mustRun(t, f.Port.Finish(t.Context(), a, stateport.RunUpdate{ID: late.ID, Revision: late.Revision, Status: stepmodel.RunFailed, ProjectCI: true}))
	requireCIIssue(t, f, model.IssueClosed, tested.ID)
	next := acceptOneRun(t, f, RunCase{ID: "ci-forward-next", Commit: strings.Repeat("d", 40)})
	mustRun(t, f.Port.Finish(t.Context(), a, stateport.RunUpdate{ID: next.ID, Revision: next.Revision, Status: stepmodel.RunFailed, ProjectCI: true}))
	requireCIIssue(t, f, model.IssueOpen, next.ID)
}

// forwardPublication moves main from the red base to the tested run's commit.
func forwardPublication(tested stateport.Run, id string) model.Publication {
	pub := movingPublication(tested)
	pub.ID, pub.Ref, pub.PreviousSHA = id, "refs/heads/main", new(strings.Repeat("b", 40))
	return pub
}

// settleForward prepares, attempts and settles pub, asking for CI projection.
func settleForward(t *testing.T, p MovingPort, pub model.Publication, delivered bool) {
	t.Helper()
	a := stateport.Action{Actor: pub.Actor}
	mustRun(t, p.PreparePublication(t.Context(), a, stateport.PublicationIntent{Publication: pub}))
	_, err := p.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: pub.ID})
	mustRun(t, err)
	mustRun(t, p.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: pub.ID, Attempt: 1, Delivered: delivered, ProjectCI: true}))
}

func requireCIIssue(t *testing.T, f RunFixture, state model.IssueState, runID string) {
	t.Helper()
	got := readNativeIssue(t, f.Port.(ciIssuePort), "origin/team/repo")
	if got.State != state || got.CIWorkID != runID {
		t.Fatal("CI issue disagrees with the newest main result", got.State, got.CIWorkID, state, runID)
	}
}
