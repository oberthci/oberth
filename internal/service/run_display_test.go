package service

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
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
		{"superseded", model.Run{Status: model.RunInterrupted, SupersededBy: "newer"}, "superseded"},
		{"cancelled", model.Run{Status: model.RunInterrupted, Phase: model.PhaseCancelled}, "cancelled"},
		{"restart", model.Run{Status: model.RunInterrupted, Reason: "oberth restarted"}, "cancelled"},
		{"passed", model.Run{Status: model.RunPassed}, "passed"},
		{"passed release", model.Run{Status: model.RunPassed, Release: true}, "passed"},
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
	if len(history) != 1 || history[0].ID != run.ID || history[0].DisplayStatus != "superseded" ||
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
	if _, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: "restart-other", Repository: fixture.repo, Branch: "feature/other", SHA: strings.Repeat("b", 40), Actor: "agent@host"}); err != nil {
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
	if run.DisplayStatus != "superseded" || run.DisplayReason != want || run.SupersededBy != replacement.ID {
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
	service := fixture.api(t)
	actor := api.Actor{Identity: "agent@host"}
	value, err := service.Runs(ctx, actor, api.RunFilter{Limit: 1})
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
	filteredValue, err := service.Runs(ctx, actor, api.RunFilter{Ref: second.Ref})
	if err != nil {
		t.Fatal(err)
	}
	filtered := filteredValue.([]model.Run)
	if len(filtered) != 1 || filtered[0].ID != second.ID || filtered[0].Scheduling == nil || filtered[0].Scheduling.QueuePosition != 2 {
		t.Fatalf("filtered list lost its global queue position: %+v", filtered)
	}
}

type countingSuccessorHistory struct {
	RunHistory
	batchCalls int
	runCalls   int
}

func (history *countingSuccessorHistory) RunsByIDs(ctx context.Context, ids []string) (map[string]model.Run, error) {
	history.batchCalls++
	return history.RunHistory.(interface {
		RunsByIDs(context.Context, []string) (map[string]model.Run, error)
	}).RunsByIDs(ctx, ids)
}

func (history *countingSuccessorHistory) Run(ctx context.Context, id string) (model.Run, error) {
	history.runCalls++
	return history.RunHistory.Run(ctx, id)
}

func TestDefaultRunListBatchesSuccessorLookups(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := t.Context()
	for _, branch := range []string{"feature/one", "feature/two"} {
		if _, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: branch + "-old", Repository: fixture.repo,
			Branch: branch, SHA: strings.Repeat("a", 40), Actor: "agent@host"}); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: branch + "-new", Repository: fixture.repo,
			Branch: branch, SHA: strings.Repeat("b", 40), Actor: "agent@host"}); err != nil {
			t.Fatal(err)
		}
	}
	service := fixture.api(t)
	history := &countingSuccessorHistory{RunHistory: service.history}
	service.history = history
	value, err := service.Runs(ctx, api.Actor{Identity: "agent@host"}, api.RunFilter{})
	if err != nil {
		t.Fatal(err)
	}
	runs := value.([]model.Run)
	if len(runs) != 4 || history.batchCalls != 1 || history.runCalls != 0 {
		t.Fatalf("list did not batch successors: %d runs, %d batches, %d single lookups", len(runs), history.batchCalls, history.runCalls)
	}
	for _, run := range runs {
		if run.SupersededBy != "" && run.DisplayReason != "superseded by bbbbbbbb" {
			t.Fatalf("successor reason = %+v", run)
		}
	}
}

