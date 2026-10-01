package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func groupedPipeline(annotations string) string {
	return `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: M
` + annotations + `spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: ["echo", "ok"]
`
}

func writePipeline(t *testing.T, dir, file, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".oberth"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".oberth", file), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The group is read from the document the run executes: build.yaml for branch
// and promotion runs, release.yaml for tags, exactly as the size is.
func TestPipelineConcurrencyGroupReadsTheRunsOwnDocument(t *testing.T) {
	dir := t.TempDir()
	writePipeline(t, dir, "build.yaml", groupedPipeline("    oberth.ci/concurrency-group: terraform-state\n"))
	writePipeline(t, dir, "release.yaml", groupedPipeline("    oberth.ci/concurrency-group: release-lane\n"))
	jobs := &ArgoJobs{intents: map[string]argoIntent{}}

	for _, tc := range []struct {
		run  model.Run
		want string
	}{
		{model.Run{Ref: "fix-a", Trigger: "branch"}, "terraform-state"},
		{model.Run{Ref: "promotion/main", Trigger: "promotion"}, "terraform-state"},
		{model.Run{Ref: "apply-1", Trigger: "tag", Release: true}, "release-lane"},
	} {
		group, err := jobs.PipelineConcurrencyGroup(context.Background(), service.JobRequest{
			Run: tc.run, Repository: model.Repository{Name: "terraform"}, SourceDir: dir,
		})
		if err != nil || group != tc.want {
			t.Fatalf("%s run group = %q, %v; want %q", tc.run.Trigger, group, err, tc.want)
		}
	}

	ungrouped := t.TempDir()
	writePipeline(t, ungrouped, "build.yaml", groupedPipeline(""))
	if group, err := jobs.PipelineConcurrencyGroup(context.Background(), service.JobRequest{
		Run: model.Run{Ref: "main"}, Repository: model.Repository{Name: "beacon"}, SourceDir: ungrouped,
	}); err != nil || group != "" {
		t.Fatalf("undeclared group = %q, %v", group, err)
	}
}

func TestPipelineConcurrencyGroupRefusesMalformedAndMissingDocuments(t *testing.T) {
	jobs := &ArgoJobs{intents: map[string]argoIntent{}}
	malformed := t.TempDir()
	writePipeline(t, malformed, "build.yaml", groupedPipeline("    oberth.ci/concurrency-group: Terraform_State\n"))
	if _, err := jobs.PipelineConcurrencyGroup(context.Background(), service.JobRequest{
		Run: model.Run{Ref: "main"}, Repository: model.Repository{Name: "terraform"}, SourceDir: malformed,
	}); err == nil || !strings.Contains(err.Error(), "invalid concurrency group") {
		t.Fatalf("malformed group = %v", err)
	}
	if _, err := jobs.PipelineConcurrencyGroup(context.Background(), service.JobRequest{
		Run: model.Run{Ref: "main"}, Repository: model.Repository{Name: "terraform"}, SourceDir: t.TempDir(),
	}); err == nil || !strings.Contains(err.Error(), "has no pipeline configuration") {
		t.Fatalf("missing document = %v", err)
	}
}

// The submission binding in the audit chain records the group the scheduler
// admitted the run under, and says nothing about groups for ungrouped runs.
func TestAuditSubmissionRecordsTheAdmittedConcurrencyGroup(t *testing.T) {
	dir := t.TempDir()
	writePipeline(t, dir, "build.yaml", groupedPipeline("    oberth.ci/concurrency-group: terraform-state\n"))
	var captured model.AuditActionSpec
	jobs := &ArgoJobs{
		auditor: &stubAuditor{onAppend: func(spec model.AuditActionSpec) { captured = spec }},
		config: argojob.Config{
			Namespace: "oberth-pipeline", PipelineServiceAccount: "oberth-pipeline",
			CredentialedServiceAccount: "oberth-credentialed", CISecretsServiceAccount: "oberth-ci-secrets",
			ExecutorServiceAccount: "oberth-executor",
		},
		intents: map[string]argoIntent{},
	}
	source, err := readArgoSource(dir, periapsis.TriggerCI)
	if err != nil {
		t.Fatal(err)
	}
	const commit = "cccccccccccccccccccccccccccccccccccccccc"
	submission := argojob.Request{
		RunID: "run-grouped", Name: "wf-grouped", Repo: "terraform", Ref: "fix-a",
		SHA: commit, Trigger: periapsis.TriggerCI, Source: source, SourceDir: dir, ApprovedSecrets: map[string]bool{},
	}
	for _, tc := range []struct {
		group string
		want  any
	}{
		{"terraform-state", "terraform-state"},
		{"", nil},
	} {
		request := service.JobRequest{
			Run:        model.Run{ID: "run-grouped", Ref: "fix-a", SHA: commit, Actor: "agent@tuxbox", ConcurrencyGroup: tc.group},
			Repository: model.Repository{Name: "terraform"}, SourceDir: dir, ConcurrencyGroup: tc.group,
		}
		if err := jobs.auditSubmission(context.Background(), request, submission); err != nil {
			t.Fatalf("auditSubmission: %v", err)
		}
		var details map[string]any
		if err := json.Unmarshal([]byte(captured.Details), &details); err != nil {
			t.Fatal(err)
		}
		if got := details["concurrency_group"]; got != tc.want {
			t.Fatalf("audited concurrency_group = %#v, want %#v (details %s)", got, tc.want, captured.Details)
		}
	}
}
