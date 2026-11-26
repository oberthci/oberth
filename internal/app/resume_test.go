package app_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/app/fake"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"strings"
	"testing"
)

type resumeNativeStub struct {
	fake.Native
	applied bool
	runs    *runFixture
}

func (n *resumeNativeStub) ResumeFailed(ctx context.Context, id string, _ error) error {
	r, err := n.runs.Run(ctx, id)
	if err != nil {
		return err
	}
	return n.runs.Finish(ctx, stateport.Action{}, stateport.RunUpdate{ID: id, Revision: r.Revision, Status: stepmodel.RunFailed, Reason: "resume could not start"})
}

func (n *resumeNativeStub) PrepareResume(_ context.Context, old stateport.Run, id, commit string) (nativebinding.ResumePlan, error) {
	w := old.Admission.Workflow.DeepCopy()
	w.Name = old.Origin.WorkflowName
	w.Labels = map[string]string{nativebinding.RunLabel: id, nativebinding.CommitLabel: commit}
	return nativebinding.ResumePlan{Workflow: w, UID: "uid-r1", ResourceVersion: "7", Carried: []string{"a"}}, nil
}
func (n *resumeNativeStub) ApplyResume(ctx context.Context, id string) error {
	r, err := n.runs.Run(ctx, id)
	if err != nil {
		return err
	}
	if !r.Origin.Assisted || r.Origin.ResumedFrom != "r1" {
		panic("retry reached before Assisted insert")
	}
	n.applied = true
	return nil
}

func TestResumeAcceptsBeforeNativeEffectAndKeepsR1Immutable(t *testing.T) {
	t.Parallel()
	f, s, n, old := appResumeFixture(t)
	out, err := s.ResumeRun(t.Context(), service.ResumeRequest{Run: old, Actor: request().Actor, ID: "resume", Commit: strings.Repeat("f", 40)})
	if err != nil || out.Run != "r2" || !n.applied {
		t.Fatal(out, err)
	}
	before, _ := json.Marshal(old)
	after, _ := f.runs.Run(t.Context(), "r1")
	body, _ := json.Marshal(after)
	if string(before) != string(body) {
		t.Fatal("predecessor changed")
	}
	r, err := f.runs.Run(t.Context(), out.Run)
	if err != nil {
		t.Fatal(err)
	}
	if r.Origin.NativeUID() != "uid-r1" || r.Origin.WorkflowUID != "" || r.Admission.CommitSHA != strings.Repeat("f", 40) {
		t.Fatal(r)
	}
	checkResumedFullRerun(t, f, s, r)
}

func checkResumedFullRerun(t *testing.T, f *fixture, s *app.Submitter, r stateport.Run) {
	t.Helper()
	q := startRequest("full", request())
	q.RerunOf = r.ID
	q.Request.ObjectSHA = strings.Repeat("f", 40)
	q.Request.CommitSHA = &q.Request.ObjectSHA
	receipt, err := s.StartRun(t.Context(), q)
	if err != nil {
		t.Fatal("full rerun refused", err)
	}
	rf, err := f.runs.Run(t.Context(), receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rf.Origin.Resumed() || rf.Origin.Assisted || rf.Origin.NativeUID() == r.Origin.NativeUID() {
		t.Fatal("rerun retained assistance or identity", rf.Origin)
	}
	found := false
	for _, param := range rf.Admission.Workflow.Spec.Arguments.Parameters {
		if param.Name == "oberth-source" {
			found = true
			if param.Value == nil || param.Value.String() != rf.ID {
				t.Fatal("rerun locator not restamped", param)
			}
		}
	}
	if !found {
		t.Fatal("fresh locator missing")
	}

}

func appResumeFixture(t *testing.T) (*fixture, *app.Submitter, *resumeNativeStub, stateport.Run) {
	t.Helper()
	f := newFixture(t)
	w := workflow()
	addSource(w, sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main})
	f.setDocument(w)
	s := f.new(t)
	if _, err := s.StartRun(t.Context(), startRequest("old", request())); err != nil {
		t.Fatal(err)
	}
	old, err := f.runs.Run(t.Context(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.runs.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: old.ID, Revision: old.Revision, Status: stepmodel.RunFailed}); err != nil {
		t.Fatal(err)
	}
	old, err = f.runs.Run(t.Context(), "r1")
	if err != nil {
		t.Fatal(err)
	}
	n := &resumeNativeStub{Native: f.native, runs: f.runs}
	f.d.Native = n
	f.d.Resolver = fake.StartResolver{Resolve: func(ctx context.Context, q service.StartRequest) (app.Request, error) {
		r, e := resolveStart(ctx, q)
		r.RunID = "r2"
		if q.RerunOf == "r2" {
			r.RunID = "rf"
		}
		return r, e
	}}
	s, err = app.New(f.d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return f, s, n, old
}

func TestResumeRetentionFailureClosesAcceptedSuccessor(t *testing.T) {
	t.Parallel()
	f, s, n, old := appResumeFixture(t)
	f.keeper.KeepFunc = func(context.Context, string, []sourcecontract.Binding) error {
		return errors.New("retention unavailable")
	}
	out, err := s.ResumeRun(t.Context(), service.ResumeRequest{Run: old, Actor: request().Actor, ID: "resume", Commit: strings.Repeat("f", 40)})
	if err == nil || out.Run == "" || n.applied {
		t.Fatal("retention failure dispatched retry", out, err)
	}
	r, readErr := f.runs.Run(t.Context(), out.Run)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if r.Status != stepmodel.RunFailed || !r.Origin.Assisted {
		t.Fatal("retention failure left accepted successor active", r.Status, r.Origin)
	}
}
