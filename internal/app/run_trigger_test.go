package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
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
		if got := runKind(testCase.run); got != testCase.want {
			t.Fatalf("runKind(%+v) = %q, want %q", testCase.run, got, testCase.want)
		}
	}
}

const perTriggerBuild = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: XL
    oberth.ci/size-by-trigger: "branch=M, promotion=XL"
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

func TestPipelineSizeIsPerRunTrigger(t *testing.T) {
	dir := writeBuild(t, perTriggerBuild)
	jobs := &ArgoJobs{intents: map[string]argoIntent{}}
	preflight := &TrustedPreflights{}
	for trigger, want := range map[string]periapsis.Size{
		"branch":    periapsis.M,
		"promotion": periapsis.XL,
		"schedule":  periapsis.XL, // unnamed: oberth.ci/size
	} {
		request := service.JobRequest{
			Run:        model.Run{Ref: "feature/x", Trigger: trigger},
			Repository: model.Repository{Name: "ebpf"},
			SourceDir:  dir,
		}
		size, err := jobs.PipelineSize(context.Background(), request)
		if err != nil || size != want {
			t.Fatalf("%s PipelineSize = %q, %v; want %q", trigger, size, err, want)
		}
		// The trusted preflight reads the same document and must not size a
		// promotion's full gate as a branch run.
		size, err = preflight.Size(context.Background(), request)
		if err != nil || size != want {
			t.Fatalf("%s preflight Size = %q, %v; want %q", trigger, size, err, want)
		}
	}
	// A tag run's preflight of build.yaml gates a release: promotion's size.
	size, err := preflight.Size(context.Background(), service.JobRequest{
		Run: model.Run{Ref: "v1.0.0", Trigger: "tag", Release: true}, Repository: model.Repository{Name: "ebpf"}, SourceDir: dir,
	})
	if err != nil || size != periapsis.XL {
		t.Fatalf("tag preflight Size = %q, %v; want XL", size, err)
	}
}

func TestPipelineSizeRefusesAMisdeclaredTrigger(t *testing.T) {
	dir := writeBuild(t, perTriggerBuild)
	if err := os.WriteFile(filepath.Join(dir, ".oberth", "build.yaml"),
		[]byte(strings.Replace(perTriggerBuild, "promotion=XL", "nightly=XL", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	jobs := &ArgoJobs{intents: map[string]argoIntent{}}
	if _, err := jobs.PipelineSize(context.Background(), service.JobRequest{
		Run: model.Run{Ref: "feature/x", Trigger: "branch"}, Repository: model.Repository{Name: "ebpf"}, SourceDir: dir,
	}); err == nil {
		t.Fatal("PipelineSize admitted an unknown trigger")
	}
}

func TestCreateCIStampsTheRunsOwnTrigger(t *testing.T) {
	fixture := newRaceFixture(t)
	for name, trigger := range map[string]string{"wf-branch": "branch", "wf-promotion": "promotion", "wf-schedule": "schedule"} {
		request := fixture.request(name, "run-"+name)
		request.Run.Trigger = trigger
		if err := fixture.jobs.CreateCI(context.Background(), request); err != nil {
			t.Fatalf("CreateCI %s: %v", trigger, err)
		}
		if got := fixture.controller.runTriggers[name]; string(got) != trigger {
			t.Fatalf("%s run submitted RunTrigger %q", trigger, got)
		}
	}
}
