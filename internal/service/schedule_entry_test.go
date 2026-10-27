package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestScheduleEntryEnqueuePersistsTheResolvedName(t *testing.T) {
	f := newRecoveryFixture(t)
	scheduler := f.scheduler(t, &recoveryJobs{})
	for _, entry := range []string{"nightly", "weekly-sweep"} {
		run, err := scheduler.EnqueueCI(context.Background(), CIRequest{EventID: "scheduled-" + entry, Repository: f.repo, Branch: "main", SHA: strings.Repeat("a", 40), Actor: "oberth:scheduler", Trigger: "schedule", ScheduleEntry: entry})
		if err != nil {
			t.Fatal(err)
		}
		stored, err := f.store.Run(context.Background(), run.ID)
		if err != nil || stored.ScheduleEntry != entry || stored.Trigger != "schedule" {
			t.Fatalf("schedule name lost: %+v %v", stored, err)
		}
	}
	runs, err := f.store.ListRecentRuns(context.Background(), model.RunListFilter{RepoID: f.repo.ID, Limit: 10})
	if err != nil || len(runs) != 2 || runs[0].Status != model.RunQueued || runs[1].Status != model.RunQueued {
		t.Fatalf("schedule entries superseded: %+v %v", runs, err)
	}
}

func TestScheduleEntryIsVisibleThroughHTTPAndMCP(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	reader := f.api(t)
	run, err := f.scheduler.EnqueueCI(ctx, CIRequest{EventID: "schedule-api", Repository: f.repo, Branch: "main", SHA: strings.Repeat("a", 40), Actor: "oberth:scheduler", Trigger: "schedule", ScheduleEntry: "nightly"})
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.New(runSurfaceAuth{}, reader, reader, "test")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/runs/"+run.ID, nil)
	request.Header.Set("Authorization", "Bearer token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	var detail RunDetailResponse
	if err = json.Unmarshal(response.Body.Bytes(), &detail); err != nil || response.Code != http.StatusOK || detail.Run.ScheduleEntry != "nightly" {
		t.Fatalf("HTTP detail lost entry: %d %s (%v)", response.Code, response.Body, err)
	}
	raw, _ := json.Marshal(map[string]string{"id": run.ID})
	value, err := reader.CallTool(ctx, api.Actor{Identity: "agent@host"}, "run_get", raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	if err = json.Unmarshal(encoded, &detail); err != nil || detail.Run.ScheduleEntry != "nightly" {
		t.Fatalf("MCP detail lost entry: %s (%v)", encoded, err)
	}
	raw, _ = json.Marshal(map[string]string{"repo": f.repo.Name, "ref": run.SHA})
	value, err = reader.CallTool(ctx, api.Actor{Identity: "agent@host"}, "status", raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(value)
	var status struct {
		Entry string `json:"schedule_entry"`
	}
	if err = json.Unmarshal(encoded, &status); err != nil || status.Entry != "nightly" {
		t.Fatalf("MCP status lost entry: %s (%v)", encoded, err)
	}
}

func TestScheduleCronUsesAtomicCursorAndCoalescesLiveEntry(t *testing.T) {
	fixture := newRecoveryFixture(t)
	scheduler := fixture.scheduler(t, &recoveryJobs{})
	qualified, err := fixture.store.QualifiedRepoName(t.Context(), fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 27, 1, 30, 0, 0, time.UTC)
	request := CIRequest{EventID: "cron-first", Repository: fixture.repo, Branch: "main", SHA: strings.Repeat("a", 40), Actor: "oberth:scheduler", Trigger: "schedule", ScheduleEntry: "nightly"}
	first, err := scheduler.EnqueueScheduledCI(t.Context(), request, model.ScheduleFireSpec{Repo: qualified, At: at})
	if err != nil || first.Outcome != "fired" {
		t.Fatalf("first cron = %+v, %v", first, err)
	}
	request.EventID = "cron-next"
	next, err := scheduler.EnqueueScheduledCI(t.Context(), request, model.ScheduleFireSpec{Repo: qualified, At: at.Add(15 * time.Minute), HasPrevious: true, Previous: at})
	if err != nil || next.Outcome != "coalesced" || next.Enqueued.ID != first.Enqueued.ID {
		t.Fatalf("next cron = %+v, %v", next, err)
	}
	retained, err := fixture.store.Run(t.Context(), first.Enqueued.ID)
	if err != nil || retained.Status != model.RunQueued || retained.SupersededBy != "" {
		t.Fatalf("cron superseded its own live entry: %+v, %v", retained, err)
	}
}
