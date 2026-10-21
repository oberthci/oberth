package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// The active view lists a started run first even though it is newer than the
// claimed runs, then the claimed runs in admission (queue_sequence) order with
// each one's wait reason, then the unclaimed queue; a newer terminal run is
// not in it. The waits are legitimate overtakes: a VM-parked run and a group
// waiter hold no place, while the head that does not fit shows its weight.
func TestActiveRunsOrderAndWaitReasons(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := t.Context()
	gate := newWeightedAdmission(4)
	service := fixture.api(t)
	service.admission = &Scheduler{admission: gate}
	enqueue := func(branch string) model.Run {
		t.Helper()
		run, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: branch, Repository: fixture.repo, Branch: branch,
			SHA: strings.Repeat("a", 40), Actor: "agent@host"})
		if err != nil {
			t.Fatal(err)
		}
		claimed, err := fixture.store.ClaimNextRun(ctx)
		if err != nil || claimed.ID != run.ID {
			t.Fatalf("claim %s = %s, %v", branch, claimed.ID, err)
		}
		return claimed
	}
	var cleanup []func()
	defer func() {
		for _, release := range cleanup {
			release()
		}
	}()
	// 1: oldest; admitted, then parked on trusted VM capacity.
	vm := enqueue("feature/vm")
	vmReservation := gate.reserve(vm.QueueSequence)
	cleanup = append(cleanup, acquireRequestWithinBudget(t, vmReservation, groupRequest(vm.ID, vm.RepoID, periapsis.M, "")))
	if err := vmReservation.parkVM(); err != nil {
		t.Fatal(err)
	}
	// 2: holds concurrency group "deploy" and has started.
	holder := enqueue("feature/holder")
	cleanup = append(cleanup, acquireRequestWithinBudget(t, gate.reserve(holder.QueueSequence), groupRequest(holder.ID, holder.RepoID, periapsis.M, "deploy")))
	if _, err := fixture.store.SetRunJobName(ctx, holder.ID, "wf-holder"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.MarkRunSubmitted(ctx, holder.ID); err != nil {
		t.Fatal(err)
	}
	// 3: waits on its group, behind the holder.
	grouped := enqueue("feature/grouped")
	groupedReservation := gate.reserve(grouped.QueueSequence)
	// Waiters give up when the test context ends, so none outlives the test.
	go func() {
		_, _ = groupedReservation.acquireRequest(ctx, groupRequest(grouped.ID, grouped.RepoID, periapsis.S, "deploy"))
	}()
	awaitWaiting(t, groupedReservation)
	// 4: newer than the waiters, admitted and finished.
	done := enqueue("feature/done")
	releaseDone := acquireRequestWithinBudget(t, gate.reserve(done.QueueSequence), groupRequest(done.ID, done.RepoID, periapsis.S, ""))
	if _, err := fixture.store.FinishRun(ctx, done.ID, model.RunResult{Status: model.RunPassed, Phase: "passed"}); err != nil {
		t.Fatal(err)
	}
	releaseDone()
	// 5: the head of the weighted queue: needs 3 with 2 of 4 in use.
	head := enqueue("feature/head")
	headReservation := gate.reserve(head.QueueSequence)
	go func() {
		_, _ = headReservation.acquireRequest(ctx, groupRequest(head.ID, head.RepoID, periapsis.L, ""))
	}()
	awaitWaiting(t, headReservation)
	// 6: accepted, not yet claimed.
	queued, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: "queued", Repository: fixture.repo, Branch: "feature/queued",
		SHA: strings.Repeat("b", 40), Actor: "agent@host"})
	if err != nil {
		t.Fatal(err)
	}

	value, err := service.Runs(ctx, api.Actor{Identity: "agent@host"}, api.RunFilter{View: api.RunViewActive})
	if err != nil {
		t.Fatal(err)
	}
	active := value.([]ActiveRun)
	var order []string
	for _, entry := range active {
		order = append(order, entry.Ref)
	}
	want := "feature/holder,feature/vm,feature/grouped,feature/head,feature/queued"
	if strings.Join(order, ",") != want {
		t.Fatalf("active order = %v, want %s", order, want)
	}
	if active[0].Status != model.RunRunning || active[0].Scheduling != nil {
		t.Fatalf("started run = %#v", active[0])
	}
	vmEntry, groupEntry, headEntry, queuedEntry := active[1], active[2], active[3], active[4]
	if vmEntry.Status != model.RunWaiting || vmEntry.Scheduling == nil || vmEntry.Scheduling.Admission == nil ||
		!vmEntry.Scheduling.Admission.VMCapacity || !strings.Contains(vmEntry.Scheduling.Message, "trusted VM capacity") {
		t.Fatalf("VM-parked run lost its reason: %#v", vmEntry.Scheduling)
	}
	if groupEntry.Scheduling == nil || groupEntry.Scheduling.Admission == nil || groupEntry.Scheduling.Admission.Group != "deploy" ||
		groupEntry.Scheduling.Admission.GroupHolder != holder.ID || !strings.Contains(groupEntry.Scheduling.Message, `concurrency group "deploy"`) {
		t.Fatalf("group waiter lost its reason: %#v", groupEntry.Scheduling)
	}
	if admission := headEntry.Scheduling.Admission; admission == nil || admission.Size != "L" || admission.Weight != 3 ||
		admission.Used != 2 || admission.Capacity != 4 || admission.Position == 0 ||
		!strings.Contains(headEntry.Scheduling.Message, "Need 3 weight; 2/4 used") {
		t.Fatalf("queue head lost its weight: %#v %#v", headEntry.Scheduling, admission)
	}
	if queuedEntry.ID != queued.ID || queuedEntry.Scheduling == nil || queuedEntry.Scheduling.State != "queued" || queuedEntry.Scheduling.QueuePosition != 1 {
		t.Fatalf("queued run = %#v", queuedEntry.Scheduling)
	}

	// The history view holds the finished run only; the default has one
	// queue-first list followed by admitted runs in admission-time order.
	history, err := service.Runs(ctx, api.Actor{Identity: "agent@host"}, api.RunFilter{View: api.RunViewHistory, Limit: 50})
	if err != nil || len(history.([]model.Run)) != 1 || history.([]model.Run)[0].ID != done.ID {
		t.Fatalf("history = %#v, %v", history, err)
	}
	all, err := service.Runs(ctx, api.Actor{Identity: "agent@host"}, api.RunFilter{Limit: 50})
	if err != nil || len(all.([]model.Run)) != 6 || all.([]model.Run)[0].ID != vm.ID ||
		all.([]model.Run)[3].ID != queued.ID || all.([]model.Run)[4].ID != done.ID {
		t.Fatalf("default list = %#v, %v", all, err)
	}

	// MCP run_list: view=active returns the same ordered list under runs.
	mcp, err := service.CallTool(ctx, api.Actor{Identity: "agent@host"}, "run_list", json.RawMessage(`{"view":"active"}`))
	if err != nil {
		t.Fatal(err)
	}
	if listed := mcp.(api.RunListResponse); len(listed.Runs.([]ActiveRun)) != len(active) || listed.NextBefore != 0 {
		t.Fatalf("run_list active = %#v", listed)
	}
	encoded, _ := json.Marshal(mcp)
	if !bytes.Contains(encoded, []byte(`"Scheduling":{`)) || !bytes.Contains(encoded, []byte(`"Ref":"feature/head"`)) {
		t.Fatalf("run_list active wire shape = %s", encoded)
	}
	for _, bad := range []string{`{"view":"active","before":3}`, `{"view":"queue"}`} {
		if _, err := service.CallTool(ctx, api.Actor{Identity: "agent@host"}, "run_list", json.RawMessage(bad)); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("run_list %s = %v, want invalid input", bad, err)
		}
	}
}

