package store

import (
	"encoding/json"
	"github.com/oberthci/oberth/internal/measured"
	"sigs.k8s.io/yaml"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestRunResourcesDeadlineFlagBelongsToKilledStep(t *testing.T) {
	at := time.Now().UTC()
	s := testStore(t, &at)
	repo := createRepo(t, s)
	run, err := s.EnqueueRun(t.Context(), testRunSpec(repo.ID, "main", strings.Repeat("a", 40)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRunWorkflowName(t.Context(), run.ID, "workflow"); err != nil {
		t.Fatal(err)
	}
	passed := resourceRow(run.ID, at)
	passed.WorkflowName = "workflow"
	passed.Step = "passed-step"
	passed.TemplateName = "passed-step"
	passed.NodeID = "passed-node"
	passed.PodName = "passed-pod"
	passed.Terminal = true
	passed.Reason = "Completed"
	zero := int32(0)
	passed.ExitCode = &zero
	passed.ContainerStartedAt = at.Add(-time.Second)
	passed.ContainerFinishedAt = at
	killed := passed
	killed.Step = "killed-step"
	killed.TemplateName = "killed-step"
	killed.NodeID = "killed-node"
	killed.PodName = "killed-pod"
	killed.PodUID = "33333333-3333-4333-8333-333333333333"
	killed.ContainerID = "containerd://" + strings.Repeat("c", 64)
	killed.Reason = "Error"
	code := int32(143)
	killed.ExitCode = &code
	if err := s.RecordStepResourceUsage(t.Context(), []model.StepResourceUsage{passed, killed}); err != nil {
		t.Fatal(err)
	}

	c := &model.StepTermination{RunID: run.ID, Namespace: killed.Namespace, WorkflowName: killed.WorkflowName, WorkflowUID: killed.WorkflowUID, NodeID: killed.NodeID, PodName: killed.PodName, PodUID: killed.PodUID, ContainerName: "main", ContainerID: killed.ContainerID, RestartCount: killed.RestartCount, Reason: "Error", PodReason: "DeadlineExceeded", ExitCode: 143, Signal: 15, StartedAt: killed.ContainerStartedAt, FinishedAt: at}
	if _, err := s.PutStepResult(t.Context(), model.StepResult{RunID: run.ID, Stage: killed.Stage, Step: killed.Step, Status: model.StepTimedOut, DeclaredSize: "M", Termination: &model.StepDiagnostic{Available: true, ObservedAt: &at, Container: c}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE runs SET status='failed',error='Workflow deadline exceeded' WHERE id=?`, run.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.RunResources(t.Context(), repo.ID, RunResourceFilter{Ref: "main", Runs: 10, IncludeFailed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Observations) != 2 {
		t.Fatalf("observations=%d", len(got.Observations))
	}
	for _, o := range got.Observations {
		if o.DeadlineKilled != (o.Usage.Step == "killed-step") {
			t.Fatalf("deadline flag crossed step boundary: %s deadline_killed=%t", o.Usage.Step, o.DeadlineKilled)
		}
	}

	for _, format := range []string{"json", "yaml"} {
		report := measured.Report{Format: format, IncludeFailed: true, Steps: measured.Build(got.Observations, nil)}
		body := []byte(report.MCPToolText())
		if format == "yaml" {
			body, err = yaml.YAMLToJSON(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		var exported measured.Report
		if err := json.Unmarshal(body, &exported); err != nil {
			t.Fatal(err)
		}
		if len(exported.Steps) != 2 {
			t.Fatalf("%s lost steps: %s", format, body)
		}
		for _, step := range exported.Steps {
			if step.Flags.DeadlineKilled != (step.Template == "killed-step") || step.Flags.DeadlineUnknown {
				t.Fatalf("%s exported wrong per-step flags: %s", format, body)
			}
		}
	}
}
