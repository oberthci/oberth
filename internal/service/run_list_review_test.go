package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/pipeline"
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
	for _, view := range []string{"", api.RunViewActive} {
		value, err := service.Runs(t.Context(), actor, api.RunFilter{Ref: seventh.Ref, View: view})
		if err != nil {
			t.Fatal(err)
		}
		var rows []model.Run
		switch value := value.(type) {
		case []model.Run:
			rows = value
		case []ActiveRun:
			for _, entry := range value {
				rows = append(rows, entry.Run)
			}
		}
		if len(rows) != 1 || rows[0].DisplayReason != "in line" || rows[0].QueuePosition != 0 {
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

func TestRunListsObserveOnlyBeforeAnyStepStart(t *testing.T) {
	for _, view := range []string{"", api.RunViewActive} {
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
				if i != 5 {
					if err := f.logs.InitializeStepProgress(run.ID); err != nil {
						t.Fatal(err)
					}
				}
				runs = append(runs, run)
			}
			now := time.Now()
			event := runprogress.Event{Version: runprogress.Version, Stage: "test", Step: "unit", Status: runprogress.StepRunning, StartedAt: &now, DeclaredSize: pipeline.M}
			if err := f.logs.AppendStepProgress(runs[1].ID, event); err != nil {
				t.Fatal(err)
			}
			if err := f.logs.AppendStepProgress(runs[2].ID, event); err != nil {
				t.Fatal(err)
			}
			event.Status, event.StartedAt = runprogress.StepQueued, nil
			if err := f.logs.AppendStepProgress(runs[2].ID, event); err != nil {
				t.Fatal(err)
			}
			step := stepResult(model.StepPassed)
			step.RunID = runs[3].ID
			step.DeclaredSize = pipeline.M
			if _, err := f.store.PutStepResult(t.Context(), step); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.root, "logs", runs[4].ID+".progress.jsonl"), []byte("corrupt progress\n"), 0600); err != nil {
				t.Fatal(err)
			}
			service := f.api(t)
			calls := map[string]int{}
			service.schedulingObserver = schedulingObserverFunc(func(_ context.Context, run model.Run) (*model.ExecutionObservation, error) {
				calls[run.ID]++
				return &model.ExecutionObservation{State: "observed", Pods: []model.SchedulingReason{{Phase: "Pending", Reason: "Unschedulable", Message: "pod pending: too many pods; waiting for a pod slot"}}}, nil
			})
			for poll := 1; poll <= 2; poll++ {
				value, err := service.Runs(t.Context(), api.Actor{Identity: "owner"}, api.RunFilter{View: view})
				if err != nil {
					t.Fatal(err)
				}
				var rows []model.Run
				switch value := value.(type) {
				case []model.Run:
					rows = value
				case []ActiveRun:
					for _, entry := range value {
						rows = append(rows, entry.Run)
					}
				}
				if len(rows) != len(runs) {
					t.Fatalf("list has %d rows", len(rows))
				}
				if len(calls) != 1 || calls[runs[0].ID] != poll {
					t.Fatalf("poll %d observed executing/unknown rows: %v", poll, calls)
				}
				for _, run := range rows {
					if run.ID == runs[0].ID && run.DisplayReason != "pod pending: too many pods; waiting for a pod slot" {
						t.Fatal(run.DisplayReason)
					}
				}
			}
			event.Status, event.StartedAt = runprogress.StepRunning, &now
			if err := f.logs.AppendStepProgress(runs[0].ID, event); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Runs(t.Context(), api.Actor{Identity: "owner"}, api.RunFilter{View: view}); err != nil {
				t.Fatal(err)
			}
			if calls[runs[0].ID] != 2 {
				t.Fatal("polling continued after first step start", calls)
			}
		})
	}
}
