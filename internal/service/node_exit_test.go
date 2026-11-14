package service

import (
	"encoding/json"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
)

func TestNativeExitAvailabilitySurvivesStoreAndRunReaders(t *testing.T) {
	for _, input := range []string{"missing", "malformed", "0", "42"} {
		t.Run(input, func(t *testing.T) {
			f := newControlFixture(t)
			run, err := f.scheduler.EnqueueCI(t.Context(), CIRequest{EventID: "native-exit", Repository: f.repo, Branch: "exit", SHA: strings.Repeat("a", 40), Actor: "reader"})
			if err != nil {
				t.Fatal(err)
			}
			node := wfv1.NodeStatus{ID: "node", Name: "leaf", DisplayName: "leaf", Type: wfv1.NodeTypePod, Phase: wfv1.NodeError}
			if input != "missing" {
				node.Outputs = &wfv1.Outputs{ExitCode: &input}
			}
			workflow := &wfv1.Workflow{Status: wfv1.WorkflowStatus{Nodes: wfv1.Nodes{"node": node}}}
			rows := argojob.NodeStepResults(workflow, nil)
			unknown := input == "missing" || input == "malformed"
			if len(rows) != 1 || rows[0].ExitCodeUnknown != unknown {
				t.Fatal("native availability lost", rows)
			}
			if err = f.store.PutStepResults(t.Context(), run.ID, rows); err != nil {
				t.Fatal(err)
			}
			value, err := f.api(t).Run(t.Context(), api.Actor{Identity: "reader"}, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			detail := value.(RunDetailResponse)
			if len(detail.Steps) != 1 || detail.Steps[0].ExitCodeUnknown != unknown {
				t.Fatal("stored availability lost", detail.Steps)
			}
			_, exit := wireStages(detail.Steps, model.Run{FailedStep: "leaf"})
			want := 0
			if input == "42" {
				want = 42
			}
			if (exit == nil) != unknown || (exit != nil && *exit != want) {
				t.Fatal("status invented or lost an exit", exit)
			}
			raw, err := json.Marshal(detail)
			if err != nil {
				t.Fatal(err)
			}
			var object struct{ Steps []map[string]any }
			if err = json.Unmarshal(raw, &object); err != nil {
				t.Fatal(err)
			}
			code, present := object.Steps[0]["ExitCode"]
			if present == unknown || (present && code != float64(want)) {
				t.Fatal("run detail invented or lost an exit", string(raw))
			}
			var decoded RunDetailResponse
			if err = json.Unmarshal(raw, &decoded); err != nil || decoded.Steps[0].ExitCodeUnknown != unknown {
				t.Fatal("JSON round trip lost availability", err)
			}
			// Learning an actual exit later replaces the earlier unavailable observation.
			if unknown {
				rows[0].ExitCode, rows[0].ExitCodeUnknown = 42, false
				if err = f.store.PutStepResults(t.Context(), run.ID, rows); err != nil {
					t.Fatal(err)
				}
				stored, err := f.store.StepResults(t.Context(), run.ID)
				if err != nil || stored[0].ExitCodeUnknown || stored[0].ExitCode != 42 {
					t.Fatal("observed exit update lost", stored, err)
				}
			}
		})
	}
}