// The default and explicit all views return the same queue-first list. A
// bounded first page carries the whole queue even when it exceeds limit.
func TestRunListDefaultViewCarriesWholeQueue(t *testing.T) {
	fixture := newControlFixture(t)
	ctx := context.Background()
	for _, branch := range []string{"feature/one", "feature/two", "feature/three"} {
		if _, err := fixture.scheduler.EnqueueCI(ctx, CIRequest{EventID: branch, Repository: fixture.repo, Branch: branch,
			SHA: strings.Repeat("c", 40), Actor: "agent@host"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := fixture.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	service := fixture.api(t)
	actor := api.Actor{Identity: "agent@host"}
	list := func(arguments string) api.RunListResponse {
		t.Helper()
		value, err := service.CallTool(ctx, actor, "run_list", json.RawMessage(arguments))
		if err != nil {
			t.Fatal(err)
		}
		return value.(api.RunListResponse)
	}
	for _, arguments := range []string{`{"limit":2}`, `{"limit":2,"view":"all"}`, `{"limit":2,"view":""}`} {
		page := list(arguments)
		runs := page.Runs.([]model.Run)
		if len(runs) != 3 || page.NextBefore != 0 || page.NextCursor != "" {
			t.Fatalf("run_list %s = %+v", arguments, page)
		}
		for position, run := range runs {
			if run.DisplayStatus != "queued" || run.Scheduling == nil || run.Scheduling.QueuePosition != int64(position+1) {
				t.Fatalf("queue position %d: %+v", position+1, run)
			}
		}
	}
	if got := list(`{}`).Runs.([]model.Run); len(got) != 3 || got[0].Ref != "feature/one" {
		t.Fatalf("default list = %+v", got)
	}
}

// A scoped uplink's active view covers its upstream only, with no global
// sequence or queue position (#073).
func TestScopedActiveRunsStayInScope(t *testing.T) {
	fixture := newScopeFixture(t)
	value, err := fixture.call(scopedActor, "run_list", map[string]any{"view": "active"})
	fixture.assertNoLeak(t, "run_list active", value, err)
	// The fixture finished the first github run; the rest are queued.
	active := value.(api.RunListResponse).Runs.([]ActiveRun)
	if len(active) != len(fixture.runs["github"])-1 || active[0].ID != fixture.runs["github"][1].ID {
		t.Fatalf("scoped active = %d runs, want %d", len(active), len(fixture.runs["github"])-1)
	}
	for _, entry := range active {
		if entry.RepoID != fixture.visible.ID || entry.QueueSequence != 0 || entry.Scheduling == nil || entry.Scheduling.QueuePosition != 0 {
			t.Fatalf("scoped active entry = %#v %#v", entry.Run, entry.Scheduling)
		}
	}
	if hidden, err := fixture.call(scopedActor, "run_list", map[string]any{"view": "active", "repo": hiddenRepoName}); err == nil {
		t.Fatalf("hidden repository active view = %#v", hidden)
	}
	all := mustCall(t, fixture, unscopedActor, "run_list", map[string]any{"view": "active"}).(api.RunListResponse).Runs.([]ActiveRun)
	if len(all) != len(fixture.runs["github"])-1+len(fixture.runs["codeberg"]) || all[0].Scheduling.QueuePosition != 1 || all[0].QueueSequence == 0 {
		t.Fatalf("unscoped active = %d runs", len(all))
	}
}
