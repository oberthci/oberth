package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// ---------------------------------------------------------------------------
// DeclarationRecorder wiring tests (issue #623, finding 4)
// ---------------------------------------------------------------------------

// fakeDeclarationRecorder captures the arguments passed to RecordGrantDeclarations.
type fakeDeclarationRecorder struct {
	repo         string
	sha          string
	declarations []store.GrantDeclaration
	called       bool
	err          error // returned by RecordGrantDeclarations when non-nil
}

func (r *fakeDeclarationRecorder) RecordGrantDeclarations(_ context.Context, repo, sha string, declarations []store.GrantDeclaration) error {
	r.called = true
	r.repo = repo
	r.sha = sha
	r.declarations = declarations
	return r.err
}

// writeWorkflowSource writes a build.yaml to a temp directory and returns
// the source bytes and the directory path.
func writeWorkflowSource(t *testing.T, content string) ([]byte, string) {
	t.Helper()
	dir := t.TempDir()
	oberthDir := filepath.Join(dir, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := []byte(content)
	if err := os.WriteFile(filepath.Join(oberthDir, "build.yaml"), source, 0o644); err != nil {
		t.Fatal(err)
	}
	return source, dir
}

// declarationWorkflow has:
//   - annotation "oberth.ci/secret-paths" declaring two paths
//   - template "publish" using `oberth secretstore exec --path=oberth/data/release/r2-token`
//     (this path is also in the annotation)
//   - annotation path "oberth/data/release/cosign-key" NOT consumed by any template
//     → should appear as ("*", "oberth/data/release/cosign-key")
const declarationWorkflow = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: M
    oberth.ci/secret-paths: "oberth/data/release/r2-token,oberth/data/release/cosign-key"
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      dag:
        tasks:
          - name: build
            template: build-step
          - name: publish
            template: publish-step
            dependencies: [build]
    - name: build-step
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: ["go", "build", "./..."]
    - name: publish-step
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: ["/run/oberth/bin/oberth"]
        args:
          - secretstore
          - exec
          - "--path=oberth/data/release/r2-token"
          - "--"
          - "publish.sh"
`

func TestRecordDeclarations_TemplateAndAnnotationPaths(t *testing.T) {
	t.Parallel()

	recorder := &fakeDeclarationRecorder{}
	source, dir := writeWorkflowSource(t, declarationWorkflow)

	jobs := &ArgoJobs{declarations: recorder, intents: map[string]argoIntent{}}

	request := service.JobRequest{
		Run: model.Run{
			ID: "run-decl-01", Ref: "refs/heads/main",
			SHA: "cccccccccccccccccccccccccccccccccccccccc", Actor: "agent@host",
		},
		Repository: model.Repository{Name: "codeberg/acme/widget"},
		SourceDir:  dir,
	}

	jobs.recordDeclarations(context.Background(), request, source, periapsis.TriggerCI)

	if !recorder.called {
		t.Fatal("RecordGrantDeclarations was not called")
	}
	if recorder.repo != "codeberg/acme/widget" {
		t.Fatalf("repo = %q, want %q", recorder.repo, "codeberg/acme/widget")
	}
	if recorder.sha != "cccccccccccccccccccccccccccccccccccccccc" {
		t.Fatalf("sha = %q, want %q", recorder.sha, "cccccccccccccccccccccccccccccccccccccccc")
	}

	// Expected declarations:
	// (a) "publish-step" -> "oberth/data/release/r2-token" (template exec --path)
	// (b) "*" -> "oberth/data/release/cosign-key" (annotation-only)
	if len(recorder.declarations) != 2 {
		t.Fatalf("declarations count = %d, want 2: %+v", len(recorder.declarations), recorder.declarations)
	}

	// The template-sourced declaration comes first.
	d0 := recorder.declarations[0]
	if d0.Step != "publish-step" || d0.Path != "oberth/data/release/r2-token" {
		t.Fatalf("declaration[0] = {Step:%q, Path:%q}, want {publish-step, oberth/data/release/r2-token}",
			d0.Step, d0.Path)
	}

	// The annotation-only declaration has wildcard step.
	d1 := recorder.declarations[1]
	if d1.Step != "*" || d1.Path != "oberth/data/release/cosign-key" {
		t.Fatalf("declaration[1] = {Step:%q, Path:%q}, want {*, oberth/data/release/cosign-key}",
			d1.Step, d1.Path)
	}
}

func TestRecordDeclarations_RecorderErrorDoesNotBlockAdmission(t *testing.T) {
	t.Parallel()

	recorder := &fakeDeclarationRecorder{err: errors.New("database write failed")}
	source, dir := writeWorkflowSource(t, declarationWorkflow)

	jobs := &ArgoJobs{declarations: recorder, intents: map[string]argoIntent{}}

	request := service.JobRequest{
		Run: model.Run{
			ID: "run-decl-err", Ref: "refs/heads/main",
			SHA: "dddddddddddddddddddddddddddddddddddddddd", Actor: "agent@host",
		},
		Repository: model.Repository{Name: "test-repo"},
		SourceDir:  dir,
	}

	// recordDeclarations is best-effort; it must not panic or propagate errors.
	jobs.recordDeclarations(context.Background(), request, source, periapsis.TriggerCI)

	if !recorder.called {
		t.Fatal("RecordGrantDeclarations was not called despite source having declarations")
	}
}

func TestRecordDeclarations_NilRecorderIsNoOp(t *testing.T) {
	t.Parallel()

	source, dir := writeWorkflowSource(t, declarationWorkflow)

	// declarations field is nil (SetDeclarationRecorder was never called).
	jobs := &ArgoJobs{intents: map[string]argoIntent{}}

	request := service.JobRequest{
		Run: model.Run{
			ID: "run-no-recorder", Ref: "refs/heads/main",
			SHA: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", Actor: "agent@host",
		},
		Repository: model.Repository{Name: "test-repo"},
		SourceDir:  dir,
	}

	// Must not panic when declarations is nil.
	jobs.recordDeclarations(context.Background(), request, source, periapsis.TriggerCI)
}

func TestRecordDeclarations_NoSecretPaths(t *testing.T) {
	t.Parallel()

	recorder := &fakeDeclarationRecorder{}

	// A workflow with no secret-paths annotation and no secretstore exec commands.
	noSecretsWorkflow := `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: S
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      container:
        image: alpine:3.20
        command: ["echo", "ok"]
`
	source, dir := writeWorkflowSource(t, noSecretsWorkflow)

	jobs := &ArgoJobs{declarations: recorder, intents: map[string]argoIntent{}}

	request := service.JobRequest{
		Run: model.Run{
			ID: "run-no-secrets", Ref: "refs/heads/main",
			SHA: "ffffffffffffffffffffffffffffffffffffffff", Actor: "agent@host",
		},
		Repository: model.Repository{Name: "test-repo"},
		SourceDir:  dir,
	}

	jobs.recordDeclarations(context.Background(), request, source, periapsis.TriggerCI)

	if recorder.called {
		t.Fatal("RecordGrantDeclarations should not be called when no paths are declared")
	}
}
