package argojob

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// journalMarkers passes the controller's actual wire markers into a real Store.
type journalMarkers struct {
	store *runlog.Store
	runID string
}

func (writer journalMarkers) Write(body []byte) (int, error) {
	for _, line := range bytes.SplitAfter(body, []byte("\n")) {
		event, found, err := runprogress.ParseMarker(line)
		if err != nil {
			return 0, err
		}
		if found {
			if err := writer.store.AppendStepProgress(writer.runID, event); err != nil {
				return 0, err
			}
		}
	}
	return len(body), nil
}

func podRetryOrdinalPolls() (*wfv1.Workflow, *wfv1.Workflow) {
	wf := &wfv1.Workflow{}
	wf.Name = "wf"
	wf.Status.Nodes = wfv1.Nodes{
		"root": {ID: "root", Name: "wf", Type: wfv1.NodeTypeDAG},
		"a":    {ID: "a", Name: "wf.a", DisplayName: "a", Type: wfv1.NodeTypeRetry, BoundaryID: "root", Children: []string{"a0"}},
		"a0": {ID: "a0", Name: "wf.a(0)", DisplayName: "a(0)", Type: wfv1.NodeTypePod, BoundaryID: "root",
			Phase: wfv1.NodeFailed, StartedAt: metav1.NewTime(time.Unix(1000, 0)), FinishedAt: metav1.NewTime(time.Unix(1001, 0))},
		"b": {ID: "b", Name: "wf.b", DisplayName: "b", Type: wfv1.NodeTypePod, BoundaryID: "root",
			Phase: wfv1.NodeRunning, StartedAt: metav1.NewTime(time.Unix(1002, 0))},
	}
	second := wf.DeepCopy()
	retry := second.Status.Nodes["a"]
	retry.Children = append(retry.Children, "a1")
	second.Status.Nodes["a"] = retry
	second.Status.Nodes["a1"] = wfv1.NodeStatus{ID: "a1", Name: "wf.a(1)", DisplayName: "a(1)", Type: wfv1.NodeTypePod, BoundaryID: "root",
		Phase: wfv1.NodeRunning, StartedAt: metav1.NewTime(time.Unix(1003, 0))}
	return wf, second
}

func TestRetryOrdinalsSurviveParallelPollsInRealJournal(t *testing.T) {
	for _, kind := range []string{"pod", "dag"} {
		t.Run(kind, func(t *testing.T) {
			first, second := podRetryOrdinalPolls()
			want := map[string]int{"a/a": 0, "b/b": 1}
			if kind == "dag" {
				first = dagRetryWorkflow(wfv1.WorkflowRunning, [2]wfv1.NodePhase{wfv1.NodeSucceeded, wfv1.NodeFailed})
				second = dagRetryWorkflow(wfv1.WorkflowRunning,
					[2]wfv1.NodePhase{wfv1.NodeSucceeded, wfv1.NodeFailed},
					[2]wfv1.NodePhase{wfv1.NodeSucceeded, wfv1.NodeRunning})
				delete(second.Status.Nodes, "install")
				b := wfv1.NodeStatus{ID: "b", Name: "wf.b", DisplayName: "b", Type: wfv1.NodeTypePod, BoundaryID: "root",
					Phase: wfv1.NodeRunning, StartedAt: metav1.NewTime(first.Status.Nodes["address-0-check"].FinishedAt.Add(time.Second))}
				first.Status.Nodes["b"], second.Status.Nodes["b"] = b, b
				want = map[string]int{"create/create": 0, "address/get": 1, "address/check": 2, "b/b": 3}
			}
			logs, err := runlog.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			writer := journalMarkers{logs, "run"}
			reporter := newProgressReporter(writer)
			controller := &Controller{}
			for poll, wf := range []*wfv1.Workflow{first, second} {
				if err := controller.publish(context.Background(), wf, reporter, writer); err != nil {
					t.Fatal(err)
				}
				if err := reporter.failure(); err != nil {
					t.Fatal(err)
				}
				events, err := logs.StepProgress("run")
				if err != nil || len(events) != len(want) {
					t.Fatalf("poll %d: progress %v: %v", poll, events, err)
				}
				for _, event := range events {
					if event.Ordinal != want[event.Burn+"/"+event.Step] {
						t.Fatalf("poll %d: changed ordinal %+v", poll, event)
					}
				}
				final := StepResults(wf)
				for i := 1; i < len(final); i++ {
					if final[i-1].Ordinal >= final[i].Ordinal {
						t.Fatalf("poll %d: final rows moved out of first-attempt order: %+v", poll, final)
					}
				}
				// Assert both the public final rows and superseded attempt rows.
				for _, rows := range [][]StepResult{stepAttempts(wf), final} {
					for _, step := range rows {
						if step.Ordinal != want[step.Burn+"/"+step.Step] {
							t.Fatalf("poll %d: %s/%s ordinal %d: %+v", poll, step.Burn, step.Step, step.Ordinal, rows)
						}
					}
				}
			}
		})
	}
}

