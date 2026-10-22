package argojob

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

// Follow declared rows through actual retry selection, log replay, and a real
// journal. In particular, attempt zero is still a retry attempt when it passes
// on the first try. Restored reporters must retain pre-fix journal ordinals.
func TestDeclaredRetryRowsResolveThroughWait(t *testing.T) {
	for _, test := range []struct {
		name     string
		attempts [][2]wfv1.NodePhase
		phase    wfv1.WorkflowPhase
		want     runprogress.Status
	}{
		{"first-attempt-passes", [][2]wfv1.NodePhase{{wfv1.NodeSucceeded, wfv1.NodeSucceeded}}, wfv1.WorkflowSucceeded, runprogress.StepPassed},
		{"fail-then-pass", [][2]wfv1.NodePhase{{wfv1.NodeSucceeded, wfv1.NodeFailed}, {wfv1.NodeSucceeded, wfv1.NodeSucceeded}}, wfv1.WorkflowSucceeded, runprogress.StepPassed},
		{"exhausted", [][2]wfv1.NodePhase{{wfv1.NodeSucceeded, wfv1.NodeFailed}, {wfv1.NodeSucceeded, wfv1.NodeFailed}}, wfv1.WorkflowFailed, runprogress.StepFailed},
	} {
		for _, restored := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/restored=%t", test.name, restored), func(t *testing.T) {
				wf := dagRetryWorkflow(test.phase, test.attempts...)
				// Neither a Hooked child with an attempt-shaped name nor an
				// unrelated child at the tail may replace the real attempt.
				retry := wf.Status.Nodes["address"]
				retry.Children = append(retry.Children, "hook", "unrelated")
				wf.Status.Nodes["address"] = retry
				wf.Status.Nodes["hook"] = wfv1.NodeStatus{ID: "hook", Name: "wf.address(99)",
					Type: wfv1.NodeTypeDAG, NodeFlag: &wfv1.NodeFlag{Hooked: true}}
				wf.Status.Nodes["unrelated"] = wfv1.NodeStatus{ID: "unrelated", Name: "wf.other(100)", Type: wfv1.NodeTypeDAG}
				latest := fmt.Sprintf("address-%d-check", len(test.attempts)-1)
				last := wf.Status.Nodes[latest]
				code := "0"
				if test.want == runprogress.StepFailed {
					code = "42"
				}
				last.Outputs = &wfv1.Outputs{ExitCode: &code}
				wf.Status.Nodes[latest] = last
				logs, err := runlog.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				planned, err := argoworkflow.PlannedSteps(wf)
				if err != nil {
					t.Fatal(err)
				}
				var plan []runprogress.PlannedStep
				for _, step := range planned {
					plan = append(plan, runprogress.PlannedStep{Burn: step.Burn, Step: step.Step, Ordinal: step.Ordinal})
				}
				if err := logs.WriteStepPlan("run-retry", plan); err != nil {
					t.Fatal(err)
				}
				if restored {
					at := last.StartedAt.Time
					for _, event := range []runprogress.Event{
						{Version: 1, Burn: "address(0)", Step: "check", Ordinal: 0, Status: runprogress.StepFailed, ExitCode: 1, StartedAt: &at, FinishedAt: &at},
						{Version: 1, Burn: "create", Step: "create", Ordinal: 7, Status: runprogress.StepPassed, StartedAt: &at, FinishedAt: &at},
						{Version: 1, Burn: "address", Step: "check", Ordinal: 26, Status: runprogress.StepQueued},
					} {
						if err := logs.AppendStepProgress("run-retry", event); err != nil {
							t.Fatal(err)
						}
					}
				}
				file, events, retained, err := logs.Resume("run-retry")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = file.Close() }()
				controller := &Controller{workflows: &staticWorkflowClient{workflow: wf},
					config: Config{Namespace: wf.Namespace, MaxRunLogBytes: 1 << 20}, interval: time.Millisecond,
					logs: func(_ context.Context, _, pod, _ string) (io.ReadCloser, error) {
						return io.NopCloser(strings.NewReader("output of " + pod + "\n")), nil
					}}
				completion, err := controller.WaitResumed(context.Background(), wf.Name, "run-retry",
					io.MultiWriter(file, journalMarkers{logs, "run-retry"}), events, retained, Adoption{})
				if err != nil || completion.Succeeded != (test.want == runprogress.StepPassed) {
					t.Fatalf("completion: %+v: %v", completion, err)
				}
				check := stepByKey(t, completion.Steps, "address/check")
				if check.NodeID != latest || check.Status != test.want || check.ExitCode != exitCode(last) ||
					check.StartedAt == nil || !check.StartedAt.Equal(last.StartedAt.Time) ||
					check.FinishedAt == nil || !check.FinishedAt.Equal(last.FinishedAt.Time) {
					t.Fatalf("declared outcome differs from latest real attempt: %+v", check)
				}
				progress, err := logs.StepProgress("run-retry")
				if err != nil {
					t.Fatal(err)
				}
				found := false
				for _, event := range progress {
					if event.Burn == "address" && event.Step == "check" {
						found = true
						if event.Status != test.want || (restored && event.Ordinal != 26) {
							t.Fatalf("unresolved or renumbered declaration: %+v", event)
						}
					}
				}
				if !found {
					t.Fatal("declared check has no progress")
				}
				if restored && stepByKey(t, completion.Steps, "create/create").Ordinal != 7 {
					t.Fatal("adoption renumbered a sibling")
				}
				if _, err := logs.BuildIndex("run-retry"); err != nil {
					t.Fatal(err)
				}
				body, err := logs.Read("run-retry", "address", "check")
				if err != nil || !strings.Contains(string(body), "output of "+PodName(wf, last)) {
					t.Fatalf("declared log range lacks latest attempt: %s: %v", body, err)
				}
				if file, _, _, err := logs.Resume("run-retry"); err != nil {
					t.Fatalf("second adoption: %v", err)
				} else {
					_ = file.Close()
				}
			})
		}
	}
}
