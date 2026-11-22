package app_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// A topic cut from an unmoved default branch fast-forwards: the promotion
// passes on the topic's run, starts no run of its own and publishes exactly
// the tested commit.
func TestRealFastForwardPromotionPublishesTheTestedCommit(t *testing.T) {
	t.Parallel()
	f := newForwardFixture(t)
	base := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/main")
	branch := f.branch(t, stepmodel.RunPassed)
	out, err := f.promote(t)(t.Context())
	landedOK(t, err)
	if out.RunID != branch.ID || out.TestedRunID != branch.ID || out.CommitSHA != f.git.Commit {
		t.Fatal("fast-forward did not stand on the tested run", out)
	}
	if len(f.starter.requests) != 0 {
		t.Fatal("fast-forward started a promotion run", f.starter.requests)
	}
	f.assertExecutions(t, 1, 1)
	p, err := f.db.Promotion(t.Context(), out.ID)
	landedOK(t, err)
	if p.Status != model.PromotionPassed || p.RunID != branch.ID || p.SourceSHA != f.git.Commit || p.ResultSHA != f.git.Commit || p.BaseSHA != base {
		t.Fatal("promotion record disagrees with the fast-forward", p)
	}
	f.assertForwardDelivery(t, out, base)
}

// assertForwardDelivery delivers the fast-forward and finds main at the tested
// commit, moved from base, with no promotion run on record.
func (f *movingFixture) assertForwardDelivery(t *testing.T, out apicontract.PromoteResult, base string) {
	t.Helper()
	id := "publication:" + out.ID
	landedOK(t, f.svc.Deliver(t.Context(), id))
	publication, err := f.db.Publication(t.Context(), id)
	landedOK(t, err)
	remote := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/main")
	assertDeliveredPromotion(t, out, publication, remote)
	if publication.PreviousSHA == nil || *publication.PreviousSHA != base {
		t.Fatal("publication lost the base it moves from", publication)
	}
	rows, err := f.db.Runs(t.Context(), stateport.RunFilter{Repositories: []string{request().Repository}, Trigger: model.TriggerPromotion, Limit: 10})
	landedOK(t, err)
	if len(rows) != 0 {
		t.Fatal("fast-forward recorded a promotion run", rows)
	}
}
