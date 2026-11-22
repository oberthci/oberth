package contracttest

import (
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"strings"
	"testing"
)

// MovingSuite is the same persistence protocol for disk and the owner fake.
func MovingSuite(t *testing.T, factory func(*testing.T) RunFixture) {
	t.Run("ci_passing_human_inputs", func(t *testing.T) {
		title := "flaky login test, agent investigating"
		for name, custom := range map[string]string{
			"custom_title":   title,
			"title_at_bound": title + strings.Repeat("<", model.CITextBytes-len(title)),
		} {
			t.Run(name, func(t *testing.T) { checkCIPassedKeepsHumanInputs(t, factory(t), custom) })
		}
	})
	for name, check := range map[string]func(*testing.T, RunFixture){"ci_budget": checkCIIssueBudget, "ci_human_inputs": checkCIRedKeepsHumanInputs, "publication": checkMovingPublication, "promotion": checkMovingPromotion, "nested_finish": checkMovingFinish, "inspection": checkMovingHolds, "inspection_evidence": checkInspectionEvidence, "release": checkMovingRelease, "rollback": checkMovingRollback, "refusals": checkMovingRefusals, "fast_forward_retry": checkMovingFastForward, "published_ci": checkPublishedCI, "release_rerun": checkMovingReleaseRerun} {
		t.Run(name, func(t *testing.T) { check(t, factory(t)) })
	}
}
func moving(t *testing.T, f RunFixture) MovingPort {
	t.Helper()
	p, ok := f.Port.(MovingPort)
	if !ok {
		t.Fatal("fixture lacks moving protocol")
	}
	return p
}
func passedMovingRun(t *testing.T, f RunFixture, release bool) stateport.Run {
	t.Helper()
	b := batchFor(t, f, RunCase{ID: "moving-run"})
	if release {
		b = batchFor(t, f, RunCase{ID: "moving-run", Ref: "refs/tags/v1.0.0", Trigger: model.TriggerTag})
		b.Runs[0].Release = true
	}
	result, err := f.Port.AcceptBatch(t.Context(), b)
	mustRun(t, err)
	r := result.Runs[0]
	mustRun(t, f.Port.Finish(t.Context(), stateport.Action{Actor: r.Origin.Actor}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed}))
	r, err = f.Port.Run(t.Context(), r.ID)
	mustRun(t, err)
	return r
}
func movingPublication(r stateport.Run) model.Publication {
	return model.Publication{ID: "moving-publication", Repo: r.Repo, Ref: r.Ref, RunID: r.ID, ObjectSHA: r.ObjectSHA, CommitSHA: r.CommitSHA, Actor: r.Origin.Actor}
}
func checkMovingPublication(t *testing.T, f RunFixture) {
	r := passedMovingRun(t, f, false)
	PublicationStore(t, moving(t, f), movingPublication(r))
	requireMovingAudit(t, f, "publication.prepare")
	requireMovingAudit(t, f, "publication.bind")
	for _, operation := range []string{"prepare", "bind", "attempt", "settle", "retry", "abandon"} {
		requireMovingReason(t, f, "publication."+operation, "publication contract")
	}
}
func movingPromotion(t *testing.T, f RunFixture) model.Promotion {
	t.Helper()
	b := batchFor(t, f, RunCase{ID: "promotion-child", Trigger: model.TriggerPromotion})
	a := b.Actor.Clone()
	a.Scope = []string{"origin"}
	return model.Promotion{ID: "promotion", Repo: b.Repository, Actor: a.Identity, Caller: a, SourceSHA: b.Runs[0].Request.ObjectSHA, ResultSHA: b.Runs[0].Request.ObjectSHA, TargetRef: b.Runs[0].Request.Ref, BaseSHA: b.Runs[0].Request.ObjectSHA}
}
func checkMovingPromotion(t *testing.T, f RunFixture) {
	PromotionStore(t, moving(t, f), movingPromotion(t, f))
	requireMovingAudit(t, f, "promotion.prepare")
	requireMovingAudit(t, f, "promotion.finish")
}
func prepareMovingChild(t *testing.T, f RunFixture) (stateport.Run, model.Promotion) {
	t.Helper()
	p := moving(t, f)
	promotion := movingPromotion(t, f)
	mustRun(t, p.PreparePromotion(t.Context(), stateport.Action{Actor: promotion.Actor}, promotion))
	b := batchFor(t, f, RunCase{ID: "promotion-child", Trigger: model.TriggerPromotion})
	b.Runs[0].PromotionID = promotion.ID
	result, err := p.AcceptBatch(t.Context(), b)
	mustRun(t, err)
	r := result.Runs[0]
	promotion, err = p.PromotionForRun(t.Context(), r.ID)
	mustRun(t, err)
	return r, promotion
}
func checkMovingFinish(t *testing.T, f RunFixture) {
	p := moving(t, f)
	r, promotion := prepareMovingChild(t, f)
	intent := movingPublication(r)
	intent.PromotionID = promotion.ID
	intent.PreviousSHA = new(promotion.BaseSHA)
	q := stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: stepmodel.RunPassed, Promotion: &stateport.PromotionUpdate{ID: promotion.ID, Revision: promotion.Revision, RunID: r.ID, Status: model.PromotionPassed}, Publication: &stateport.PublicationIntent{Publication: intent}}
	a := stateport.Action{Actor: r.Origin.Actor, Events: []stateport.EventWrite{runEvent(r, "moving-finish")}}
	mustRun(t, p.Finish(t.Context(), a, q))
	stored, err := p.Promotion(t.Context(), promotion.ID)
	mustRun(t, err)
	pub, err := p.Publication(t.Context(), intent.ID)
	mustRun(t, err)
	if stored.Status != model.PromotionPassed || pub.PromotionID != stored.ID || pub.Status != model.PublicationPending {
		t.Fatal("nested finish lost promotion/publication", stored, pub)
	}
	requireMovingAudit(t, f, "promotion.finish")
	requireMovingAudit(t, f, "publication.prepare")
	if err = p.Finish(t.Context(), a, q); err == nil {
		t.Fatal("stale nested finish reused")
	}
}

