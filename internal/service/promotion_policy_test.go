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

func TestPromotionPolicyRefusesUnknownFieldsAndExtraDocuments(t *testing.T) {
	t.Parallel()
	for _, body := range []string{"uplinks_may_promote: true\nunknown: true", "uplinks_may_promote: true\n---\nuplinks_may_promote: true"} {
		f := setup(t)
		f.seed(stepmodel.RunPassed)
		f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
			return gitport.Ref{Name: "refs/heads/main", ObjectSHA: head, CommitSHA: head}, gitport.File{Bytes: []byte(body)}, nil
		}
		_, err := f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteArgs{Repo: repository, SHA: head})
		requireCode(t, err, publicerr.InvalidInput)
	}
}

func TestPromotionRetryRequiresFailureAndFreshPolicy(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	p := model.Promotion{ID: "failed", Repo: repository, SourceSHA: head, ResultSHA: head, Caller: caller(), Actor: caller().Identity}
	a := stateport.Action{Actor: p.Actor}
	if err := f.store.PreparePromotion(t.Context(), a, p); err != nil {
		t.Fatal(err)
	}
	q := apicontract.PromoteRetryArgs{ID: p.ID, Reason: "try again"}
	_, err := f.s.PreparePromoteRetry(t.Context(), apicontract.Caller{Actor: caller()}, q)
	requireCode(t, err, publicerr.InvalidTransition)
	if err := f.store.FinishPromotion(t.Context(), a, stateport.PromotionUpdate{ID: p.ID, Revision: 1, Status: model.PromotionFailed}); err != nil {
		t.Fatal(err)
	}
	base := strings.Repeat("c", 40)
	f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
		return gitport.Ref{Name: "refs/heads/main", ObjectSHA: base, CommitSHA: base}, gitport.File{Bytes: []byte("uplinks_may_promote: false")}, nil
	}
	_, err = f.s.PreparePromoteRetry(t.Context(), apicontract.Caller{Actor: caller()}, q)
	requireCode(t, err, publicerr.Forbidden)
	call, err := f.s.PreparePromoteRetry(t.Context(), apicontract.Caller{Actor: admin()}, q)
	if err != nil {
		t.Fatal(err)
	}
	out, err := call(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	row, err := f.store.Promotion(t.Context(), out.ID)
	if err != nil || row.PredecessorID != p.ID || row.Actor != admin().Identity || !row.Caller.Admin || row.BaseSHA != base {
		t.Fatal("retry did not use fresh caller and head", row, err)
	}
}

func TestPromotionRefusesPreparedWrongTargetOrObject(t *testing.T) {
	t.Parallel()
	for _, which := range []string{"target", "object"} {
		f := setup(t)
		f.seed(stepmodel.RunPassed)
		f.moving.Prepare = func(context.Context, gitport.PromotionRequest) (gitport.Promotion, error) {
			r := gitport.Ref{Name: "refs/heads/main", ObjectSHA: head, CommitSHA: head}
			if which == "target" {
				r.Name = "refs/heads/other"
			} else {
				r.ObjectSHA = strings.Repeat("c", 40)
			}
			return gitport.Promotion{Result: r}, nil
		}
		call, err := f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteArgs{Repo: repository, SHA: head})
		if err != nil {
			t.Fatal(err)
		}
		_, err = call(t.Context())
		requireCode(t, err, publicerr.Forbidden)
		if len(f.store.native.PublicationValues()) != 0 {
			t.Fatal("invalid prepared result became intent")
		}
	}
}

func TestPolicyAllowsUntestedCommitOnlyWithFreshPromotionRun(t *testing.T) {
	t.Parallel()
	f := setup(t)
	sha := strings.Repeat("f", 40)
	f.source.File = func(context.Context, string, string) (gitport.Ref, gitport.File, error) {
		return gitport.Ref{Name: "refs/heads/main", ObjectSHA: head, CommitSHA: head}, gitport.File{Bytes: []byte("uplinks_may_promote: true\nallow_without_branch_ci: true")}, nil
	}
	call, err := f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteArgs{Repo: repository, SHA: sha})
	if err != nil {
		t.Fatal(err)
	}
	out, err := call(t.Context())
	if err != nil || out.TestedRunID != "" || out.RunID == "" || out.CommitSHA != sha || len(f.store.native.PublicationValues()) != 0 {
		t.Fatal("untested source bypassed promotion CI", out, err)
	}
	r, err := f.store.Run(t.Context(), out.RunID)
	if err != nil || r.Origin.Trigger != model.TriggerPromotion || r.ObjectSHA != sha {
		t.Fatal("starter lost exact untested result", r, err)
	}
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: r.ID, Status: stepmodel.RunPassed}); err != nil {
		t.Fatal(err)
	}
	checkMergedVerdict(t, f, out.ID, stepmodel.RunPassed)
}
