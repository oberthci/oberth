package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// recordingScheduleControl serves one "nightly" entry on every repository,
// reads pauses from the store as the scheduler does, and records what the
// service asks it to enqueue.
type recordingScheduleControl struct {
	store    *store.Store
	plans    []SchedulePlan
	enqueued []string
	busy     bool
}

func (control *recordingScheduleControl) InspectSchedule(ctx context.Context, repository model.Repository) (ScheduleRepoView, bool, error) {
	pauses, err := control.store.SchedulePauses(ctx, repository.Name)
	if err != nil {
		return ScheduleRepoView{}, false, err
	}
	entry := ScheduleEntryView{Name: "nightly", Cron: "0 3 * * *"}
	if pause, ok := pauses["nightly"]; ok {
		entry.Paused = &SchedulePauseView{By: pause.By, Reason: pause.Reason}
	}
	return ScheduleRepoView{Repo: repository.Name, DefaultBranch: "main", Entries: []ScheduleEntryView{entry}}, true, nil
}

func (control *recordingScheduleControl) PlanScheduleTrigger(_ context.Context, repository model.Repository, entry string, force bool) (SchedulePlan, error) {
	if control.busy && !force {
		return SchedulePlan{}, fmt.Errorf("%w: busy", store.ErrInvalidState)
	}
	plan := SchedulePlan{Repository: repository, Qualified: repository.Name, Entry: entry, Branch: "main", SHA: sha("a")}
	control.plans = append(control.plans, plan)
	return plan, nil
}

func (control *recordingScheduleControl) EnqueueSchedule(_ context.Context, plan SchedulePlan, actor string) (model.EnqueueRunResult, error) {
	control.enqueued = append(control.enqueued, actor+"@"+plan.Branch)
	return model.EnqueueRunResult{Run: model.Run{ID: "run-1", Status: model.RunQueued}}, nil
}

func auditHead(t *testing.T, fixture *scopeFixture) int64 {
	t.Helper()
	head, err := fixture.store.AuditHeadHint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return head.ID
}

