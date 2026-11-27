package contracttest

import (
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
)

func poisonMoving(t *testing.T, f RunFixture) {
	t.Helper()
	event := stateport.EventWrite{Event: model.AgentEvent{Kind: string(model.EventRunUpdated), Repo: "origin/team/repo", Summary: "first"}, Dedupe: "moving-rollback"}
	mustRun(t, f.Emit(t.Context(), []stateport.EventWrite{event}))
}
func movingFails(t *testing.T, f RunFixture, call func(stateport.Action) error) {
	t.Helper()
	a := stateport.Action{Actor: "operator", Events: []stateport.EventWrite{{Event: model.AgentEvent{Kind: string(model.EventRunUpdated), Repo: "origin/team/repo", Summary: "conflict"}, Dedupe: "moving-rollback"}}}
	before := len(f.Audit(t))
	changed := observeChanges(t, f.Port)
	if err := call(a); err == nil {
		t.Fatal("midway event failure accepted")
	}
	if len(f.Audit(t)) != before {
		t.Fatal("rollback retained audit")
	}
	unchangedSignal(t, changed)
}
func checkMovingRollback(t *testing.T, f RunFixture) {
	p := moving(t, f)
	poisonMoving(t, f)
	promotion := movingPromotion(t, f)
	movingFails(t, f, func(a stateport.Action) error { return p.PreparePromotion(t.Context(), a, promotion) })
	if _, err := p.Promotion(t.Context(), promotion.ID); err == nil {
		t.Fatal("rolled back promotion retained")
	}
	r, promotion := prepareMovingChild(t, f)
	intent := movingPublication(r)
	q := stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed, Promotion: &stateport.PromotionUpdate{ID: promotion.ID, Revision: promotion.Revision, RunID: r.ID, Status: model.PromotionPassed}, Publication: &stateport.PublicationIntent{Publication: intent}}
	movingFails(t, f, func(a stateport.Action) error { return p.Finish(t.Context(), a, q) })
	got, err := p.Run(t.Context(), r.ID)
	mustRun(t, err)
	if got.Status != r.Status {
		t.Fatal("verdict escaped transaction")
	}
	row, err := p.Promotion(t.Context(), promotion.ID)
	mustRun(t, err)
	if row.Revision != promotion.Revision {
		t.Fatal("promotion escaped transaction")
	}
	if _, err = p.Publication(t.Context(), intent.ID); err == nil {
		t.Fatal("publication escaped transaction")
	}
	a := stateport.Action{Actor: r.Origin.Actor}
	mustRun(t, p.Finish(t.Context(), a, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed}))
	mustRun(t, p.PreparePublication(t.Context(), a, stateport.PublicationIntent{Publication: intent}))
	movingFails(t, f, func(a stateport.Action) error { _, err := p.BindPublication(t.Context(), a, intent.ID, ""); return err })
	pub, err := p.Publication(t.Context(), intent.ID)
	mustRun(t, err)
	if pub.PreviousSHA != nil {
		t.Fatal("binding escaped transaction")
	}
	checkReleaseRollback(t, f)
	checkAssistRollback(t, f)
}
func checkReleaseRollback(t *testing.T, f RunFixture) {
	p := moving(t, f)
	r := passedMovingRun(t, f, true)
	pub := movingPublication(r)
	pub.ID = "release-publication"
	pub.PreviousSHA = new("")
	a := stateport.Action{Actor: r.Origin.Actor}
	mustRun(t, p.PreparePublication(t.Context(), a, stateport.PublicationIntent{Publication: pub}))
	_, err := p.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: pub.ID})
	mustRun(t, err)
	receipt := model.ReleaseReceipt{Tag: r.Ref, CommitSHA: *r.CommitSHA, RunID: r.ID, PublicationID: pub.ID}
	movingFails(t, f, func(a stateport.Action) error {
		return p.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: pub.ID, Attempt: 1, Delivered: true, Release: &receipt})
	})
	got, err := p.Publication(t.Context(), pub.ID)
	mustRun(t, err)
	if got.Release != nil || got.Status != model.PublicationPending {
		t.Fatal("release escaped transaction", got)
	}
}
