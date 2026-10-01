package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestNoPipelineError_NotExist(t *testing.T) {
	err := fmt.Errorf("open .oberth/build.yaml without escaping source root: %w",
		&os.PathError{Op: "openat", Path: ".oberth/build.yaml", Err: os.ErrNotExist})

	got := noPipelineError(err, periapsis.TriggerCI, "terraform")
	want := `repository "terraform" has no pipeline configuration (.oberth/build.yaml); run "oberth init" in the repository to generate one`
	if got.Error() != want {
		t.Fatalf("noPipelineError(ErrNotExist):\n  got:  %s\n  want: %s", got, want)
	}
}

func TestNoPipelineError_ReleaseFile(t *testing.T) {
	err := fmt.Errorf("open .oberth/release.yaml without escaping source root: %w",
		&os.PathError{Op: "openat", Path: ".oberth/release.yaml", Err: os.ErrNotExist})

	got := noPipelineError(err, periapsis.TriggerRelease, "oberth")
	if !strings.Contains(got.Error(), argoworkflow.ReleaseFile) {
		t.Fatalf("expected release file path in error, got: %s", got)
	}
	if !strings.Contains(got.Error(), `"oberth"`) {
		t.Fatalf("expected repo name in error, got: %s", got)
	}
}

func TestNoPipelineError_OtherError(t *testing.T) {
	original := fmt.Errorf("permission denied")
	got := noPipelineError(original, periapsis.TriggerCI, "terraform")
	if got.Error() != original.Error() {
		t.Fatalf("non-ErrNotExist should pass through unchanged, got: %v", got)
	}
}

func TestPipelineSize_MissingFile(t *testing.T) {
	dir := t.TempDir()
	// Empty directory — no .oberth/build.yaml exists.

	jobs := &ArgoJobs{intents: map[string]argoIntent{}}
	_, err := jobs.PipelineSize(context.Background(), service.JobRequest{
		Run:        model.Run{Ref: "refs/heads/main"},
		Repository: model.Repository{Name: "terraform"},
		SourceDir:  dir,
	})
	if err == nil {
		t.Fatal("PipelineSize should fail when pipeline file is missing")
	}

	want := `repository "terraform" has no pipeline configuration (.oberth/build.yaml); run "oberth init" in the repository to generate one`
	if err.Error() != want {
		t.Fatalf("PipelineSize error:\n  got:  %s\n  want: %s", err, want)
	}
}

