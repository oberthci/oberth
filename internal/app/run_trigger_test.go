package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestRunKindReadsOnlyTheDurableRun(t *testing.T) {
	for _, testCase := range []struct {
		run  model.Run
		want argoworkflow.RunTrigger
	}{
		{model.Run{Trigger: "branch"}, argoworkflow.RunTriggerBranch},
		{model.Run{Trigger: "promotion"}, argoworkflow.RunTriggerPromotion},
		{model.Run{Trigger: "schedule"}, argoworkflow.RunTriggerSchedule},
		{model.Run{Trigger: "tag", Release: true}, argoworkflow.RunTriggerTag},
		// Release decides the tier even when the column says otherwise.
		{model.Run{Trigger: "promotion", Release: true}, argoworkflow.RunTriggerTag},
		// Legacy and test rows ("push", empty) are branch runs, as the store
		// has always treated every non-promotion branch trigger.
		{model.Run{Trigger: "push"}, argoworkflow.RunTriggerBranch},
		{model.Run{}, argoworkflow.RunTriggerBranch},
	} {
		if got, err := runKind(testCase.run); err != nil || got != testCase.want {
			t.Fatalf("runKind(%+v) = %q, %v, want %q", testCase.run, got, err, testCase.want)
		}
	}
	// Go-path and unknown triggers are refused, never sized as a branch.
	for _, trigger := range []string{"plan", "apply", "nightly"} {
		if got, err := runKind(model.Run{Trigger: trigger}); err == nil {
			t.Fatalf("runKind(%q) = %q, want a refusal", trigger, got)
		}
	}
	jobs := &ArgoJobs{intents: map[string]argoIntent{}}
	if _, err := jobs.PipelineSize(context.Background(), service.WorkflowRequest{
		Run: model.Run{Trigger: "apply"}, Repository: model.Repository{Name: "infra"}, SourceDir: writeBuild(t, perTriggerBuild),
	}); err == nil {
		t.Fatal("PipelineSize sized an apply run")
	}
}

const perTriggerBuild = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    example.com/tier: XL
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: ["echo", "ok"]
`

func writeBuild(t *testing.T, document string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".oberth"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".oberth", "build.yaml"), []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCreateWorkflowStampsTheRunsOwnTrigger(t *testing.T) {
	fixture := newRaceFixture(t)
	for name, trigger := range map[string]string{"wf-branch": "branch", "wf-promotion": "promotion", "wf-schedule": "schedule"} {
		request := fixture.request(name, "run-"+name)
		request.Run.Trigger = trigger
		if trigger == "schedule" {
			request.Run.ScheduleEntry = "weekly-sweep"
		}
		if err := fixture.jobs.CreateWorkflow(context.Background(), request); err != nil {
			t.Fatalf("CreateCI %s: %v", trigger, err)
		}
		if got := fixture.controller.scheduleEntries[name]; got != request.Run.ScheduleEntry {
			t.Fatalf("%s run submitted entry %q, want %q", trigger, got, request.Run.ScheduleEntry)
		}
		if got := fixture.controller.runTriggers[name]; string(got) != trigger {
			t.Fatalf("%s run submitted RunTrigger %q", trigger, got)
		}
	}
}