func checkMovingRelease(t *testing.T, f RunFixture) {
	p := moving(t, f)
	r := passedMovingRun(t, f, true)
	pub := movingPublication(r)
	pub.PreviousSHA = new("")
	a := stateport.Action{Actor: r.Origin.Actor}
	mustRun(t, p.PreparePublication(t.Context(), a, stateport.PublicationIntent{Publication: pub}))
	_, err := p.StartPublicationAttempt(t.Context(), a, stateport.PublicationAttempt{ID: pub.ID})
	mustRun(t, err)
	receipt := model.ReleaseReceipt{Tag: r.Ref, CommitSHA: *r.CommitSHA, RunID: r.ID, PublicationID: pub.ID}
	a.Events = []stateport.EventWrite{{Event: model.AgentEvent{Kind: string(model.EventReleasePublished), Repo: r.Repo, RunID: r.ID, Summary: "release published"}, Dedupe: "release-event"}}
	mustRun(t, p.SettlePublication(t.Context(), a, stateport.PublicationResult{ID: pub.ID, Attempt: 1, Delivered: true, Release: &receipt}))
	got, err := p.Publication(t.Context(), pub.ID)
	mustRun(t, err)
	requireMovingAudit(t, f, "publication.settle")
	if got.Release == nil || *got.Release != receipt {
		t.Fatal("release receipt lost", got)
	}
	events, err := p.Events(t.Context(), stateport.EventFilter{Repositories: []string{r.Repo}, Limit: 200})
	mustRun(t, err)
	if len(events.Events) != 1 || events.Events[0].Kind != string(model.EventReleasePublished) {
		t.Fatal("release event lost", events)
	}
}

func requireMovingAudit(t *testing.T, f RunFixture, action string) {
	t.Helper()
	for _, a := range f.Audit(t) {
		if a.Action == action && a.Actor != "" {
			return
		}
	}
	t.Fatal("fact missing attributed audit", action)
}

func requireMovingReason(t *testing.T, f RunFixture, action, reason string) {
	t.Helper()
	for _, a := range f.Audit(t) {
		if a.Action == action && strings.Contains(a.Details, reason) {
			return
		}
	}
	t.Fatal("audit lost caller reason", action)
}