func latestScheduleAudit(t *testing.T, fixture *scopeFixture, action string) (model.AuditAction, bool) {
	t.Helper()
	got, err := fixture.store.LatestAuditActionByType(context.Background(), action, "schedule")
	if errors.Is(err, store.ErrNotFound) {
		return model.AuditAction{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return got, true
}

func TestScheduleMutationsAreAdminOnlyAuditedAndScoped(t *testing.T) {
	t.Parallel()
	fixture := newScopeFixture(t)
	control := &recordingScheduleControl{store: fixture.store}
	fixture.service.schedules = control
	repo := map[string]any{"repo": "oberth", "entry": "nightly"}

	for _, tool := range []string{"schedule_pause", "schedule_resume", "schedule_trigger"} {
		if _, err := fixture.call(scopedActor, tool, repo); !errors.Is(err, ErrForbidden) {
			t.Fatalf("non-admin %s error = %v, want forbidden", tool, err)
		}
	}
	if len(control.plans)+len(control.enqueued) != 0 {
		t.Fatal("a refused trigger reached the scheduler")
	}

	// A scoped admin cannot tell a hidden repository from a missing one.
	_, hiddenErr := fixture.call(scopedAdmin, "schedule_pause", map[string]any{"repo": "codeberg/cloudtaser/" + hiddenRepoName, "entry": "nightly"})
	_, missingErr := fixture.call(scopedAdmin, "schedule_pause", map[string]any{"repo": "codeberg/cloudtaser/does-not-exist", "entry": "nightly"})
	if !errors.Is(hiddenErr, store.ErrNotFound) || !errors.Is(missingErr, store.ErrNotFound) {
		t.Fatalf("hidden %v, missing %v; both want not found", hiddenErr, missingErr)
	}

	// pause is audited once and is idempotent.
	var afterFirst int64
	for attempt, wantChanged := range []bool{true, false} {
		value, err := fixture.call(unscopedActor, "schedule_pause", map[string]any{"repo": "oberth", "entry": "nightly", "reason": "forge saturated"})
		if err != nil {
			t.Fatal(err)
		}
		if got := value.(map[string]any)["changed"]; got != wantChanged {
			t.Fatalf("pause attempt %d changed = %v, want %v", attempt, got, wantChanged)
		}
		if attempt == 0 {
			afterFirst = auditHead(t, fixture)
		}
	}
	if pause, ok := latestScheduleAudit(t, fixture, "schedule.pause"); !ok || pause.ID != afterFirst || pause.Actor != unscopedActor.Identity || auditHead(t, fixture) != afterFirst {
		t.Fatalf("pause audit = %+v (head %d, after first %d); want exactly one row by the caller", pause, auditHead(t, fixture), afterFirst)
	}
	listed, err := fixture.call(scopedActor, "schedule_list", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	schedules := listed.(ScheduleListResponse).Schedules
	if len(schedules) != 1 || schedules[0].Repo != "oberth" || schedules[0].Entries[0].Paused == nil || schedules[0].Entries[0].Paused.By != unscopedActor.Identity {
		t.Fatalf("scoped schedule_list = %+v, want the visible repository's paused entry", schedules)
	}

	// A paused entry needs force; trigger is audited before it enqueues and
	// attributes the run to the caller.
	if _, err := fixture.call(unscopedActor, "schedule_trigger", repo); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("trigger of a paused entry error = %v", err)
	}
	if _, err := fixture.call(unscopedActor, "schedule_resume", repo); err != nil {
		t.Fatal(err)
	}
	control.busy = true
	if _, err := fixture.call(unscopedActor, "schedule_trigger", repo); !errors.Is(err, store.ErrInvalidState) {
		t.Fatalf("trigger on a busy repository error = %v", err)
	}
	if _, ok := latestScheduleAudit(t, fixture, "schedule.trigger"); ok || len(control.enqueued) != 0 {
		t.Fatal("a refused trigger was audited or enqueued")
	}
	value, err := fixture.call(unscopedActor, "schedule_trigger", map[string]any{"repo": "oberth", "entry": "nightly", "force": true})
	if err != nil {
		t.Fatal(err)
	}
	if got := value.(ScheduleTriggerResponse); got.RunID != "run-1" || got.Branch != "main" {
		t.Fatalf("trigger response = %+v", got)
	}
	if len(control.enqueued) != 1 || control.enqueued[0] != unscopedActor.Identity+"@main" {
		t.Fatalf("enqueued = %v", control.enqueued)
	}
	trigger, triggered := latestScheduleAudit(t, fixture, "schedule.trigger")
	_, resumed := latestScheduleAudit(t, fixture, "schedule.resume")
	if !triggered || !resumed || trigger.Actor != unscopedActor.Identity || trigger.ResourceID != "oberth#nightly" {
		t.Fatalf("trigger audit = %+v (resume recorded: %v)", trigger, resumed)
	}
}

type scheduleSurfaceAuth struct{ actor api.Actor }

func (auth scheduleSurfaceAuth) Authenticate(context.Context, string) (api.Actor, error) {
	return auth.actor, nil
}

func TestScheduleHTTPMatchesMCPAndHidesOutOfScopeRepositories(t *testing.T) {
	t.Parallel()
	fixture := newScopeFixture(t)
	fixture.service.schedules = &recordingScheduleControl{store: fixture.store}
	for _, actor := range []api.Actor{unscopedActor, scopedActor, scopedAdmin, {Identity: "reader"}} {
		server, err := api.New(scheduleSurfaceAuth{actor}, fixture.service, fixture.service, "test", api.WithErrorClassifier(func(err error) (int, string) {
			if errors.Is(err, store.ErrNotFound) {
				return http.StatusNotFound, "not found"
			}
			return http.StatusInternalServerError, "internal error"
		}))
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, "/api/schedules", nil)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated schedule list: %d", response.Code)
		}
		for _, repo := range []string{"", "oberth", "codeberg/cloudtaser/" + hiddenRepoName, "codeberg/cloudtaser/does-not-exist"} {
			request = httptest.NewRequest(http.MethodGet, "/api/schedules?repo="+url.QueryEscape(repo), nil)
			request.Header.Set("Authorization", "Bearer test")
			response = httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			value, err := fixture.call(actor, "schedule_list", map[string]any{"repo": repo})
			if errors.Is(err, store.ErrNotFound) {
				if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), hiddenRepoName) {
					t.Fatalf("actor %s repo %s: %d %s", actor.Identity, repo, response.Code, response.Body)
				}
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			var page ScheduleListResponse
			if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil || response.Code != http.StatusOK {
				t.Fatalf("HTTP %d %s: %v", response.Code, response.Body, err)
			}
			if !reflect.DeepEqual(page, value) {
				t.Fatalf("HTTP=%+v MCP=%+v", page, value)
			}
			if actor.Scoped() && (len(page.Schedules) != 1 || page.Schedules[0].Repo != "oberth") {
				t.Fatalf("scoped schedules: %+v", page)
			}
			if !actor.Scoped() && repo == "" && len(page.Schedules) != 2 {
				t.Fatalf("unscoped schedules: %+v", page)
			}
		}
	}
}
