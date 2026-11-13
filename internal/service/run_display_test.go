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
	"github.com/oberthci/oberth/internal/storetest"
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

		{"cancelled", model.Run{Status: model.RunInterrupted, Phase: model.PhaseCancelled}, "cancelled"},
		{"restart", model.Run{Status: model.RunInterrupted, Reason: "oberth restarted"}, "cancelled"},
		{"passed", model.Run{Status: model.RunPassed}, "passed"},
		{"passed release", model.Run{Status: model.RunPassed, Release: true}, "passed"},
		{"infrastructure error", model.Run{Status: model.RunFailed, Error: "controller unavailable"}, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := model.DisplayRunStatus(tc.run); got != tc.want {
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
	service := &API{}
	preflight := service.presentationRun(t.Context(), model.Run{ID: "preflight", Status: model.RunRunning, Phase: "preflight", StartedAt: &started})
	if preflight.DisplayStatus != "running" || preflight.Phase != "preflight" {
		t.Fatalf("trusted preflight presentation = %+v", preflight)
	}
	if scheduling, _ := service.observeScheduling(t.Context(), preflight, nil, true); scheduling == nil || scheduling.State != "preflight" {
		t.Fatalf("trusted preflight scheduling = %+v", scheduling)
	}
	for _, tc := range []struct {
		run  model.Run
		want string
	}{
		{model.Run{Status: model.RunInterrupted, Phase: model.PhaseCancelled, Reason: "cancelled by owner: no longer needed"}, "by owner: no longer needed"},
		{model.Run{Status: model.RunInterrupted, Reason: "oberth restarted"}, "server restart, retry needed"},
		{model.Run{Status: model.RunFailed, Error: "controller unavailable"}, "infrastructure error: controller unavailable"},
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
	entry := defaultRuns.([]model.Run)[0]
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
	if len(history) != 0 {
		t.Fatalf("newer push terminalized an earlier run: %+v", history)
	}

}

func TestRestartResetDisplaysQueuedUnderOriginalID(t *testing.T) {
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
	if run.DisplayStatus != "queued" || run.Reason != store.RestartRequeueReason || run.SupersededBy != "" || replacement.ID != original.ID {
		t.Fatalf("restart presentation = %+v", run)
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
	// The claimed run is first in claim order, at position 1.
	runs := value.([]model.Run)
	if len(runs) != 2 || runs[0].ID != first.ID || runs[1].ID != second.ID {
		t.Fatalf("queue order = %+v", runs)
	}
	for i, run := range runs {
		if want := int64(i + 1); run.DisplayStatus != "queued" || run.Scheduling == nil || run.Scheduling.QueuePosition != want || run.DisplayReason == "" {
			t.Fatalf("queue position %d = %+v", want, run)
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
	for _, view := range []string{""} {
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
	enqueued, _, err := storetest.EnqueuePromotionRun(f.store, ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "promotion/main/bbbbbbbbbbbb", SHA: strings.Repeat("b", 40), Actor: "agent", Trigger: "promotion", TestedSHA: strings.Repeat("b", 40), BaseSHA: strings.Repeat("a", 40)}, model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: "feature/restart", SourceSHA: strings.Repeat("b", 40), TargetRef: "main", PreviousSHA: strings.Repeat("a", 40), ResultSHA: strings.Repeat("b", 40), Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.MarkRunSubmissionStarted(ctx, enqueued.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := storetest.Open(ctx, filepath.Join(f.root, "oberth.sqlite"), store.Options{})
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
