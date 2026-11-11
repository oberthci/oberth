package service

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/storetest"
)

func (f *agentFixture) runList(t *testing.T, arguments map[string]any) ([]model.Run, error) {
	t.Helper()
	raw, _ := json.Marshal(arguments)
	value, err := f.service.CallTool(t.Context(), agentActor, "run_list", raw)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(value.(api.RunListResponse).Runs)
	if err != nil {
		t.Fatal(err)
	}
	var runs []model.Run
	if err := json.Unmarshal(encoded, &runs); err != nil {
		t.Fatal(err)
	}
	return runs, nil
}

func runIDs(runs []model.Run) []string {
	ids := make([]string, len(runs))
	for index, run := range runs {
		ids[index] = run.ID
	}
	return ids
}

// run_list own:true keeps the caller's runs in every view; held:true the
// running assisted runs with an unreleased parked hold (review P2-4).
func TestRunListOwnAndHeldFilters(t *testing.T) {
	f := newAgentFixture(t)
	mine := f.enqueue(t, "land/mine", "a", true)
	foreign, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "land/theirs",
		SHA: strings.Repeat("b", 40), Actor: "other@host", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range []string{"", "all"} {
		arguments := map[string]any{"own": true}
		if view != "" {
			arguments["view"] = view
		}
		runs, err := f.runList(t, arguments)
		if err != nil || len(runs) != 1 || runs[0].ID != mine.ID {
			t.Fatalf("own view %q = %v, %v", view, runIDs(runs), err)
		}
	}
	if runs, err := f.runList(t, map[string]any{}); err != nil || len(runs) != 2 {
		t.Fatalf("unfiltered = %v, %v (foreign %s)", runIDs(runs), err, foreign.ID)
	}

	if runs, err := f.runList(t, map[string]any{"held": true}); err != nil || len(runs) != 0 {
		t.Fatalf("held before any hold = %v, %v", runIDs(runs), err)
	}
	claimed, err := f.store.ClaimNextRun(t.Context())
	if err != nil || claimed.ID != mine.ID {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	if _, err := f.store.SetRunWorkflowName(t.Context(), mine.ID, "wf-mine"); err != nil {
		t.Fatal(err)
	}
	if current, _ := f.store.Run(t.Context(), mine.ID); current.Status != model.RunRunning {
		t.Fatalf("fixture run is %s, not running", current.Status)
	}
	f.holds.holds = []model.AssistHold{{Pod: "wf-mine-test-1", Step: "test", Attempt: 1, HeldSince: time.Now()}}
	if runs, err := f.runList(t, map[string]any{"held": true, "own": true}); err != nil || len(runs) != 1 || runs[0].ID != mine.ID {
		t.Fatalf("held = %v, %v", runIDs(runs), err)
	}
	f.holds.holds[0].Released = true
	if runs, err := f.runList(t, map[string]any{"held": true}); err != nil || len(runs) != 0 {
		t.Fatalf("held after release = %v, %v", runIDs(runs), err)
	}
	for name, arguments := range map[string]map[string]any{
		"before":     {"held": true, "before": 5},
		"history":    {"held": true, "view": "history"},
		"superseded": {"held": true, "superseded": true},
		"view":       {"held": true, "view": "bogus"},
	} {
		if _, err := f.runList(t, arguments); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("held with %s = %v", name, err)
		}
	}
}

// run_list {ref_glob, latest:true} is the newest run of each matching ref
// (#1524); a malformed pattern is refused.
func TestRunListLatestPerRefGlob(t *testing.T) {
	f := newAgentFixture(t)
	f.enqueue(t, "land/a", "a", true)
	newestA := f.enqueue(t, "land/a", "b", true)
	landB := f.enqueue(t, "land/b", "c", true)
	f.enqueue(t, "feature/x", "d", true)
	runs, err := f.runList(t, map[string]any{"ref_glob": "land/*", "latest": true})
	if err != nil {
		t.Fatal(err)
	}
	ids := runIDs(runs)
	if len(ids) != 2 || !slices.Contains(ids, newestA.ID) || !slices.Contains(ids, landB.ID) {
		t.Fatalf("latest land/* = %v", ids)
	}
	if _, err := f.runList(t, map[string]any{"ref_glob": "land/[", "latest": true}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("malformed glob: %v", err)
	}
	if _, err := f.runList(t, map[string]any{"held": true, "latest": true}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("held latest: %v", err)
	}
}