func TestPipelineSize_ValidFile(t *testing.T) {
	dir := t.TempDir()
	oberthDir := filepath.Join(dir, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Minimal valid Argo Workflow with size annotation.
	workflow := `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: M
spec:
  entrypoint: main
  templates:
    - name: main
      steps:
        - - name: test
            template: echo
    - name: echo
      container:
        image: alpine:3.20
        command: ["echo", "ok"]
`
	if err := os.WriteFile(filepath.Join(oberthDir, "build.yaml"), []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}

	jobs := &ArgoJobs{intents: map[string]argoIntent{}}
	size, err := jobs.PipelineSize(context.Background(), service.JobRequest{
		Run:        model.Run{Ref: "refs/heads/main"},
		Repository: model.Repository{Name: "terraform"},
		SourceDir:  dir,
	})
	if err != nil {
		t.Fatalf("PipelineSize should succeed with valid file: %v", err)
	}
	if size != periapsis.M {
		t.Fatalf("expected size M, got %s", size)
	}
}

// TestAuditSubmissionRecordsExecutedCommitNotTagObject verifies that when
// Run.SHA (tag object) differs from Run.TestedSHA (peeled commit), the audit
// record attributes execution to the commit that was actually checked out,
// while preserving the tag object OID for provenance.
func TestAuditSubmissionRecordsExecutedCommitNotTagObject(t *testing.T) {
	const tagObjectSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const commitSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	dir := t.TempDir()
	oberthDir := filepath.Join(dir, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: M
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: ["echo", "ok"]
`
	if err := os.WriteFile(filepath.Join(oberthDir, "release.yaml"), []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}

	var captured model.AuditActionSpec
	auditor := &stubAuditor{onAppend: func(spec model.AuditActionSpec) {
		captured = spec
	}}
	config := argojob.Config{
		Namespace:                  "oberth-pipeline",
		PipelineServiceAccount:     "oberth-pipeline",
		CredentialedServiceAccount: "oberth-credentialed",
		CISecretsServiceAccount:    "oberth-ci-secrets",
		ExecutorServiceAccount:     "oberth-executor",
	}
	jobs := &ArgoJobs{auditor: auditor, config: config, intents: map[string]argoIntent{}}

	request := service.JobRequest{
		Run: model.Run{
			ID: "run-test-audit", Ref: "refs/tags/v1.0.0",
			SHA: tagObjectSHA, TestedSHA: commitSHA,
			Actor: "agent@tuxbox", Release: true,
		},
		Repository: model.Repository{Name: "oberth"},
		SourceDir:  dir,
	}
	// Build the submission the same way create() does.
	source, err := readArgoSource(dir, periapsis.TriggerRelease)
	if err != nil {
		t.Fatal(err)
	}
	submission := argojob.Request{
		RunID: request.Run.ID, Name: "test-workflow",
		Repo: "oberth", Ref: request.Run.Ref,
		SHA: commitSHA, Trigger: periapsis.TriggerRelease, Source: source,
		SourceDir: dir, ApprovedSecrets: map[string]bool{},
	}
	if err := jobs.auditSubmission(context.Background(), request, submission); err != nil {
		t.Fatalf("auditSubmission: %v", err)
	}

	var details map[string]any
	if err := json.Unmarshal([]byte(captured.Details), &details); err != nil {
		t.Fatalf("unmarshal audit details: %v", err)
	}

	// "sha" must be the executed commit (submission.SHA = TestedSHA).
	if got, ok := details["sha"].(string); !ok || got != commitSHA {
		t.Fatalf("audit sha = %v, want %s (the executed commit)", details["sha"], commitSHA)
	}
	// "object_sha" must preserve the tag object for provenance.
	if got, ok := details["object_sha"].(string); !ok || got != tagObjectSHA {
		t.Fatalf("audit object_sha = %v, want %s (the tag object)", details["object_sha"], tagObjectSHA)
	}
}

type stubAuditor struct {
	onAppend func(model.AuditActionSpec)
}

type orderedNonrootController struct {
	argoControl
	prepare func(argojob.Request) (argojob.Request, error)
	create  func(argojob.Request) (string, error)
}

func (controller *orderedNonrootController) PrepareNonroot(_ context.Context, request argojob.Request) (argojob.Request, error) {
	return controller.prepare(request)
}

func (controller *orderedNonrootController) Create(_ context.Context, request argojob.Request) (string, error) {
	return controller.create(request)
}

func TestNonrootPreparationPrecedesAuditAndCreation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		fixture := newRaceFixture(t)
		prepared, audited, created := false, false, false
		fixture.jobs.controller = &orderedNonrootController{argoControl: fixture.controller,
			prepare: func(request argojob.Request) (argojob.Request, error) {
				if audited || created {
					t.Fatal("preparation occurred after audit/create")
				}
				prepared = true
				if fail {
					return argojob.Request{}, errors.New("controlled profile rejection")
				}
				return request, nil
			},
			create: func(request argojob.Request) (string, error) {
				if !prepared || !audited {
					t.Fatal("create preceded prepared audit")
				}
				created = true
				return request.Name, nil
			},
		}
		fixture.jobs.auditor = &stubAuditor{onAppend: func(model.AuditActionSpec) {
			if !prepared || created {
				t.Fatal("audit is not between preparation and create")
			}
			audited = true
		}}
		err := fixture.jobs.CreateCI(t.Context(), fixture.request("wf-nonroot-order", "run-nonroot-order"))
		if (err != nil) != fail || !prepared || audited == fail || created == fail {
			t.Fatalf("failed=%v prepared=%v audited=%v created=%v error=%v", fail, prepared, audited, created, err)
		}
	}
}

func TestSelectedNonrootWithoutProofCannotBeAudited(t *testing.T) {
	fixture := newRaceFixture(t)
	path := filepath.Join(fixture.sourceDir, ".oberth", "build.yaml")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source = []byte(strings.Replace(string(source), "oberth.ci/size: M", "oberth.ci/size: M\n    oberth.ci/nonroot-templates: main", 1))
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.jobs.config.NonrootProfile = argoworkflow.NonrootStaticProfile
	fixture.jobs.auditor = &stubAuditor{onAppend: func(model.AuditActionSpec) { t.Fatal("unproved nonroot submission audited") }}
	if err := fixture.jobs.CreateCI(t.Context(), fixture.request("wf-no-proof", "run-no-proof")); err == nil {
		t.Fatal("nonroot declaration manufactured proof")
	}
	if len(fixture.controller.created) != 0 {
		t.Fatal("unproved nonroot submitted")
	}
}

func (a *stubAuditor) AppendAuditAction(_ context.Context, spec model.AuditActionSpec) (model.AuditAction, error) {
	if a.onAppend != nil {
		a.onAppend(spec)
	}
	return model.AuditAction{ID: 1}, nil
}
