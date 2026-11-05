package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestConcurrencyGroupsReadOnlySurfaceIncludesIdleDefinitions(t *testing.T) {
	f := newControlFixture(t)
	run, err := f.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("a", 40), Actor: "owner", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.SetRunConcurrencyGroup(t.Context(), run.ID, "state", 2); err != nil {
		t.Fatal(err)
	}
	control := f.api(t)
	control.admission = f.scheduler
	value, err := control.CallTool(t.Context(), api.Actor{Identity: "reader"}, "concurrency_groups", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response, ok := value.(api.ConcurrencyGroupsResponse)
	groups := response.Groups
	if !ok || len(groups) != 1 || groups[0].Name != "state" || groups[0].Limit != 2 || len(groups[0].Holders) != 0 {
		t.Fatalf("unexpected groups %#v", value)
	}
	if MutatingTool("concurrency_groups") {
		t.Fatal("read-only tool classified as mutation")
	}
	if len(f.scheduler.AdmissionState().Groups) != 1 {
		t.Fatal("idle definition missing from system status")
	}
	if _, err = control.CallTool(t.Context(), api.Actor{Identity: "scoped", Scope: []string{"codeberg"}}, "concurrency_groups", json.RawMessage(`{}`)); !errors.Is(err, ErrForbidden) {
		t.Fatal("scoped cross-repository list not refused", err)
	}
}

func TestConcurrencyGroupsScopedQueuedReasonHidesOtherRepositories(t *testing.T) {
	g := newWorkflowAdmissionForJobs(3)
	hidden := strings.Repeat("a", 32)
	r := g.reserveRun(model.Run{ID: hidden, RepoID: 2, QueueSequence: 1})
	release := acquireRequestWithinBudget(t, r, admissionRequest{RunID: hidden, RepoID: 2, Group: "state"})
	defer release()
	run := model.Run{ID: strings.Repeat("b", 32), RepoID: 1, QueueSequence: 2, Status: model.RunRunning}
	w := g.reserveRun(run)
	ready := make(chan func(), 1)
	go acquireRequestForTest(w, admissionRequest{RunID: run.ID, RepoID: 1, Group: "state"}, ready)
	awaitWaiting(t, w)
	defer w.cancel()
	control := &API{admission: &Scheduler{admission: g}, scope: &repoScope{repos: map[int64]model.Repository{1: {ID: 1}}}}
	observation, reason := control.observeScheduling(t.Context(), run, nil, false)
	body, err := json.Marshal(observation)
	if err != nil || strings.Contains(string(body), hidden) || strings.Contains(reason, hidden[:12]) || reason != "group state busy" || observation.Admission == nil || observation.Admission.GroupLimit != 1 {
		t.Fatal(string(body), reason, err)
	}
}
