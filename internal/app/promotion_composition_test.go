package app_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestRealPromotionOwnersPublishTheAdmittedCommit(t *testing.T) {
	t.Parallel()
	f := newMovingFixture(t)
	branch := f.branch(t, stepmodel.RunPassed)
	out, err := f.promote(t)(t.Context())
	landedOK(t, err)
	if out.RunID == branch.ID || out.CommitSHA == f.git.Commit {
		t.Fatal("promotion did not admit its merge result", out)
	}
	f.assertPromotionBinding(t, out)
	f.finish(t, out.RunID, stepmodel.RunPassed)
	id := "publication:" + out.ID
	landedOK(t, f.svc.Deliver(t.Context(), id))
	p, err := f.db.Publication(t.Context(), id)
	landedOK(t, err)
	remote := f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/main")
	assertDeliveredPromotion(t, out, p, remote)
	f.assertDeliveredReceipt(t, p)
	landedOK(t, f.svc.Deliver(t.Context(), id))
	if len(f.moving.receipts) != 1 || f.creates.Load() != 2 || f.prepares.Load() != 2 {
		t.Fatal("publication replay repeated execution or source delivery")
	}
}

func assertDeliveredPromotion(t *testing.T, out apicontract.PromoteResult, p model.Publication, remote string) {
	t.Helper()
	if p.Status != model.PublicationDelivered || p.Attempts != 1 || p.PromotionID != out.ID || p.RunID != out.RunID || p.ObjectSHA != out.CommitSHA || p.CommitSHA == nil || *p.CommitSHA != out.CommitSHA || remote != out.CommitSHA {
		t.Fatal("durable delivery disagrees with admitted and upstream commit", p, out, remote)
	}
}

func (f *movingFixture) assertPromotionBinding(t *testing.T, out apicontract.PromoteResult) {
	t.Helper()
	p, err := f.db.PromotionForRun(t.Context(), out.RunID)
	landedOK(t, err)
	r, err := f.db.Run(t.Context(), out.RunID)
	landedOK(t, err)
	if p.ID != out.ID || p.ResultSHA != out.CommitSHA || p.Caller.Identity != movingCaller().Actor.Identity {
		t.Fatal("promotion identity disagrees with its result", p, out)
	}
	assertMergeRun(t, r, out.CommitSHA)
	if len(r.Admission.SourceBindings) != 1 || r.Admission.SourceBindings[0].Revision.Commit != out.CommitSHA {
		t.Fatal("admission did not bind the real prepared source", r.Admission.SourceBindings)
	}
	if len(f.moving.requests) != 1 || f.moving.requests[0].ActionReceipt != out.ID || f.moving.requests[0].Source.Commit != f.git.Commit || f.moving.requests[0].Target.ObjectSHA != p.BaseSHA {
		t.Fatal("source prepared different roots", f.moving.requests, p)
	}
}

func assertMergeRun(t *testing.T, r stateport.Run, commit string) {
	t.Helper()
	if r.CommitSHA == nil || *r.CommitSHA != commit || r.Admission.CommitSHA != commit || r.Admission.ObjectSHA != commit || r.Origin.Trigger != model.TriggerPromotion || r.Origin.WorkflowName != nativebinding.WorkflowName(r.ID) || r.Origin.WorkflowUID == "" {
		t.Fatal("admission and native binding disagree with merge result", r, commit)
	}
}

func (f *movingFixture) assertDeliveredReceipt(t *testing.T, p model.Publication) {
	t.Helper()
	want := gitport.PublicationReceipt{Repository: p.Repo, ActionReceipt: p.ID, Refs: []gitport.Ref{{Name: p.Ref, ObjectSHA: p.ObjectSHA, CommitSHA: *p.CommitSHA}}}
	if len(f.moving.receipts) != 1 {
		t.Fatal("missing real source readback receipt", f.moving.receipts)
	}
	r := f.moving.receipts[0]
	if r.Repository != want.Repository || r.ActionReceipt != want.ActionReceipt || !slices.Equal(r.Refs, want.Refs) {
		t.Fatal("source readback lost the exact publication identity", r, want)
	}
	f.assertFinishedEvent(t, p)
	f.assertDeliveryAudit(t, p)
}

