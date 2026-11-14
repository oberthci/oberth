package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
)

func TestScopedRunListUnknownPositionDoesNotInventQueueHead(t *testing.T) {
	f := newControlFixture(t)
	var seventh model.Run
	for i := range 7 {
		run, err := f.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: fmt.Sprintf("feature/scoped-%d", i), SHA: strings.Repeat("a", 40), Actor: "owner", Trigger: "push"})
		if err != nil {
			t.Fatal(err)
		}
		seventh = run.Run
	}
	service := f.api(t)
	actor := api.Actor{Identity: "scoped", Scope: []string{"codeberg"}}
	for _, view := range []string{""} {
		value, err := service.Runs(t.Context(), actor, api.RunFilter{Ref: seventh.Ref, View: view})
		if err != nil {
			t.Fatal(err)
		}
		var rows []model.Run
		switch value := value.(type) {
		case []model.Run:
			rows = value
		}
		if len(rows) != 1 || rows[0].DisplayReason != "in line" {
			t.Fatalf("scoped queue invented head: %+v", rows)
		}
	}
	value, err := service.Run(t.Context(), actor, seventh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := value.(RunDetailResponse).Run.DisplayReason; got != "in line" {
		t.Fatal(got)
	}
}

func TestRunListsObserveArgoWithoutDependingOnLocalStepHistory(t *testing.T) {
	for _, view := range []string{""} {
		t.Run(view, func(t *testing.T) {
			f := newControlFixture(t)
			runs := make([]model.Run, 0, 6)
			for i := range 6 {
				enqueued, err := f.store.EnqueueRun(t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: fmt.Sprintf("feature/list-startup-%d", i), SHA: strings.Repeat("a", 40), Actor: "owner", Trigger: "push"})
				if err != nil {
					t.Fatal(err)
				}
				run, err := f.store.ClaimNextRun(t.Context())
				if err != nil || run.ID != enqueued.ID {
					t.Fatal("claim", err)
				}
				if _, err := f.store.SetRunWorkflowName(t.Context(), run.ID, "workflow-"+run.ID[:8]); err != nil {
					t.Fatal(err)
				}
				run, err = f.store.MarkRunSubmitted(t.Context(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				runs = append(runs, run)
			}
			step := stepResult(model.StepPassed)
			step.RunID = runs[3].ID
			if _, err := putNativeStepFixture(t.Context(), f.store, step); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.root, "logs", runs[4].ID+".progress.jsonl"), []byte("corrupt progress\n"), 0600); err != nil {
				t.Fatal(err)
			}
			service := f.api(t)
			calls := map[string]int{}
			var callsMu sync.Mutex
			service.schedulingObserver = schedulingObserverFunc(func(_ context.Context, run model.Run) (*model.ExecutionObservation, error) {
				callsMu.Lock()
				calls[run.ID]++
				callsMu.Unlock()
				return &model.ExecutionObservation{State: "observed", Pods: []model.SchedulingReason{{Phase: "Pending", Reason: "Unschedulable", Message: "pod pending: too many pods; waiting for a pod slot"}}}, nil
			})
			for poll := 1; poll <= 3; poll++ {
				rows := pollDiagnosticList(t, service, api.Actor{Identity: "owner"}, api.RunFilter{View: view}, func(rows []model.Run) bool {
					for _, run := range rows {
						if run.ID == runs[0].ID {
							return run.DisplayReason == "pod pending: too many pods; waiting for a pod slot"
						}
					}
					return false
				})
				if len(rows) != len(runs) {
					t.Fatalf("list has %d rows", len(rows))
				}
				callsMu.Lock()
				if len(calls) != len(runs) {
					t.Errorf("missing live observations: %v", calls)
				}
				for _, run := range runs {
					want := poll
					if calls[run.ID] != want {
						t.Errorf("native state cache calls: %v", calls)
					}
				}
				callsMu.Unlock()
			}
			if _, err := service.Runs(t.Context(), api.Actor{Identity: "owner"}, api.RunFilter{View: view}); err != nil {
				t.Fatal(err)
			}
			callsMu.Lock()
			defer callsMu.Unlock()
			if calls[runs[0].ID] != 4 {
				t.Fatal("progress hid native suspension/queue observations", calls)
			}
		})
	}
}
