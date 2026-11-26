package service_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

const resumedID = "abcdef01000000000000000000000002"
const resumedReason = "resumed run; not evidence"

// resumed seeds R2, newer than the fixture's run: it resumed R1 on the same
// commit and is Assisted from its insert.
func (f *fixture) resumed(status stepmodel.RunStatus, from string) stateport.Run {
	commit := head
	r := stateport.Run{RunSummary: stepmodel.RunSummary{ID: resumedID, Repo: repository, Ref: "refs/heads/topic", ObjectSHA: head, CommitSHA: &commit, Status: status, CreatedAt: at()}, Revision: 1, Sequence: 2}
	r.Origin.Actor, r.Origin.Trigger, r.Origin.Assisted, r.Origin.ResumedFrom = caller().Identity, "branch", true, from
	f.store.native.SeedRun(r)
	return r
}

// A resumed run neither gives nor blocks evidence: RF promotes even while a
// newer resumed run passed or failed. The control is an ordinary assisted run,
// which still blocks.
func TestResumeNeverEvidence(t *testing.T) {
	t.Parallel()
	for _, status := range []stepmodel.RunStatus{stepmodel.RunPassed, stepmodel.RunFailed} {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			f := setup(t)
			rf := f.seed(stepmodel.RunPassed)
			f.resumed(status, "abcdef01000000000000000000000009")
			out := promote(t, f)
			if out.TestedRunID != rf.ID {
				t.Fatal("resumed run shadowed the full run", out)
			}
		})
	}
	f := setup(t)
	f.seed(stepmodel.RunPassed)
	f.resumed(stepmodel.RunPassed, "")
	call, err := f.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteArgs{Repo: repository, SHA: head, Reason: "land"})
	if err == nil {
		_, err = call(t.Context())
	}
	requireCode(t, err, publicerr.Forbidden)
	only := setup(t)
	only.resumed(stepmodel.RunPassed, "abcdef01000000000000000000000009")
	call, err = only.s.PreparePromote(t.Context(), apicontract.Caller{Actor: caller()}, apicontract.PromoteArgs{Repo: repository, SHA: head, Reason: "land"})
	if err == nil {
		_, err = call(t.Context())
	}
	if err == nil {
		t.Fatal("a passed resumed run alone promoted")
	}
}

// Every surface names a resumed run as such, passed included; selection by
// commit or ref never picks it, its own ID does.
func TestResumedRunDisplay(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.seed(stepmodel.RunFailed)
	f.resumed(stepmodel.RunPassed, runID)
	got, err := f.s.RunGet(t.Context(), caller(), viewQuery{ID: resumedID})
	if err != nil || got.Status != stepmodel.RunPassed || got.DisplayReason != resumedReason {
		t.Fatal("run_get", got.RunSummary, err)
	}
	page, err := f.s.RunList(t.Context(), caller(), viewQuery{Repo: repository, Limit: 10})
	if err != nil || len(page.Runs) != 2 || page.Runs[0].DisplayReason != resumedReason || page.Runs[1].DisplayReason == resumedReason {
		t.Fatal("run_list", page, err)
	}
	checkSelectionSkipsResumed(t, f)
	if out := waitRun(t, f, apicontract.WaitArgs{ID: resumedID}); out.ID != resumedID || out.DisplayReason != resumedReason {
		t.Fatal("wait by id", out)
	}
}

// checkSelectionSkipsResumed: status and wait by ref or commit pick R1.
func checkSelectionSkipsResumed(t *testing.T, f *fixture) {
	t.Helper()
	for _, q := range []viewQuery{{Repo: repository, Ref: "topic"}, {Repo: repository, SHA: head[:12]}} {
		s, err := f.s.Status(t.Context(), caller(), q)
		if err != nil || s.ID != runID {
			t.Fatal("status selected the resumed run", q, s.ID, err)
		}
	}
	for _, q := range []apicontract.WaitArgs{{Repo: repository, SHA: head}, {Repo: repository, Ref: "topic"}} {
		if out := waitRun(t, f, q); out.ID != runID {
			t.Fatal("wait selected the resumed run", q, out.ID)
		}
	}
}

func waitRun(t *testing.T, f *fixture, q apicontract.WaitArgs) stepmodel.RunSummary {
	t.Helper()
	call, err := f.s.Service.PrepareWait(t.Context(), apicontract.Caller{Actor: caller()}, q)
	out, err := execute(t.Context(), call, err)
	if err != nil || out.Run == nil {
		t.Fatal("wait", q, err)
	}
	return *out.Run
}

// A resumed run's verdict carries its reason, and its failure hint is never
// rerun: an unassisted run of that commit may be active.
func TestResumedRunFinishHint(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.resumed(stepmodel.RunRunning, runID)
	failure := &stepmodel.FailureRecord{Class: stepmodel.FailureInfrastructure, Step: "build", Retry: stepmodel.RetryHint{Action: stepmodel.RetryRerun, ReasonCode: "node"}}
	if err := f.s.Finished(t.Context(), service.FinishedRun{RunID: resumedID, Status: stepmodel.RunFailed, Reason: "node shut down", Failure: failure}); err != nil {
		t.Fatal(err)
	}
	r, err := f.store.Run(t.Context(), resumedID)
	if err != nil || r.DisplayReason != resumedReason || r.Failure == nil || r.Failure.Retry.Action != stepmodel.RetryNone || failure.Retry.Action != stepmodel.RetryRerun {
		t.Fatal("finish", r.RunSummary, r.Failure, err)
	}
	if summaries := finishedSummaries(t, f, resumedID); len(summaries) != 1 || !strings.Contains(summaries[0], "resumed run") {
		t.Fatal("finished events", summaries)
	}
}

func finishedSummaries(t *testing.T, f *fixture, id string) []string {
	t.Helper()
	events, err := f.store.Events(t.Context(), stateport.EventFilter{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, e := range events.Events {
		if e.RunID == id && e.Kind == "run.finished" {
			out = append(out, e.Summary)
		}
	}
	return out
}