func TestWaitResumedLegacyRetryKeyKeepsSiblingOrdinal(t *testing.T) {
	for _, planned := range []bool{false, true} {
		t.Run(fmt.Sprint("plan=", planned), func(t *testing.T) {
			logs, err := runlog.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			wf := dagRetryWorkflow(wfv1.WorkflowSucceeded,
				[2]wfv1.NodePhase{wfv1.NodeSucceeded, wfv1.NodeFailed},
				[2]wfv1.NodePhase{wfv1.NodeSucceeded, wfv1.NodeSucceeded})
			if planned {
				plan, err := argoworkflow.PlannedSteps(wf)
				if err != nil {
					t.Fatal(err)
				}
				var journalPlan []runprogress.PlannedStep
				for _, step := range plan {
					journalPlan = append(journalPlan, runprogress.PlannedStep{Burn: step.Burn, Step: step.Step, Ordinal: step.Ordinal})
				}
				if err := logs.WriteStepPlan("run-retry", journalPlan); err != nil {
					t.Fatal(err)
				}
			}
			at := time.Unix(1000, 0).UTC()
			for _, event := range []runprogress.Event{
				{Version: 1, Burn: "address(0)", Step: "check", Ordinal: 0, Status: runprogress.StepFailed, StartedAt: &at, FinishedAt: &at},
				{Version: 1, Burn: "create", Step: "create", Ordinal: 1, Status: runprogress.StepPassed, StartedAt: &at, FinishedAt: &at},
			} {
				if err := logs.AppendStepProgress("run-retry", event); err != nil {
					t.Fatal(err)
				}
			}
			file, events, retained, err := logs.Resume("run-retry")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = file.Close() }()
			controller := &Controller{workflows: &staticWorkflowClient{workflow: wf},
				config: Config{Namespace: wf.Namespace, MaxRunLogBytes: 1 << 20}, interval: time.Millisecond}
			completion, err := controller.WaitResumed(context.Background(), wf.Name, "run-retry",
				journalMarkers{logs, "run-retry"}, events, retained, Adoption{})
			if err != nil || !completion.Succeeded {
				t.Fatalf("adoption completion: %+v: %v", completion, err)
			}
			if step := stepByKey(t, completion.Steps, "create/create"); step.Ordinal != 1 {
				t.Fatalf("completion moved the surviving sibling: %+v", step)
			}
			progress, err := logs.StepProgress("run-retry")
			if err != nil || len(progress) != 4 {
				t.Fatalf("resumed progress: %+v: %v", progress, err)
			}
			for _, event := range progress {
				if event.Burn == "address(0)" || (event.Burn == "create" && event.Ordinal != 1) {
					t.Fatalf("orphan or moved surviving sibling: %+v", event)
				}
			}
			if file, _, _, err := logs.Resume("run-retry"); err != nil {
				t.Fatalf("second adoption: %v", err)
			} else {
				_ = file.Close()
			}
		})
	}
}
