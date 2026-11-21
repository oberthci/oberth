package contracttest

import (
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"testing"
)

func checkMovingRefusals(t *testing.T, f RunFixture) {
	p := moving(t, f)
	r := passedMovingRun(t, f, false)
	pub := movingPublication(r)
	bad := pub.Clone()
	bad.CommitSHA = new("different-commit")
	if err := p.PreparePublication(t.Context(), stateport.Action{Actor: r.Origin.Actor}, stateport.PublicationIntent{Publication: bad}); err == nil {
		t.Fatal("different tested commit became intent")
	}

	a := stateport.Action{Actor: r.Origin.Actor}
	mustRun(t, p.PreparePublication(t.Context(), a, stateport.PublicationIntent{Publication: pub}))
	_, err := p.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: pub.ID})
	mustRun(t, err)
	receipt := model.ReleaseReceipt{Tag: r.Ref, CommitSHA: *r.CommitSHA, RunID: r.ID, PublicationID: pub.ID}
	if err = p.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: pub.ID, Attempt: 1, Delivered: true, Release: &receipt}); err == nil {
		t.Fatal("ordinary branch acquired release receipt")
	}
	stored, err := p.Publication(t.Context(), pub.ID)
	mustRun(t, err)
	if stored.Status != model.PublicationPending || stored.Release != nil {
		t.Fatal("refusal mutated publication", stored)
	}
	mustRun(t, p.AbandonPublication(t.Context(), a, pub.ID))
	if _, err = p.BindPublication(t.Context(), a, pub.ID, ""); err == nil {
		t.Fatal("settled intent acquired predecessor")
	}
	promotion := movingPromotion(t, f)
	mustRun(t, p.PreparePromotion(t.Context(), a, promotion))
	b := batchFor(t, f, RunCase{ID: "wrong-promotion", Trigger: model.TriggerBranch})
	b.Runs[0].PromotionID = promotion.ID
	if _, err = p.AcceptBatch(t.Context(), b); err == nil {
		t.Fatal("ordinary run claimed promotion")
	}
	got, err := p.Promotion(t.Context(), promotion.ID)
	mustRun(t, err)
	if got.RunID != "" || got.Revision != 1 {
		t.Fatal("rejected admission changed promotion", got)
	}
}
