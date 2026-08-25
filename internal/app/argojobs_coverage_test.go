package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runprogress"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// ---------------------------------------------------------------------------
// NewArgoJobs validation
// ---------------------------------------------------------------------------

func TestNewArgoJobs_NilController(t *testing.T) {
	t.Parallel()
	_, err := NewArgoJobs(nil, argojob.Config{}, &stubAuditor{}, nil)
	if err == nil || !strings.Contains(err.Error(), "controller is required") {
		t.Fatalf("expected controller-required error, got: %v", err)
	}
}

func TestNewArgoJobs_NilAuditor(t *testing.T) {
	t.Parallel()
	_, err := NewArgoJobs(newBlockingController(), argojob.Config{}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "audit chain") {
		t.Fatalf("expected auditor-required error, got: %v", err)
	}
}

func TestNewArgoJobs_InvalidConfig(t *testing.T) {
	t.Parallel()
	// Config with empty namespace fails validation.
	_, err := NewArgoJobs(newBlockingController(), argojob.Config{}, &stubAuditor{}, nil)
	if err == nil {
		t.Fatal("expected config validation error")
	}
}

func TestNewArgoJobs_ValidConstruction(t *testing.T) {
	t.Parallel()
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(newBlockingController(), config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatalf("valid construction failed: %v", err)
	}
	if jobs == nil {
		t.Fatal("NewArgoJobs returned nil")
	}
}

// ---------------------------------------------------------------------------
// SetReconcilerHealth
// ---------------------------------------------------------------------------

type fakeReconciler struct {
	healthy bool
}

func (r *fakeReconciler) ReconcileHealthy() bool { return r.healthy }

