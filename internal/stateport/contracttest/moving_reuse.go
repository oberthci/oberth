package contracttest

import (
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func checkMovingFastForward(t *testing.T, f RunFixture) {
	p := moving(t, f)
	r := passedMovingRun(t, f, false)
	for i := range 2 {
		promotion := movingPromotion(t, f)
		promotion.ID = fmt.Sprintf("fast-forward-%d", i)
		promotion.SourceRunID = r.ID
		a := stateport.Action{Actor: r.Origin.Actor}
		mustRun(t, p.PreparePromotion(t.Context(), a, promotion))
		pub := movingPublication(r)
		pub.ID = promotion.ID
		pub.PromotionID = promotion.ID
		pub.PreviousSHA = new(promotion.BaseSHA)
		q := stateport.PromotionUpdate{ID: promotion.ID, Revision: 1, RunID: r.ID, Status: model.PromotionPassed, Publication: &stateport.PublicationIntent{Publication: pub}}
		mustRun(t, p.FinishPromotion(t.Context(), a, q))
		_, err := p.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: pub.ID})
		mustRun(t, err)
		mustRun(t, p.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: pub.ID, Attempt: 1, Reason: "delivery failed"}))
		got, err := p.Promotion(t.Context(), promotion.ID)
		mustRun(t, err)
		if got.RunID != r.ID {
			t.Fatal("passing evidence changed", got)
		}
	}
}
func checkMovingReleaseRerun(t *testing.T, f RunFixture) {
	p := moving(t, f)
	first := passedMovingRun(t, f, true)
	pub := movingPublication(first)
	a := stateport.Action{Actor: first.Origin.Actor}
	mustRun(t, p.PreparePublication(t.Context(), a, stateport.PublicationIntent{Publication: pub}))
	settleMovingRelease(t, f, pub)
	firstPublication, err := p.PublishedRelease(t.Context(), pub.Repo, pub.Ref)
	mustRun(t, err)
	b := batchFor(t, f, RunCase{ID: "release-rerun", Ref: first.Ref, Trigger: model.TriggerTag})
	b.Runs[0].Release = true
	rows, err := p.AcceptBatch(t.Context(), b)
	mustRun(t, err)
	r := rows.Runs[0]
	next := movingPublication(r)
	next.ID = "release-rerun-publication"
	mustRun(t, p.Finish(t.Context(), a, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed, Publication: &stateport.PublicationIntent{Publication: next}}))
	settleMovingRelease(t, f, next)
	got, err := p.Publication(t.Context(), next.ID)
	mustRun(t, err)
	if got.Release != nil || got.Status != model.PublicationDelivered {
		t.Fatal("release rerun emitted a second receipt", got)
	}
	retained, err := p.PublishedRelease(t.Context(), pub.Repo, pub.Ref)
	mustRun(t, err)
	if retained.ID != firstPublication.ID {
		t.Fatal("release receipt was replaced", retained)
	}
}
func settleMovingRelease(t *testing.T, f RunFixture, pub model.Publication) {
	t.Helper()
	p := moving(t, f)
	a := stateport.Action{Actor: pub.Actor}
	_, err := p.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: pub.ID})
	mustRun(t, err)
	if previous, err := p.PublishedRelease(t.Context(), pub.Repo, pub.Ref); err == nil {
		if previous.ObjectSHA != pub.ObjectSHA {
			t.Fatal("release object differs", previous, pub)
		}
		duplicate := model.ReleaseReceipt{Tag: pub.Ref, CommitSHA: *pub.CommitSHA, RunID: pub.RunID, PublicationID: pub.ID}
		if err := p.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: pub.ID, Attempt: 1, Delivered: true, Release: &duplicate}); err == nil {
			t.Fatal("duplicate release receipt accepted")
		}
		mustRun(t, p.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: pub.ID, Attempt: 1, Delivered: true}))
		return
	}
	receipt := model.ReleaseReceipt{Tag: pub.Ref, CommitSHA: *pub.CommitSHA, RunID: pub.RunID, PublicationID: pub.ID}
	a.Events = []stateport.EventWrite{{Event: model.AgentEvent{Kind: string(model.EventReleasePublished), Repo: pub.Repo, RunID: pub.RunID, Summary: "release published"}, Dedupe: pub.ID}}
	mustRun(t, p.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: pub.ID, Attempt: 1, Delivered: true, Release: &receipt}))
}
