package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestDisplayRunStatusSeparatesPrestartAndTerminalCauses(t *testing.T) {
	started := time.Now()
	for _, tc := range []struct {
		name string
		run  model.Run
		want string
	}{
		{"unclaimed", model.Run{Status: model.RunQueued}, "queued"},
		{"claimed", model.Run{Status: model.RunRunning}, "queued"},
		{"legacy waiting", model.Run{Status: model.RunWaiting}, "queued"},
		{"started", model.Run{Status: model.RunRunning, StartedAt: &started}, "running"},
		{"superseded", model.Run{Status: model.RunInterrupted, SupersededBy: "newer"}, "superseded"},
		{"cancelled", model.Run{Status: model.RunInterrupted, Phase: model.PhaseCancelled}, "cancelled"},
		{"restart", model.Run{Status: model.RunInterrupted, Reason: "oberth restarted"}, "interrupted"},
		{"passed", model.Run{Status: model.RunPassed}, "passed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := displayRunStatus(tc.run); got != tc.want {
				t.Fatalf("display status = %q, want %q", got, tc.want)
			}
			if projected := presentationRun(tc.run); projected.DisplayStatus != tc.want {
				t.Fatalf("presentation = %+v", projected)
			}
		})
	}
}

func TestRunViewsAddDisplayStatusWithoutChangingDefaultList(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := context.Background()
	run, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: "display", Repository: fixture.repo,
		Branch: "feature/display", SHA: strings.Repeat("a", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := fixture.store.ClaimNextRun(ctx)
	if err != nil || claimed.ID != run.ID {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	service := fixture.api(t)
	actor := api.Actor{Identity: "agent@host"}
	defaultRuns, err := service.Runs(ctx, actor, api.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	defaultJSON, _ := json.Marshal(defaultRuns)
	if strings.Contains(string(defaultJSON), "display_status") {
		t.Fatalf("default list changed: %s", defaultJSON)
	}
	active, err := service.Runs(ctx, actor, api.RunFilter{View: api.RunViewActive})
	if err != nil {
		t.Fatal(err)
	}
	entry := active.([]ActiveRun)[0]
	if entry.Status != model.RunWaiting || entry.DisplayStatus != "queued" || entry.Scheduling == nil {
		t.Fatalf("active run = %+v", entry)
	}
	detailValue, err := service.Run(ctx, actor, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	detail := detailValue.(RunDetailResponse)
	if detail.Run.Status != model.RunWaiting || detail.Run.DisplayStatus != "queued" || detail.Scheduling == nil {
		t.Fatalf("detail = %+v", detail)
	}
	if _, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: "successor", Repository: fixture.repo,
		Branch: "feature/display", SHA: strings.Repeat("b", 40), Actor: "agent@host"}); err != nil {
		t.Fatal(err)
	}
	historyValue, err := service.Runs(ctx, actor, api.RunFilter{View: api.RunViewHistory})
	if err != nil {
		t.Fatal(err)
	}
	history := historyValue.([]model.Run)
	if len(history) != 1 || history[0].ID != run.ID || history[0].DisplayStatus != "superseded" ||
		history[0].SupersededBy == "" {
		t.Fatalf("history did not link successor: %+v", history)
	}
}
