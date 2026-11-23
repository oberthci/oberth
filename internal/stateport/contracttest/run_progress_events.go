package contracttest

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func checkProgressEvents(t *testing.T, f RunFixture) {
	r := acceptOneRun(t, f, RunCase{ID: "progress-events"})
	entry := r.Admission.Plan.Entries[0]
	initial, err := f.Port.Events(t.Context(), stateport.EventFilter{Limit: 500})
	mustRun(t, err)
	after, audit := initial.LatestID, len(f.Audit(t))
	started := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	step := stepmodel.RunStep{Path: entry.Path, Name: entry.Name, Status: stepmodel.StepQueued}
	for phase := range 5 {
		p := stateport.Progress{Steps: []stepmodel.RunStep{step}, Waiting: "waiting for a run slot"}
		if phase > 0 {
			p.Waiting = ""
			p.Steps[0].Status, p.Steps[0].StartedAt = stepmodel.StepRunning, &started
		}
		if phase == 2 {
			p.Steps[0].DisplayReason = "waiting for a node"
		}
		if phase == 4 {
			p.Steps[0].Status, p.Steps[0].FinishedAt = stepmodel.StepPassed, &started
		}
		changed := f.Port.Changes()
		mustRun(t, f.Port.SaveProgress(t.Context(), r.ID, p))
		changedSignal(t, changed)
		page, err := f.Port.Events(t.Context(), stateport.EventFilter{After: after, Limit: 500})
		mustRun(t, err)
		wantNotice := model.AgentEvent{Kind: string(model.EventRunUpdated), RunID: r.ID, Repo: r.Repo, Ref: r.Ref, Summary: p.Waiting}
		checkProgressNotice(t, page, wantNotice, after)
		after = page.Events[0].ID
		saved, err := f.Port.Run(t.Context(), r.ID)
		mustRun(t, err)
		want := stepmodel.RunRunning
		if phase == 0 {
			want = stepmodel.RunQueued
		}
		if saved.Revision != r.Revision || saved.Status != want || len(f.Audit(t)) != audit {
			t.Fatal("progress changed control state or missed the run start", saved)
		}
		changed = f.Port.Changes()
		mustRun(t, f.Port.SaveProgress(t.Context(), r.ID, p))
		unchangedSignal(t, changed)
		replay, err := f.Port.Events(t.Context(), stateport.EventFilter{After: after, Limit: 500})
		mustRun(t, err)
		if len(replay.Events) != 0 {
			t.Fatal("unchanged projection repeated its notice", replay)
		}
	}
}

func checkProgressNotice(t *testing.T, page model.EventPage, want model.AgentEvent, after int64) {
	t.Helper()
	if len(page.Events) != 1 || page.Events[0].Kind != want.Kind || page.Events[0].RunID != want.RunID || page.Events[0].Repo != want.Repo || page.Events[0].Ref != want.Ref || page.Events[0].ID <= after {
		t.Fatal("changed projection did not commit one scoped notice", page)
	}
	if want.Summary != "" && page.Events[0].Summary != want.Summary {
		t.Fatal("queued reason was not retained in history", page)
	}
}
