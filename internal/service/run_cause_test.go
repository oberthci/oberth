package service_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func runCauses() []model.RunCause {
	return []model.RunCause{
		{Actor: "worker", Trigger: model.TriggerBranch, AssistRequested: true},
		{Actor: "oberth:scheduler", Trigger: model.TriggerSchedule, ScheduleEntry: "nightly"},
		{Actor: "worker", Trigger: model.TriggerBranch, RerunOf: runID},
		{Actor: "owner", Trigger: model.TriggerPromotion},
		{Actor: "owner", Trigger: model.TriggerTag},
	}
}

func seedCause(f *fixture, i int, cause model.RunCause) stateport.Run {
	r := f.seed(stepmodel.RunPassed)
	r.ID = fmt.Sprintf("abcdef02%024d", i)
	r.Origin = model.RunOrigin{RunCause: cause, Assisted: cause.AssistRequested, WorkflowName: "native-" + r.ID, WorkflowUID: "uid-" + r.ID}
	f.store.native.SeedRun(r)
	f.store.native.SeedSteps(r.ID, nil)
	return r
}

// requireRunGetCause reads one run and refuses a changed cause or any shown
// execution identity.
func requireRunGetCause(t *testing.T, f *fixture, id string, cause model.RunCause) {
	t.Helper()
	got, err := f.s.RunGet(t.Context(), caller(), viewQuery{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if got.RunCause != cause || got.ID != id {
		t.Fatalf("run_get cause = %+v, want %+v", got.RunCause, cause)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"workflow_name", "workflow_uid", `"assisted"`, "native-", "uid-"} {
		if strings.Contains(string(data), private) {
			t.Fatalf("run_get shows execution identity %q: %s", private, data)
		}
	}
}

// TestRunViewsShowRecordedCause pins that run_get and run_list show what
// started each run exactly as acceptance recorded it, and nothing else of origin.
func TestRunViewsShowRecordedCause(t *testing.T) {
	t.Parallel()
	f := setup(t)
	want := map[string]model.RunCause{}
	for i, cause := range runCauses() {
		r := seedCause(f, i, cause)
		want[r.ID] = cause
		requireRunGetCause(t, f, r.ID, cause)
	}
	page, err := f.s.RunList(t.Context(), caller(), viewQuery{Repo: repository})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, row := range page.Runs {
		if cause, ok := want[row.ID]; ok {
			seen++
			if row.RunCause != cause {
				t.Fatalf("run_list cause for %s = %+v, want %+v", row.ID, row.RunCause, cause)
			}
		}
	}
	if seen != len(want) {
		t.Fatalf("run_list showed %d of %d runs", seen, len(want))
	}
}

// TestRunCauseStaysInsideScope pins that an actor name reaches only callers
// who may read the run.
func TestRunCauseStaysInsideScope(t *testing.T) {
	t.Parallel()
	f := setup(t)
	hidden := f.seed(stepmodel.RunPassed)
	hidden.ID = strings.Repeat("b", 32)
	hidden.Repo = "private/team/hidden"
	hidden.Origin.Actor = "hidden-actor"
	f.store.native.SeedRepository(stateport.Repository{Upstream: "private", Name: "hidden", Canonical: hidden.Repo})
	f.store.native.SeedRun(hidden)
	_, err := f.s.RunGet(t.Context(), caller(), viewQuery{ID: hidden.ID})
	requireCode(t, err, publicerr.NotFound)
	page, err := f.s.RunList(t.Context(), caller(), viewQuery{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "hidden-actor") || len(page.Runs) == 0 {
		t.Fatalf("run_list scope = %s", data)
	}
	shown, err := f.s.RunGet(t.Context(), admin(), viewQuery{ID: hidden.ID})
	if err != nil || shown.Actor != "hidden-actor" {
		t.Fatalf("admin view = %+v, %v", shown.RunCause, err)
	}
}