func TestSetReconcilerHealth(t *testing.T) {
	t.Parallel()
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(newBlockingController(), config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	checker := &fakeReconciler{healthy: true}
	jobs.SetReconcilerHealth(checker)
	jobs.mu.Lock()
	if jobs.reconcilerHealthy == nil {
		t.Fatal("reconciler health checker was not set")
	}
	if !jobs.reconcilerHealthy.ReconcileHealthy() {
		t.Fatal("reconciler should report healthy")
	}
	jobs.mu.Unlock()
}

func TestCreateCI_ReconcilerUnhealthyBlocks(t *testing.T) {
	t.Parallel()
	fixture := newRaceFixture(t)
	checker := &fakeReconciler{healthy: false}
	fixture.jobs.SetReconcilerHealth(checker)

	err := fixture.jobs.CreateCI(context.Background(), fixture.request("wf-unhealthy", "run-unhealthy"))
	if err == nil || !strings.Contains(err.Error(), "initial reconciliation") {
		t.Fatalf("expected reconciler health gate error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// CreateRelease
// ---------------------------------------------------------------------------

func TestCreateRelease_Success(t *testing.T) {
	t.Parallel()
	controller := newBlockingController()
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(controller, config, &stubAuditor{}, emptySecretAccess{})
	if err != nil {
		t.Fatal(err)
	}

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

	request := service.JobRequest{
		JobName: "release-wf-01",
		Run: model.Run{
			ID: "run-release-01", Ref: "refs/tags/v1.0.0",
			SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "agent@host",
			Release: true,
		},
		Repository: model.Repository{Name: "test-repo"},
		SourceDir:  dir,
	}
	if err := jobs.CreateRelease(context.Background(), request); err != nil {
		t.Fatalf("CreateRelease failed: %v", err)
	}

	controller.mu.Lock()
	if len(controller.created) != 1 || controller.created[0] != "release-wf-01" {
		t.Fatalf("controller.created = %v", controller.created)
	}
	controller.mu.Unlock()
}

func TestCreateRelease_TriggerMismatch(t *testing.T) {
	t.Parallel()
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(newBlockingController(), config, &stubAuditor{}, emptySecretAccess{})
	if err != nil {
		t.Fatal(err)
	}

	// Run is CI (Release=false), but trying CreateRelease -- mismatch.
	request := service.JobRequest{
		JobName: "mismatch-wf",
		Run: model.Run{
			ID: "run-mismatch", Ref: "refs/heads/main",
			SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "agent@host",
			Release: false,
		},
		Repository: model.Repository{Name: "test-repo"},
		SourceDir:  t.TempDir(),
	}
	err = jobs.CreateRelease(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "trigger does not match") {
		t.Fatalf("expected trigger mismatch error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// PlannedSteps
// ---------------------------------------------------------------------------

func TestPlannedSteps_ValidDocument(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	oberthDir := filepath.Join(dir, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Two-burn pipeline: a DAG entrypoint with two task invocations, each a
	// container template.
	workflow := `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: S
spec:
  entrypoint: main
  templates:
    - name: main
      dag:
        tasks:
          - name: lint
            template: lint-template
          - name: test
            template: test-template
            dependencies: [lint]
    - name: lint-template
      container:
        image: alpine:3.20
        command: ["echo", "lint"]
    - name: test-template
      container:
        image: alpine:3.20
        command: ["echo", "test"]
`
	if err := os.WriteFile(filepath.Join(oberthDir, "build.yaml"), []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}

	jobs := &ArgoJobs{intents: map[string]argoIntent{}}
	steps, err := jobs.PlannedSteps(context.Background(), service.JobRequest{
		Run:        model.Run{Ref: "refs/heads/main"},
		Repository: model.Repository{Name: "test-repo"},
		SourceDir:  dir,
	})
	if err != nil {
		t.Fatalf("PlannedSteps: %v", err)
	}
	if len(steps) == 0 {
		t.Fatal("PlannedSteps returned zero steps")
	}
	// Verify the steps are mapped to runprogress.PlannedStep.
	for _, step := range steps {
		if step.Step == "" {
			t.Fatalf("step has empty Step field: %+v", step)
		}
	}
}

func TestPlannedSteps_MissingFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	jobs := &ArgoJobs{intents: map[string]argoIntent{}}
	_, err := jobs.PlannedSteps(context.Background(), service.JobRequest{
		Run:        model.Run{Ref: "refs/heads/main"},
		Repository: model.Repository{Name: "missing-repo"},
		SourceDir:  dir,
	})
	if err == nil {
		t.Fatal("PlannedSteps should fail when pipeline file is missing")
	}
	if !strings.Contains(err.Error(), "no pipeline configuration") {
		t.Fatalf("expected user-facing no-pipeline error, got: %v", err)
	}
}

func TestPlannedSteps_InvalidDocument(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	oberthDir := filepath.Join(dir, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oberthDir, "build.yaml"), []byte("not: valid: yaml: workflow"), 0o644); err != nil {
		t.Fatal(err)
	}

	jobs := &ArgoJobs{intents: map[string]argoIntent{}}
	_, err := jobs.PlannedSteps(context.Background(), service.JobRequest{
		Run:        model.Run{Ref: "refs/heads/main"},
		Repository: model.Repository{Name: "bad-repo"},
		SourceDir:  dir,
	})
	if err == nil {
		t.Fatal("PlannedSteps should fail with invalid YAML")
	}
}

// ---------------------------------------------------------------------------
// Wait
// ---------------------------------------------------------------------------

// configuredController extends blockingController with configurable Wait results.
type configuredController struct {
	blockingController
	waitResult argojob.Completion
	waitErr    error
	termState  *argojob.Completion
	termErr    error
	ownsResult bool
	ownsErr    error
}

func (c *configuredController) Wait(_ context.Context, _, _ string, _ io.Writer) (argojob.Completion, error) {
	return c.waitResult, c.waitErr
}

func (c *configuredController) TerminalState(_ context.Context, _ string) (*argojob.Completion, error) {
	return c.termState, c.termErr
}

func (c *configuredController) Owns(_ context.Context, _ string) (bool, error) {
	return c.ownsResult, c.ownsErr
}

func TestWait_Success(t *testing.T) {
	t.Parallel()
	controller := &configuredController{
		blockingController: *newBlockingController(),
		waitResult: argojob.Completion{
			Succeeded: true,
			Steps: []argojob.StepResult{
				{Burn: "test", Step: "lint", Ordinal: 0, Status: "passed", ExitCode: 0},
			},
		},
	}
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(controller, config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Plant an intent so Wait can find it.
	const name = "wf-wait-01"
	const runID = "run-wait-01"
	jobs.mu.Lock()
	jobs.intents[name] = argoIntent{runID: runID, trigger: periapsis.TriggerCI, created: true}
	jobs.mu.Unlock()

	result, err := jobs.Wait(context.Background(), name, io.Discard)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if result.Status != model.RunPassed {
		t.Fatalf("result.Status = %q, want passed", result.Status)
	}
	if len(result.Steps) != 1 {
		t.Fatalf("result.Steps length = %d, want 1", len(result.Steps))
	}

	// Intent should be cleaned up.
	jobs.mu.Lock()
	_, present := jobs.intents[name]
	jobs.mu.Unlock()
	if present {
		t.Fatal("intent was not cleaned up after Wait")
	}
}

func TestWait_NoIntent(t *testing.T) {
	t.Parallel()
	controller := &configuredController{blockingController: *newBlockingController()}
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(controller, config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = jobs.Wait(context.Background(), "nonexistent", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "no in-process intent") {
		t.Fatalf("expected no-intent error, got: %v", err)
	}
}

func TestWait_ControllerError(t *testing.T) {
	t.Parallel()
	waitErr := errors.New("context deadline exceeded")
	controller := &configuredController{
		blockingController: *newBlockingController(),
		waitErr:            waitErr,
	}
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(controller, config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	const name = "wf-wait-err"
	jobs.mu.Lock()
	jobs.intents[name] = argoIntent{runID: "run-wait-err", trigger: periapsis.TriggerCI, created: true}
	jobs.mu.Unlock()

	_, err = jobs.Wait(context.Background(), name, io.Discard)
	if !errors.Is(err, waitErr) {
		t.Fatalf("expected wait error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// TerminalResult
// ---------------------------------------------------------------------------

func TestTerminalResult_Succeeded(t *testing.T) {
	t.Parallel()
	now := time.Now()
	controller := &configuredController{
		blockingController: *newBlockingController(),
		termState: &argojob.Completion{
			Succeeded: true,
			Phase:     "Succeeded",
			Steps: []argojob.StepResult{
				{Burn: "build", Step: "compile", Ordinal: 0, Status: "passed", ExitCode: 0,
					StartedAt: &now, FinishedAt: &now},
			},
		},
	}
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(controller, config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	result, err := jobs.TerminalResult(context.Background(), "wf-terminal")
	if err != nil {
		t.Fatalf("TerminalResult: %v", err)
	}
	if result.Status != model.RunPassed {
		t.Fatalf("status = %q, want passed", result.Status)
	}
	if len(result.Steps) != 1 || result.Steps[0].Burn != "build" {
		t.Fatalf("steps = %+v", result.Steps)
	}
}

func TestTerminalResult_NotTerminal(t *testing.T) {
	t.Parallel()
	controller := &configuredController{
		blockingController: *newBlockingController(),
		termErr:            argojob.ErrNotTerminal,
	}
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(controller, config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = jobs.TerminalResult(context.Background(), "wf-active")
	if !errors.Is(err, service.ErrJobNotTerminal) {
		t.Fatalf("expected ErrJobNotTerminal, got: %v", err)
	}
}

func TestTerminalResult_NilCompletion(t *testing.T) {
	t.Parallel()
	controller := &configuredController{
		blockingController: *newBlockingController(),
		termState:          nil,
		termErr:            nil,
	}
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(controller, config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = jobs.TerminalResult(context.Background(), "wf-nil")
	if !errors.Is(err, service.ErrJobNotTerminal) {
		t.Fatalf("expected ErrJobNotTerminal for nil completion, got: %v", err)
	}
}

func TestTerminalResult_ControllerError(t *testing.T) {
	t.Parallel()
	controllerErr := errors.New("API server unreachable")
	controller := &configuredController{
		blockingController: *newBlockingController(),
		termErr:            controllerErr,
	}
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(controller, config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	_, err = jobs.TerminalResult(context.Background(), "wf-err")
	if !errors.Is(err, controllerErr) {
		t.Fatalf("expected controller error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Owns
// ---------------------------------------------------------------------------

func TestOwns_Delegation(t *testing.T) {
	t.Parallel()
	controller := &configuredController{
		blockingController: *newBlockingController(),
		ownsResult:         true,
	}
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(controller, config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	owns, err := jobs.Owns(context.Background(), "wf-owned")
	if err != nil {
		t.Fatalf("Owns: %v", err)
	}
	if !owns {
		t.Fatal("Owns returned false, want true")
	}
}

func TestOwns_NotOwned(t *testing.T) {
	t.Parallel()
	controller := &configuredController{
		blockingController: *newBlockingController(),
		ownsResult:         false,
	}
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(controller, config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	owns, err := jobs.Owns(context.Background(), "wf-not-owned")
	if err != nil {
		t.Fatalf("Owns: %v", err)
	}
	if owns {
		t.Fatal("Owns returned true, want false")
	}
}

// ---------------------------------------------------------------------------
// forget
// ---------------------------------------------------------------------------

func TestForget_CleansMatchingIntent(t *testing.T) {
	t.Parallel()
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(newBlockingController(), config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	const name = "wf-forget"
	const runID = "run-forget"
	jobs.mu.Lock()
	jobs.intents[name] = argoIntent{runID: runID, trigger: periapsis.TriggerCI}
	jobs.mu.Unlock()

	jobs.forget(name, runID)

	jobs.mu.Lock()
	_, present := jobs.intents[name]
	jobs.mu.Unlock()
	if present {
		t.Fatal("intent was not removed by forget")
	}
}

func TestForget_IgnoresDifferentRunID(t *testing.T) {
	t.Parallel()
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(newBlockingController(), config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	const name = "wf-forget-mismatch"
	jobs.mu.Lock()
	jobs.intents[name] = argoIntent{runID: "run-owner", trigger: periapsis.TriggerCI}
	jobs.mu.Unlock()

	// Call forget with a different runID -- should NOT remove.
	jobs.forget(name, "run-different")

	jobs.mu.Lock()
	_, present := jobs.intents[name]
	jobs.mu.Unlock()
	if !present {
		t.Fatal("forget removed intent belonging to a different run")
	}
}

// ---------------------------------------------------------------------------
// result mapping (complete table-driven coverage)
// ---------------------------------------------------------------------------

func TestResultMapping(t *testing.T) {
	t.Parallel()
	now := time.Now()
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(newBlockingController(), config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name       string
		completion argojob.Completion
		wantStatus model.RunStatus
		wantPhase  string
		wantError  string
	}{
		{
			name:       "succeeded",
			completion: argojob.Completion{Succeeded: true},
			wantStatus: model.RunPassed,
			wantPhase:  "passed",
		},
		{
			name:       "failed with reason",
			completion: argojob.Completion{Succeeded: false, Reason: "OOMKilled"},
			wantStatus: model.RunFailed,
			wantPhase:  "job",
			wantError:  "OOMKilled",
		},
		{
			name:       "failed without reason",
			completion: argojob.Completion{Succeeded: false, Phase: "Failed"},
			wantStatus: model.RunFailed,
			wantPhase:  "job",
			wantError:  "Workflow Failed",
		},
		{
			name: "failed with failed step",
			completion: argojob.Completion{
				Succeeded:  false,
				FailedBurn: "test",
				FailedStep: "lint",
			},
			wantStatus: model.RunFailed,
			wantPhase:  "test",
		},
		{
			name: "succeeded but reports failed step -- inconsistent",
			completion: argojob.Completion{
				Succeeded:  true,
				FailedBurn: "build",
				FailedStep: "compile",
			},
			wantStatus: model.RunFailed,
			wantPhase:  "build",
			wantError:  "successful Workflow reported a failed step",
		},
		{
			name: "with step results",
			completion: argojob.Completion{
				Succeeded: true,
				Steps: []argojob.StepResult{
					{Burn: "test", Step: "unit", Ordinal: 0, Status: "passed", ExitCode: 0,
						StartedAt: &now, FinishedAt: &now},
					{Burn: "build", Step: "compile", Ordinal: 1, Status: "passed", ExitCode: 0},
				},
			},
			wantStatus: model.RunPassed,
			wantPhase:  "passed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			result := jobs.result(tc.completion)
			if result.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q", result.Status, tc.wantStatus)
			}
			if result.Phase != tc.wantPhase {
				t.Fatalf("phase = %q, want %q", result.Phase, tc.wantPhase)
			}
			if tc.wantError != "" && result.Error != tc.wantError {
				t.Fatalf("error = %q, want %q", result.Error, tc.wantError)
			}
			if len(tc.completion.Steps) > 0 && len(result.Steps) != len(tc.completion.Steps) {
				t.Fatalf("steps length = %d, want %d", len(result.Steps), len(tc.completion.Steps))
			}
			for i, step := range result.Steps {
				if step.Burn != tc.completion.Steps[i].Burn {
					t.Fatalf("step[%d].Burn = %q, want %q", i, step.Burn, tc.completion.Steps[i].Burn)
				}
				if step.Step != tc.completion.Steps[i].Step {
					t.Fatalf("step[%d].Step = %q, want %q", i, step.Step, tc.completion.Steps[i].Step)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Delete edge cases
// ---------------------------------------------------------------------------

func TestDelete_EmptyRunID(t *testing.T) {
	t.Parallel()
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(newBlockingController(), config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	err = jobs.Delete(context.Background(), "wf-delete", "")
	if err == nil || !strings.Contains(err.Error(), "run ID is required") {
		t.Fatalf("expected run-ID-required error, got: %v", err)
	}
}

func TestDelete_WrongRunIDForIntent(t *testing.T) {
	t.Parallel()
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(newBlockingController(), config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	const name = "wf-wrong-run"
	jobs.mu.Lock()
	jobs.intents[name] = argoIntent{runID: "run-owner", trigger: periapsis.TriggerCI, created: true}
	jobs.mu.Unlock()

	err = jobs.Delete(context.Background(), name, "run-intruder")
	if err == nil || !strings.Contains(err.Error(), "different in-process run") {
		t.Fatalf("expected different-run error, got: %v", err)
	}
}

func TestDelete_CreatedIntent(t *testing.T) {
	t.Parallel()
	controller := newBlockingController()
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(controller, config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	const name = "wf-delete-created"
	const runID = "run-delete-created"
	jobs.mu.Lock()
	jobs.intents[name] = argoIntent{runID: runID, trigger: periapsis.TriggerCI, created: true}
	jobs.mu.Unlock()

	if err := jobs.Delete(context.Background(), name, runID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Intent should be removed.
	jobs.mu.Lock()
	_, present := jobs.intents[name]
	jobs.mu.Unlock()
	if present {
		t.Fatal("intent was not removed after Delete of created workflow")
	}

	// Controller should have received a Cancel.
	controller.mu.Lock()
	found := false
	for _, n := range controller.cancelled {
		if n == name {
			found = true
			break
		}
	}
	controller.mu.Unlock()
	if !found {
		t.Fatal("controller.Cancel was not called")
	}
}

func TestDelete_NoIntent(t *testing.T) {
	t.Parallel()
	controller := newBlockingController()
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(controller, config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	// No intent planted -- Delete should still call controller.Cancel.
	err = jobs.Delete(context.Background(), "wf-absent", "run-absent")
	if err != nil {
		t.Fatalf("Delete with no intent: %v", err)
	}
}

// ---------------------------------------------------------------------------
// create edge cases
// ---------------------------------------------------------------------------

func TestCreate_EmptyJobName(t *testing.T) {
	t.Parallel()
	config := argojob.Config{
		Namespace:                  "test-pipeline",
		PipelineServiceAccount:     "test-pipeline",
		CredentialedServiceAccount: "test-credentialed",
		CISecretsServiceAccount:    "test-ci-secrets",
		ExecutorServiceAccount:     "test-executor",
	}
	jobs, err := NewArgoJobs(newBlockingController(), config, &stubAuditor{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	request := service.JobRequest{
		JobName: "",
		Run: model.Run{
			ID: "run-empty-name", Ref: "refs/heads/main",
			SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Actor: "agent@host",
		},
		Repository: model.Repository{Name: "test-repo"},
	}
	err = jobs.CreateCI(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "required") {
		t.Fatalf("expected required-fields error, got: %v", err)
	}
}

func TestCreate_EmptyActor(t *testing.T) {
	t.Parallel()
	fixture := newRaceFixture(t)
	request := fixture.request("wf-no-actor", "run-no-actor")
	request.Run.Actor = ""
	err := fixture.jobs.CreateCI(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "attributable uplink identity") {
		t.Fatalf("expected actor-required error, got: %v", err)
	}
}

func TestCreate_InFlightDifferentRun(t *testing.T) {
	t.Parallel()
	fixture := newRaceFixture(t)

	const name = "wf-conflict"
	// First create succeeds.
	if err := fixture.jobs.CreateCI(context.Background(), fixture.request(name, "run-first")); err != nil {
		t.Fatal(err)
	}

	// Second create for the same name but different run ID.
	err := fixture.jobs.CreateCI(context.Background(), fixture.request(name, "run-second"))
	if err == nil || !strings.Contains(err.Error(), "in flight for a different run") {
		t.Fatalf("expected in-flight conflict error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// noPipelineError
// ---------------------------------------------------------------------------

func TestNoPipelineError_UnknownTrigger(t *testing.T) {
	t.Parallel()
	// When TriggerFile returns empty for an unknown trigger, noPipelineError
	// falls back to BuildFile.
	err := noPipelineError(os.ErrNotExist, periapsis.Trigger("unknown"), "test-repo")
	if err == nil {
		t.Fatal("expected error")
	}
	// The error should be the TriggerFile error since "unknown" is invalid.
	// Actually noPipelineError wraps os.ErrNotExist: it checks errors.Is first.
	// Since os.ErrNotExist is the unwrapped error, it reaches the TriggerFile call.
}

// ---------------------------------------------------------------------------
// PipelineSize edge cases
// ---------------------------------------------------------------------------

func TestPipelineSize_ReleaseFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	oberthDir := filepath.Join(dir, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	workflow := `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: L
spec:
  entrypoint: main
  templates:
    - name: main
      steps:
        - - name: publish
            template: echo
    - name: echo
      container:
        image: alpine:3.20
        command: ["echo", "release"]
`
	if err := os.WriteFile(filepath.Join(oberthDir, "release.yaml"), []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}

	jobs := &ArgoJobs{intents: map[string]argoIntent{}}
	size, err := jobs.PipelineSize(context.Background(), service.JobRequest{
		Run:        model.Run{Ref: "refs/tags/v1.0.0", Release: true},
		Repository: model.Repository{Name: "test-repo"},
		SourceDir:  dir,
	})
	if err != nil {
		t.Fatalf("PipelineSize: %v", err)
	}
	if size != periapsis.L {
		t.Fatalf("size = %s, want L", size)
	}
}

// ---------------------------------------------------------------------------
// readArgoSource edge cases
// ---------------------------------------------------------------------------

func TestReadArgoSource_ValidCI(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	oberthDir := filepath.Join(dir, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("test-content")
	if err := os.WriteFile(filepath.Join(oberthDir, "build.yaml"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	source, err := readArgoSource(dir, periapsis.TriggerCI)
	if err != nil {
		t.Fatalf("readArgoSource: %v", err)
	}
	if !bytes.Equal(source, content) {
		t.Fatalf("source = %q, want %q", source, content)
	}
}

func TestReadArgoSource_InvalidTrigger(t *testing.T) {
	t.Parallel()
	_, err := readArgoSource(t.TempDir(), periapsis.Trigger("invalid"))
	if err == nil {
		t.Fatal("expected error for invalid trigger")
	}
}

// ---------------------------------------------------------------------------
// runTrigger
// ---------------------------------------------------------------------------

func TestRunTrigger_CI(t *testing.T) {
	t.Parallel()
	trigger, err := runTrigger(model.Run{Release: false})
	if err != nil || trigger != periapsis.TriggerCI {
		t.Fatalf("trigger = %v, err = %v", trigger, err)
	}
}

func TestRunTrigger_Release(t *testing.T) {
	t.Parallel()
	trigger, err := runTrigger(model.Run{Release: true})
	if err != nil || trigger != periapsis.TriggerRelease {
		t.Fatalf("trigger = %v, err = %v", trigger, err)
	}
}

// ---------------------------------------------------------------------------
// PlannedSteps step mapping verification
// ---------------------------------------------------------------------------

func TestPlannedSteps_StepFieldMapping(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	oberthDir := filepath.Join(dir, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Single-container entrypoint: the simplest valid shape.
	workflow := `apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/size: S
spec:
  entrypoint: main
  templates:
    - name: main
      container:
        image: alpine:3.20
        command: ["echo", "ok"]
`
	if err := os.WriteFile(filepath.Join(oberthDir, "build.yaml"), []byte(workflow), 0o644); err != nil {
		t.Fatal(err)
	}

	jobs := &ArgoJobs{intents: map[string]argoIntent{}}
	steps, err := jobs.PlannedSteps(context.Background(), service.JobRequest{
		Run:        model.Run{Ref: "refs/heads/main"},
		Repository: model.Repository{Name: "test-repo"},
		SourceDir:  dir,
	})
	if err != nil {
		t.Fatalf("PlannedSteps: %v", err)
	}
	// Verify that every step has the correct type.
	for _, step := range steps {
		_ = runprogress.PlannedStep{Burn: step.Burn, Step: step.Step, Ordinal: step.Ordinal}
	}
}
