package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestAdminRunListStatusFiltersBeforePaging(t *testing.T) {
	f := newControlFixture(t)
	control := f.api(t)
	actor := api.Actor{Identity: "reader"}
	var failed []string
	for i, status := range []model.RunStatus{model.RunFailed, model.RunPassed, model.RunFailed, model.RunPassed} {
		run, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: fmt.Sprintf("feature/%d", i), SHA: strings.Repeat("a", 40), Actor: "dev", Trigger: "push"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.store.FinishRun(t.Context(), run.ID, model.RunResult{Status: status}); err != nil {
			t.Fatal(err)
		}
		if status == model.RunFailed {
			failed = append(failed, run.ID)
		}
	}
	for _, selector := range []string{`"failed"`, `["failed"]`} {
		raw := json.RawMessage(`{"status":` + selector + `,"limit":1}`)
		value, err := control.CallTool(t.Context(), actor, "run_list", raw)
		if err != nil {
			t.Fatal(err)
		}
		first := value.(api.RunListResponse)
		runs := first.Runs.([]model.Run)
		if len(runs) != 1 || runs[0].ID != failed[1] || first.NextBefore == 0 {
			t.Fatalf("first filtered page=%+v", first)
		}
		raw = json.RawMessage(fmt.Sprintf(`{"status":%s,"limit":1,"before":%d}`, selector, first.NextBefore))
		value, err = control.CallTool(t.Context(), actor, "run_list", raw)
		if err != nil {
			t.Fatal(err)
		}
		second := value.(api.RunListResponse).Runs.([]model.Run)
		if len(second) != 1 || second[0].ID != failed[0] {
			t.Fatalf("second filtered page=%+v", second)
		}
	}
}

func TestAdminRunListStatusKeepsScopedCursors(t *testing.T) {
	f := newScopeFixture(t)
	value, err := f.call(scopedActor, "run_list", map[string]any{"status": []string{"queued", "passed", "failed"}, "limit": 1})
	f.assertNoLeak(t, "filtered run list", value, err)
	if err != nil {
		t.Fatal(err)
	}
	page := value.(api.RunListResponse)
	if page.NextBefore != 0 || page.NextCursor == "" {
		t.Fatalf("scoped cursor changed: %+v", page)
	}
	value, err = f.call(scopedActor, "run_list", map[string]any{"status": []string{"queued", "passed", "failed"}, "limit": 1, "before": page.NextCursor})
	f.assertNoLeak(t, "filtered next page", value, err)
	if err != nil {
		t.Fatal(err)
	}
}
