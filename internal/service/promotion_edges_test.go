package service_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestPolicyAbsenceNamesTheRequestedFile(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
		return gitport.Ref{Name: "refs/heads/main", ObjectSHA: head, CommitSHA: head}, gitport.File{}, &gitport.AbsentError{Repository: repository, Path: "different-policy"}
	}
	_, err := f.s.PreparePromote(t.Context(), workerCaller(), apicontract.PromoteArgs{Repo: repository, SHA: head})
	requireCode(t, err, publicerr.NotFound)
}
func TestMissingPromotionEvidenceKeepsNotFound(t *testing.T) {
	t.Parallel()
	f := setup(t)
	_, err := f.s.PreparePromote(t.Context(), workerCaller(), apicontract.PromoteArgs{Repo: repository, SHA: head})
	requireCode(t, err, publicerr.NotFound)
}
func TestTagEvidenceRequiresExplicitSkipPolicy(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunPassed)
	r.Origin.Trigger = model.TriggerTag
	r.Ref = "refs/tags/ordinary"
	f.store.native.SeedRun(r)
	_, err := f.s.PreparePromote(t.Context(), workerCaller(), apicontract.PromoteArgs{Repo: repository, SHA: head})
	requireCode(t, err, publicerr.Forbidden)
}
func TestAssistedPromotionFinishesWithoutPublication(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.seed(stepmodel.RunRunning)
	r.Origin.Trigger = model.TriggerPromotion
	r.Origin.Assisted = true
	f.store.native.SeedRun(r)
	p := model.Promotion{ID: "assisted-result", Repo: repository, Actor: caller().Identity, Caller: caller(), ResultSHA: head, RunID: r.ID, TargetRef: "refs/heads/main", BaseSHA: strings.Repeat("b", 40)}
	if err := f.store.PreparePromotion(t.Context(), stateport.Action{Actor: p.Actor}, p); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: r.ID, Status: stepmodel.RunPassed}); err != nil {
		t.Fatal(err)
	}
	stored, err := f.store.Promotion(t.Context(), p.ID)
	if err != nil || stored.Status != model.PromotionFailed || len(f.store.native.PublicationValues()) != 0 {
		t.Fatal("assisted result became evidence", stored, err)
	}
}
func TestSyncRequiresExactBranchObject(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.git.Resolve = func(_ context.Context, _ string, ref string) (gitport.Ref, error) {
		return gitport.Ref{Name: ref, ObjectSHA: strings.Repeat("d", 40), CommitSHA: head}, nil
	}
	_, err := f.s.PrepareSync(t.Context(), workerCaller(), apicontract.SyncArgs{Repo: repository, Branch: "topic", SHA: head})
	requireCode(t, err, publicerr.InvalidInput)
}
func TestDeliveredPublicationIsAnIdempotentNoOp(t *testing.T) {
	t.Parallel()
	f := setup(t)
	p := finishPublication(t, f, f.seed(stepmodel.RunRunning))
	for range 2 {
		if err := f.s.Deliver(t.Context(), p.ID); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := f.store.Publication(t.Context(), p.ID)
	if err != nil || stored.Attempts != 1 || stored.Status != model.PublicationDelivered {
		t.Fatal("delivered publication was attempted again", stored, err)
	}
}
