package service_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestResumeBranchAndFixPreconditions(t *testing.T) {
	t.Parallel()
	for _, q := range []struct {
		name      string
		change    func(*stateport.Run)
		commit    string
		unrelated bool
		allowed   bool
	}{
		{name: "descendant head", allowed: true},
		{name: "unrelated head", unrelated: true},
		{name: "moved head", commit: strings.Repeat("c", 40)},
		{name: "abbreviated fix", commit: head[:8]},
		{name: "same ordinary commit", change: func(r *stateport.Run) { r.CommitSHA = new(head); r.ObjectSHA = head }},
		{name: "same resumed commit", allowed: true, change: func(r *stateport.Run) {
			r.CommitSHA, r.ObjectSHA, r.Origin.ResumedFrom = new(head), head, "predecessor"
		}},
		{name: "default branch", change: func(r *stateport.Run) { r.Ref = "refs/heads/main" }},
		{name: "tag", change: func(r *stateport.Run) { r.Ref = "refs/tags/v1.0.0" }},
		{name: "release", change: func(r *stateport.Run) { r.Release = true }},
		{name: "schedule", change: func(r *stateport.Run) { r.Origin.Trigger = model.TriggerSchedule }},
		{name: "schedule entry", change: func(r *stateport.Run) { r.Origin.ScheduleEntry = "nightly" }},
		{name: "cancelled", change: func(r *stateport.Run) { r.Status = stepmodel.RunCancelled }},
		{name: "passed", change: func(r *stateport.Run) { r.Status = stepmodel.RunPassed }},
		{name: "running", change: func(r *stateport.Run) { r.Status = stepmodel.RunRunning }},
		{name: "no commit", change: func(r *stateport.Run) { r.CommitSHA = nil }},
	} {
		t.Run(q.name, func(t *testing.T) {
			f := setup(t)
			r := f.seed(stepmodel.RunFailed)
			r.CommitSHA, r.ObjectSHA = new(strings.Repeat("b", 40)), strings.Repeat("b", 40)
			if q.change != nil {
				q.change(&r)
			}
			f.store.native.SeedRun(r)
			f.c.Git = resumeGit{f.c.Git, !q.unrelated}
			spy := &resumeStarterSpy{Starter: f.c.Starter}
			f.c.Starter = spy
			f.restart(t)
			commit := q.commit
			if commit == "" {
				commit = head
			}
			call, err := f.s.PrepareRunResume(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.RunResumeArgs{ID: r.ID, Commit: commit})
			if err == nil {
				_, err = call(t.Context())
			}
			checkResumePreconditionResult(t, q.allowed, spy.calls, err)
		})
	}
}

func checkResumePreconditionResult(t *testing.T, allowed bool, calls int, err error) {
	t.Helper()
	if allowed {
		if err != nil || calls != 1 {
			t.Fatal("valid branch fix refused", err, calls)
		}
	} else if err == nil || calls != 0 {
		t.Fatal("invalid branch fix reached resume", err, calls)
	}
}
