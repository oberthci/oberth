package service_test

import (
	"context"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	ports "github.com/oberthci/oberth/internal/service/contracttest"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"strings"
	"testing"
)

type resumeGit struct {
	service.Git
	ancestor bool
}

func (g resumeGit) ResumeAncestor(ctx context.Context, _, _, _ string) (bool, error) {
	return g.ancestor, ctx.Err()
}

type resumeStarterSpy struct {
	service.Starter
	calls int
}

func (s *resumeStarterSpy) ResumeRun(ctx context.Context, q service.ResumeRequest) (apicontract.ResumeResult, error) {
	s.calls++
	return apicontract.ResumeResult{Run: q.ID, Mode: "retry"}, ctx.Err()
}

func TestResumeRequiresFreshPushAuthority(t *testing.T) {
	t.Parallel()
	for _, deny := range []bool{false, true} {
		f := setup(t)
		r := f.seed(stepmodel.RunFailed)
		old := strings.Repeat("b", 40)
		r.CommitSHA = &old
		r.ObjectSHA = old
		f.store.native.SeedRun(r)
		f.c.Git = resumeGit{f.c.Git, true}
		spy := &resumeStarterSpy{Starter: f.c.Starter}
		f.c.Starter = spy
		calls := 0
		f.c.Authority = ports.AuthorityFuncs{KeyPolicyFunc: f.store.KeyPolicy, CurrentFunc: func(ctx context.Context, a actor.Actor) (actor.Actor, error) {
			calls++
			if deny && calls == 3 {
				return actor.Actor{}, publicerr.New(publicerr.Forbidden, "Push is not allowed.", "Ask the repository administrator for access.", nil)
			}
			return f.store.Current(ctx, a)
		}}
		f.restart(t)
		call, err := f.s.PrepareRunResume(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunResumeArgs{ID: r.ID, Commit: head})
		if err == nil {
			_, err = call(t.Context())
		}
		if deny {
			if err == nil || spy.calls != 0 {
				t.Fatal("resume bypassed fresh push authority", err, spy.calls)
			}
		} else if err != nil || spy.calls != 1 {
			t.Fatal("authorized resume refused", err, spy.calls)
		}
	}
}

func TestPassedResumeRerunsAsOrdinary(t *testing.T) {
	t.Parallel()
	f := setup(t)
	r := f.resumed(stepmodel.RunPassed, runID)
	f.starter.Start = func(_ context.Context, q service.StartRequest) (apicontract.Receipt, error) {
		if q.Request.ObjectSHA != head || q.Request.Assist != r.Origin.AssistRequested || q.RerunOf != r.ID {
			t.Fatal("rerun lost source", q)
		}
		return apicontract.Receipt{ID: "fresh"}, nil
	}
	call, err := f.s.PrepareRunRerun(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunRerunArgs{ID: r.ID})
	if err == nil {
		_, err = call(t.Context())
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestResumePromotionRemedyAndFinishedData(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.resumed(stepmodel.RunRunning, runID)
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: resumedID, Status: stepmodel.RunPassed}); err != nil {
		t.Fatal(err)
	}
	call, err := f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteArgs{Repo: repository, SHA: head, Reason: "land"})
	if err == nil {
		_, err = call(t.Context())
	}
	if err == nil || !strings.Contains(publicerr.Classify(err).Remedy(), "run_rerun "+resumedID) {
		t.Fatal("promotion did not explain the full run", err)
	}
	page, err := f.store.Events(t.Context(), stateport.EventFilter{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range page.Events {
		if event.RunID == resumedID && event.Kind == "run.finished" {
			if !strings.Contains(string(event.Data), runID) {
				t.Fatal("finished event lost predecessor", string(event.Data))
			}
			return
		}
	}
	t.Fatal("finished event missing")
}
