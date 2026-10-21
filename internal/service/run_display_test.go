package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/vmrunner"
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
		{"superseded", model.Run{Status: model.RunInterrupted, SupersededBy: "newer"}, "cancelled"},
		{"cancelled", model.Run{Status: model.RunInterrupted, Phase: model.PhaseCancelled}, "cancelled"},
		{"restart", model.Run{Status: model.RunInterrupted, Reason: "oberth restarted"}, "cancelled"},
		{"passed", model.Run{Status: model.RunPassed}, "passed"},
		{"delivered release", model.Run{Status: model.RunPassed, Release: true}, "delivered"},
		{"infrastructure error", model.Run{Status: model.RunFailed, Error: "controller unavailable"}, "failed"},
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

func TestRunDisplayReasonsCoverTerminalCausesAndTrustedPreflight(t *testing.T) {
	started := time.Now().UTC()
	service := &API{kvmPreflights: kvmReadFake{value: vmrunner.KVMExecution{Submitted: true}}}
	preflight := service.presentationRun(t.Context(), model.Run{ID: "preflight", Status: model.RunRunning, StartedAt: &started})
	if preflight.DisplayStatus != "running" || preflight.Phase != "preflight" {
		t.Fatalf("trusted preflight presentation = %+v", preflight)
	}
	if scheduling := service.scheduling(t.Context(), preflight, nil); scheduling == nil || scheduling.State != "preflight" {
		t.Fatalf("trusted preflight scheduling = %+v", scheduling)
	}
	for _, tc := range []struct {
		run  model.Run
		want string
	}{
		{model.Run{Status: model.RunInterrupted, Phase: model.PhaseCancelled, Reason: "cancelled by owner: no longer needed"}, "cancelled by owner: no longer needed"},
		{model.Run{Status: model.RunInterrupted, Reason: "oberth restarted"}, "server restart, retry needed"},
		{model.Run{Status: model.RunFailed, Error: "controller unavailable"}, ""},
	} {
		if got := presentationRun(tc.run).DisplayReason; got != tc.want {
			t.Fatalf("display reason for %+v = %q, want %q", tc.run, got, tc.want)
		}
	}
}

func TestRunViewsAddDisplayStatusAndReasonToDefaultList(t *testing.T) {
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
	if !strings.Contains(string(defaultJSON), `"display_status":"queued"`) ||
		!strings.Contains(string(defaultJSON), `"display_reason":`) {
		t.Fatalf("default list lacks display fields: %s", defaultJSON)
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
	status, err := service.statusForRun(ctx, fixture.repo, claimed)
	if err != nil || status.DisplayStatus != "queued" || status.DisplayReason == "" || status.Run.DisplayReason != status.DisplayReason {
		t.Fatalf("status display pair = %+v, %v", status, err)
	}
	logValue, err := service.RunLogLive(ctx, actor, run.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	log := logValue.(LiveLogResponse)
	if log.DisplayStatus != "queued" || log.DisplayReason == "" {
		t.Fatalf("live log display pair = %+v", log)
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
	if len(history) != 1 || history[0].ID != run.ID || history[0].DisplayStatus != "cancelled" ||
		history[0].SupersededBy == "" || history[0].DisplayReason != "superseded by bbbbbbbb" {
		t.Fatalf("history did not link successor: %+v", history)
	}
}

func TestRestartRefireDisplaysCancelledWithSuccessor(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := context.Background()
	original, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: "restart-display", Repository: fixture.repo,
		Branch: "feature/restart", SHA: strings.Repeat("c", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	replacement, err := fixture.store.RequeueStrandedRun(ctx, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	value, err := fixture.api(t).Run(ctx, api.Actor{Identity: "agent@host"}, original.ID)
	if err != nil {
		t.Fatal(err)
	}
	run := value.(RunDetailResponse).Run
	want := "server restart, re-queued as " + shortRunID(replacement.ID)
	if run.DisplayStatus != "cancelled" || run.DisplayReason != want || run.SupersededBy != replacement.ID {
		t.Fatalf("restart presentation = %+v, want %q", run, want)
	}
}

func TestDefaultRunListNumbersClaimedAndUnclaimedTogether(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := t.Context()
	first, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: "first-position", Repository: fixture.repo,
		Branch: "feature/first", SHA: strings.Repeat("d", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := fixture.store.ClaimNextRun(ctx)
	if err != nil || claimed.ID != first.ID {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	second, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: "second-position", Repository: fixture.repo,
		Branch: "feature/second", SHA: strings.Repeat("e", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}
	value, err := fixture.api(t).Runs(ctx, api.Actor{Identity: "agent@host"}, api.RunFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	runs := value.([]model.Run)
	if len(runs) != 2 || runs[0].ID != first.ID || runs[1].ID != second.ID {
		t.Fatalf("queue order = %+v", runs)
	}
	for i, run := range runs {
		if run.DisplayStatus != "queued" || run.Scheduling == nil || run.Scheduling.QueuePosition != int64(i+1) || run.DisplayReason == "" {
			t.Fatalf("queue position %d = %+v", i+1, run)
		}
	}
}
