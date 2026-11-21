package app_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestDefaultFileUsesOneFreshDefaultSnapshotPerRead(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	first := sourcecontract.Ref{Name: "refs/heads/main", ObjectSHA: strings.Repeat("a", 40), CommitSHA: strings.Repeat("b", 40)}
	second := sourcecontract.Ref{Name: "refs/heads/main", ObjectSHA: strings.Repeat("c", 40), CommitSHA: strings.Repeat("d", 40)}
	current := first
	checkouts := 0
	f.source.Export = func(_ context.Context, repo string, r sourcecontract.Ref) (sourcecontract.Export, error) {
		if repo != request().Repository || r != (sourcecontract.Ref{}) {
			t.Error("default lookup used a caller or cached ref", repo, r)
		}
		checkouts++
		return sourcecontract.Export{DefaultBranch: current}, nil
	}
	f.source.Read = func(_ context.Context, r gitport.Revision, path string) (gitport.File, error) {
		if r.Repository != request().Repository || r.Commit != current.CommitSHA || path != ".oberth.yaml" {
			t.Error("file read did not use the fresh exact head", r, path)
		}
		current = second
		return gitport.File{Path: path, Bytes: []byte(r.Commit)}, nil
	}
	s := f.new(t)
	for _, want := range []sourcecontract.Ref{first, second} {
		ref, file, err := s.DefaultFile(t.Context(), request().Repository, ".oberth.yaml")
		if err != nil || ref != want || string(file.Bytes) != want.CommitSHA {
			t.Fatal("default file snapshot changed during read", ref, file, err)
		}
	}
	if checkouts != 2 {
		t.Fatal("default file cached a snapshot", checkouts)
	}
}
func TestDefaultFileKeepsKnownHeadAndDistinguishesAbsenceFromOutage(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"absent", "unavailable", "moved"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			want := request().Ref
			f.source.Export = func(context.Context, string, sourcecontract.Ref) (sourcecontract.Export, error) {
				return sourcecontract.Export{DefaultBranch: want}, nil
			}
			failure := errors.New("source could not be read")
			returned := failure
			if kind == "moved" {
				returned = publicerr.New(publicerr.Conflict, "default ref moved", "Read the fresh default head again.", failure)
			}
			f.source.Read = func(ctx context.Context, r gitport.Revision, path string) (gitport.File, error) {
				if kind == "absent" {
					return missingFile(ctx, r, path)
				}
				return gitport.File{}, returned
			}
			ref, _, err := f.new(t).DefaultFile(t.Context(), request().Repository, ".oberth.yaml")
			if ref != want || err == nil {
				t.Fatal("known default head lost", ref, err)
			}
			var absent *gitport.AbsentError
			if errors.As(err, &absent) != (kind == "absent") {
				t.Fatal("outage or movement was called absence", err)
			}
			if kind != "absent" && !errors.Is(err, failure) {
				t.Fatal("source cause lost", err)
			}
		})
	}
}
func TestDefaultFileDoesNotReadAfterDefaultLookupFails(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	outage := errors.New("default lookup unavailable")
	f.source.Export = func(context.Context, string, sourcecontract.Ref) (sourcecontract.Export, error) {
		return sourcecontract.Export{}, outage
	}
	f.source.Read = func(context.Context, gitport.Revision, string) (gitport.File, error) {
		t.Error("file read after failed default lookup")
		return gitport.File{}, nil
	}
	ref, _, err := f.new(t).DefaultFile(t.Context(), request().Repository, ".oberth.yaml")
	if ref != (gitport.Ref{}) || !errors.Is(err, outage) {
		t.Fatal(ref, err)
	}
}
func TestStarterPassesPromotionIdentityBeforeNativeCreation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	q := startRequest("promotion-receive", request())
	q.Request.Trigger = model.TriggerPromotion
	q.Request.CommitSHA = new(strings.Repeat("e", 40))
	q.Request.ObjectSHA = *q.Request.CommitSHA
	q.PromotionID = "promotion-1"
	q.Reason = "test the exact promotion result"
	promotion := model.Promotion{ID: q.PromotionID, Repo: q.Request.Repo, Actor: q.Request.Actor.Identity, Caller: q.Request.Actor.Clone(), ResultSHA: q.Request.ObjectSHA}
	if err := f.runs.PreparePromotion(t.Context(), stateport.Action{Actor: q.Request.Actor.Identity}, promotion); err != nil {
		t.Fatal(err)
	}
	create := f.native.CreateFunc
	f.native.CreateFunc = func(ctx context.Context, p *admissioncontract.PreparedAdmission) (nativebinding.RunBinding, error) {
		a, ok := f.runs.Admission(p.View().RunID)
		if !ok {
			t.Error("promotion was not accepted before Create", a)
		}
		assertPromotionAccepted(t, a, q)
		stored, err := f.runs.Promotion(ctx, q.PromotionID)
		assertPromotionBinding(t, stored, p.View().RunID, err)
		return create(ctx, p)
	}
	out, err := f.new(t).StartRun(t.Context(), q)
	if err != nil || out.State != "started" || f.prepares.Load() != 1 || f.creates.Load() != 1 {
		t.Fatal(out, err)
	}
	a, ok := f.runs.Admission(out.ID)
	if !ok || a.PromotionID != q.PromotionID || a.Prepared.View().CommitSHA != *q.Request.CommitSHA {
		t.Fatal("promotion identity or exact result lost", a)
	}
}

func assertPromotionAccepted(t *testing.T, a stateport.Acceptance, q service.StartRequest) {
	t.Helper()
	if a.PromotionID != q.PromotionID || a.Request.Trigger != model.TriggerPromotion || a.Request.CommitSHA == nil || *a.Request.CommitSHA != *q.Request.CommitSHA {
		t.Error("promotion intent changed", a)
	}
	if len(a.Actions) == 0 || !strings.Contains(a.Actions[0].Details, q.Reason) {
		t.Error("promotion reason lost", a.Actions)
	}
}

func assertPromotionBinding(t *testing.T, p model.Promotion, runID string, err error) {
	t.Helper()
	if err != nil || p.RunID != runID {
		t.Error("promotion was not durably linked before Create", p, err)
	}
}