func TestDisplayReasonUsesDurableCauseNotSHAOrTextHeuristics(t *testing.T) {
	successor := model.Run{ID: strings.Repeat("b", 32), SHA: strings.Repeat("a", 40), Ref: "same"}
	service := &API{}
	for _, tc := range []struct{ reason, phase, want string }{
		{"superseded by newer run", "interrupted", "superseded by aaaaaaaa"},
		{"re-queued after restart", "interrupted", "server restart, re-queued as bbbbbbbbbbbb"},
		{store.KVMRestartRetryReason, "interrupted", "server restart, re-queued as bbbbbbbbbbbb"},
		{"cancelled by owner: restart later", "cancelled", "cancelled by owner: restart later"},
		{"scheduler stopped", "interrupted", "server restart, retry needed"},
		{"unrelated reason", "interrupted", "server restart, retry needed"},
	} {
		run := model.Run{Status: model.RunInterrupted, Reason: tc.reason, Phase: tc.phase, SupersededBy: successor.ID, SHA: successor.SHA, Ref: successor.Ref}
		got := service.presentationRunWithSuccessor(t.Context(), run, map[string]model.Run{successor.ID: successor})
		wantStatus := "superseded"
		if tc.phase == model.PhaseCancelled {
			wantStatus = "cancelled"
		}
		if got.DisplayStatus != wantStatus || got.DisplayReason != tc.want {
			t.Fatalf("%s: %+v", tc.reason, got)
		}
	}
	for _, phase := range []string{"waiting", "preflight", "running", "publishing"} {
		now := time.Now()
		run := model.Run{Status: model.RunRunning, Phase: phase, StartedAt: &now}
		if got := presentationRun(run); got.DisplayStatus != "running" || got.DisplayReason != "" {
			t.Fatalf("phase %s: %+v", phase, got)
		}
		run.StartedAt = nil
		if got := presentationRun(run); got.DisplayStatus != "queued" {
			t.Fatalf("unadmitted phase %s: %+v", phase, got)
		}
	}
}

func TestScopedDisplayDoesNotLinkHiddenSuccessor(t *testing.T) {
	service := &API{scope: &repoScope{}}
	for _, reason := range []string{"superseded by newer run", "re-queued after restart", store.KVMRestartRetryReason} {
		run := model.Run{Status: model.RunInterrupted, Reason: reason, SupersededBy: strings.Repeat("b", 32)}
		got := service.presentationRunWithSuccessor(t.Context(), run, nil)
		if got.SupersededBy != "" || strings.Contains(got.DisplayReason, "bbbb") || got.DisplayStatus != "superseded" {
			t.Fatalf("hidden successor leaked: %+v", got)
		}
		if reason == store.KVMRestartRetryReason && got.DisplayReason != "server restart, re-queued" {
			t.Fatalf("hidden trusted retry reason = %q", got.DisplayReason)
		}
	}
}

type listPositionGuard struct {
	RunResolver
	calls int
}

func (guard *listPositionGuard) QueuedRunPosition(context.Context, string) (int64, error) {
	guard.calls++
	return 999, nil
}

func TestRunListsUseStorePositionsWithoutPerRowQueries(t *testing.T) {
	fixture := newControlFixture(t)
	for _, branch := range []string{"one", "two"} {
		if _, err := fixture.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: branch, Repository: fixture.repo, Branch: branch, SHA: strings.Repeat("a", 40), Actor: "agent"}); err != nil {
			t.Fatal(err)
		}
	}
	reader := fixture.api(t)
	guard := &listPositionGuard{RunResolver: reader.runs}
	reader.runs = guard
	for _, view := range []string{"", api.RunViewActive} {
		if _, err := reader.Runs(t.Context(), api.Actor{Identity: "agent"}, api.RunFilter{View: view}); err != nil {
			t.Fatal(err)
		}
	}
	if guard.calls != 0 {
		t.Fatalf("list performed %d per-row position reads", guard.calls)
	}
	counts, err := reader.RunCounts(t.Context(), api.Actor{Identity: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if got := counts.(model.RunCounts); got.Total != 2 || got.Queued != 2 || got.Running != 0 || got.QueuedByRepo[fixture.repo.ID] != 2 {
		t.Fatalf("counts %+v", got)
	}
}

func TestPromotionRestartDisplaysRetryNeededWithoutCIIssue(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	enqueued, _, err := f.store.EnqueuePromotionRun(ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/bbbbbbbbbbbb", SHA: strings.Repeat("b", 40), Actor: "agent", Trigger: "promotion", TestedSHA: strings.Repeat("b", 40), BaseSHA: strings.Repeat("a", 40)}, model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: "feature/restart", SourceSHA: strings.Repeat("b", 40), TargetRef: "main", PreviousSHA: strings.Repeat("a", 40), ResultSHA: strings.Repeat("b", 40), Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Open(ctx, filepath.Join(f.root, "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	run, err := recovered.Run(ctx, enqueued.ID)
	if err != nil {
		t.Fatal(err)
	}
	shown := presentationRun(run)
	if shown.DisplayStatus != "cancelled" || shown.DisplayReason != "server restart, retry needed" || shown.SupersededBy != "" {
		t.Fatalf("restart promotion %+v", shown)
	}
	if _, err = recovered.OpenCIIssue(ctx, f.repo.ID, "feature/restart"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("restart CI issue: %v", err)
	}
}
