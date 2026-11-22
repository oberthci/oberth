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

// checkPublishedCICountedOnce: a run already counted for main, then published
// with ProjectCI, settles as delivered and leaves the issue as a newer run set it.
func checkPublishedCICountedOnce(t *testing.T, f RunFixture) {
	p := moving(t, f)
	a := stateport.Action{Actor: "operator"}
	green := acceptOneRun(t, f, RunCase{ID: "ci-counted-green", Commit: strings.Repeat("e", 40)})
	mustRun(t, f.Port.Finish(t.Context(), a, stateport.RunUpdate{ID: green.ID, Revision: green.Revision, Status: stepmodel.RunPassed, ProjectCI: true}))
	red := acceptOneRun(t, f, RunCase{ID: "ci-counted-red", Commit: strings.Repeat("f", 40)})
	mustRun(t, f.Port.Finish(t.Context(), a, stateport.RunUpdate{ID: red.ID, Revision: red.Revision, Status: stepmodel.RunFailed, ProjectCI: true}))
	green, err := f.Port.Run(t.Context(), green.ID)
	mustRun(t, err)
	settleForward(t, p, forwardPublication(green, "ci-counted-publication"), true)
	got, err := p.Publication(t.Context(), "ci-counted-publication")
	mustRun(t, err)
	if got.Status != model.PublicationDelivered {
		t.Fatal("counted run did not settle as delivered", got.Status)
	}
	requireCIIssue(t, f, model.IssueOpen, red.ID)
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
	startForward(t, p, pub)
	settleStarted(t, p, pub, delivered)
}

// startForward prepares pub and starts its first push attempt.
func startForward(t *testing.T, p MovingPort, pub model.Publication) {
	t.Helper()
	a := stateport.Action{Actor: pub.Actor}
	mustRun(t, p.PreparePublication(t.Context(), a, stateport.PublicationIntent{Publication: pub}))
	_, err := p.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: pub.ID})
	mustRun(t, err)
}

// settleStarted settles pub's first attempt, asking for CI projection.
func settleStarted(t *testing.T, p MovingPort, pub model.Publication, delivered bool) {
	t.Helper()
	a := stateport.Action{Actor: pub.Actor}
	mustRun(t, p.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: pub.ID, Attempt: 1, Delivered: delivered, ProjectCI: true}))
}

// A main run created after the push attempt started may have tested the new
// head, so it is ordered after the delivered fast-forward whether it finishes
// after the settlement (checkPublishedCIRunAfterPush) or before it
// (checkPublishedCIRunBeforeSettlement).
func checkPublishedCIRunAfterPush(t *testing.T, f RunFixture) { checkRunAfterPush(t, f, false) }
func checkPublishedCIRunBeforeSettlement(t *testing.T, f RunFixture) {
	checkRunAfterPush(t, f, true)
}
func checkRunAfterPush(t *testing.T, f RunFixture, finishFirst bool) {
	t.Helper()
	p := moving(t, f)
	a := stateport.Action{Actor: "operator"}
	tested := acceptOneRun(t, f, RunCase{ID: "ci-push-tested", Ref: "refs/heads/topic"})
	mustRun(t, f.Port.Finish(t.Context(), a, stateport.RunUpdate{ID: tested.ID, Revision: tested.Revision, Status: stepmodel.RunPassed}))
	red := acceptOneRun(t, f, RunCase{ID: "ci-push-red", Commit: strings.Repeat("b", 40)})
	mustRun(t, f.Port.Finish(t.Context(), a, stateport.RunUpdate{ID: red.ID, Revision: red.Revision, Status: stepmodel.RunFailed, ProjectCI: true}))
	tested, err := f.Port.Run(t.Context(), tested.ID)
	mustRun(t, err)
	pub := forwardPublication(tested, "ci-push-publication")
	startForward(t, p, pub)
	newer := acceptOneRun(t, f, RunCase{ID: "ci-push-newer", Commit: strings.Repeat("a", 40), Trigger: model.TriggerSchedule})
	finish := stateport.RunUpdate{ID: newer.ID, Revision: newer.Revision, Status: stepmodel.RunFailed, ProjectCI: true}
	if finishFirst {
		mustRun(t, f.Port.Finish(t.Context(), a, finish))
	}
	settleStarted(t, p, pub, true)
	if !finishFirst {
		requireCIIssue(t, f, model.IssueClosed, tested.ID)
		mustRun(t, f.Port.Finish(t.Context(), a, finish))
	}
	requireCIIssue(t, f, model.IssueOpen, newer.ID)
}

func requireCIIssue(t *testing.T, f RunFixture, state model.IssueState, runID string) {
	t.Helper()
	got := readNativeIssue(t, f.Port.(ciIssuePort), "origin/team/repo")
	if got.State != state || got.CIWorkID != runID {
		t.Fatal("CI issue disagrees with the newest main result", got.State, got.CIWorkID, state, runID)
	}
}