func (f *movingFixture) assertFinishedEvent(t *testing.T, p model.Publication) {
	t.Helper()
	events, err := f.db.Events(t.Context(), stateport.EventFilter{Repositories: []string{p.Repo}, Limit: 100})
	landedOK(t, err)
	completed := 0
	for _, e := range events.Events {
		if e.Kind == string(model.EventRunFinished) && e.RunID == p.RunID {
			if e.Ref != p.Ref || e.ObjectSHA != p.ObjectSHA || e.CommitSHA == nil || *e.CommitSHA != *p.CommitSHA {
				t.Fatal("finished event lost tested evidence", e, p)
			}
			completed++
		}
	}
	if completed != 1 {
		t.Fatal("Finished did not commit one terminal event", events)
	}
}

func (f *movingFixture) assertDeliveryAudit(t *testing.T, p model.Publication) {
	t.Helper()
	rows, _, err := f.db.Audit(t.Context(), 0, 200)
	landedOK(t, err)
	delivered := 0
	for _, a := range rows {
		if a.Action != "publication.settle" || a.ResourceID != p.ID {
			continue
		}
		var detail struct{ Result stateport.PublicationResult }
		landedOK(t, json.Unmarshal([]byte(a.Details), &detail))
		if a.Actor != p.Actor || !detail.Result.Delivered || detail.Result.ID != p.ID || detail.Result.Attempt != 1 {
			t.Fatal("durable settlement lost delivered evidence", a)
		}
		delivered++
	}
	if delivered != 1 {
		t.Fatal("missing one durable delivery settlement", rows)
	}
}

func TestMovingPromotionTargetRefreshesPolicyBeforeSuccessor(t *testing.T) {
	t.Parallel()
	for _, narrowed := range []bool{false, true} {
		t.Run(map[bool]string{false: "allowed", true: "narrowed"}[narrowed], func(t *testing.T) {
			f := newMovingFixture(t)
			f.branch(t, stepmodel.RunPassed)
			out, err := f.promote(t)(t.Context())
			landedOK(t, err)
			f.finish(t, out.RunID, stepmodel.RunPassed)
			head := f.advanceDefault(t, narrowed)
			err = f.svc.Deliver(t.Context(), "publication:"+out.ID)
			if narrowed {
				if publicerr.Classify(err).Code() != publicerr.Forbidden {
					t.Fatal("fresh narrowed policy did not refuse", err)
				}
			} else {
				landedOK(t, err)
			}
			f.assertSuccessor(t, out, head, narrowed)
		})
	}
}

func (f *movingFixture) advanceDefault(t *testing.T, narrowed bool) string {
	t.Helper()
	g := f.git
	g.Git(t, g.Work, "checkout", "main")
	g.Write(t, "target-change", []byte("new default head\n"))
	if narrowed {
		g.Write(t, ".oberth/promotion.yaml", []byte("uplinks_may_promote: false\n"))
	}
	head := g.CommitFile(t, "advance promotion target")
	g.Git(t, g.Work, "push", g.Bare, "main")
	return head
}

func (f *movingFixture) assertSuccessor(t *testing.T, out apicontract.PromoteResult, head string, narrowed bool) {
	t.Helper()
	old, err := f.db.Publication(t.Context(), "publication:"+out.ID)
	landedOK(t, err)
	if old.Status != model.PublicationAbandoned || f.git.Git(t, f.git.Bare, "rev-parse", "refs/heads/main") != head {
		t.Fatal("stale promotion moved the target", old)
	}
	rows, err := f.db.Promotions(t.Context(), stateport.RecordFilter{Repositories: []string{request().Repository}, SourceSHA: f.git.Commit, Limit: 10})
	landedOK(t, err)
	if narrowed {
		f.assertExecutions(t, 2, 1)
		if len(rows) != 1 {
			t.Fatal("refusal created or prepared a successor", rows)
		}
		return
	}
	f.assertExecutions(t, 3, 2)
	if len(rows) != 2 {
		t.Fatal("target movement did not create exactly one successor", rows)
	}
	for _, p := range rows {
		if p.ID == out.ID {
			continue
		}
		f.assertSuccessorRecord(t, out, p, head)
	}
}

func (f *movingFixture) assertSuccessorRecord(t *testing.T, out apicontract.PromoteResult, p model.Promotion, head string) {
	t.Helper()
	if p.PredecessorID != out.ID || p.Successors != 1 || p.BaseSHA != head || p.Status != model.PromotionPending || p.RunID == out.RunID || f.moving.requests[1].Target.ObjectSHA != head {
		t.Fatal("successor lost refreshed target or predecessor", p, f.moving.requests)
	}
	r, err := f.db.Run(t.Context(), p.RunID)
	landedOK(t, err)
	assertMergeRun(t, r, p.ResultSHA)
}
