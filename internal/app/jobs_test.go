package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/job"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runner"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type fakeJobControl struct {
	requests               []job.Request
	secretReads            int
	snapshotJobs           []string
	requested              [][]string
	secretSnapshot         job.SecretSnapshot
	storeReads             int
	storeDeclared          [][]periapsis.SecretStoreDeclaration
	storeTakenNames        [][]string
	storeSnapshot          job.SecretStoreSnapshot
	storeErr               error
	waitSecrets            [][]byte
	waitSecretStorePayload []byte
	completion             job.Completion
	canceledNames          []string
}

func (control *fakeJobControl) SecretStoreSnapshot(_ context.Context, declared []periapsis.SecretStoreDeclaration, kubernetesNames []string) (job.SecretStoreSnapshot, error) {
	control.storeReads++
	control.storeDeclared = append(control.storeDeclared, slices.Clone(declared))
	control.storeTakenNames = append(control.storeTakenNames, slices.Clone(kubernetesNames))
	if control.storeErr != nil {
		return job.SecretStoreSnapshot{}, control.storeErr
	}
	if control.storeSnapshot.Empty() {
		control.storeSnapshot = job.SecretStoreSnapshot{Secrets: []job.SecretStoreData{{
			Name: "r2-token", Path: "ci/data/r2-upload", Keys: []string{"token"},
			Values: map[string][]byte{"token": []byte("store-secret-value")},
		}}}
	}
	return control.storeSnapshot, nil
}

type blockingReleaseControl struct {
	preparationStarted chan struct{}
	allowPreparation   chan struct{}
	created            chan string
	canceled           chan string
}

func (control *blockingReleaseControl) SecretSnapshot(_ context.Context, _ string, _ []string) (job.SecretSnapshot, error) {
	close(control.preparationStarted)
	<-control.allowPreparation
	return job.SecretSnapshot{
		Name:   "oberth-release-snapshot",
		Mounts: job.ReleaseSecretMounts{Secrets: []job.ReleaseSecret{{Name: "gar-sa-key", Keys: []string{"json"}}}},
		Data:   map[string][]byte{"secret-0-key-0": []byte("release-secret")},
	}, nil
}

func (control *blockingReleaseControl) Create(_ context.Context, request job.Request) (string, error) {
	control.created <- request.JobName
	return request.JobName, nil
}

func (*blockingReleaseControl) Wait(context.Context, string, string, io.Writer, [][]byte, []byte) (job.Completion, error) {
	return job.Completion{}, nil
}

func (control *blockingReleaseControl) Cancel(_ context.Context, name, _ string) error {
	control.canceled <- name
	return nil
}

func (control *fakeJobControl) Create(_ context.Context, request job.Request) (string, error) {
	control.requests = append(control.requests, request)
	return request.JobName, nil
}

func (control *fakeJobControl) Wait(_ context.Context, name, _ string, _ io.Writer, secrets [][]byte, storePayload []byte) (job.Completion, error) {
	// Deep-clone: the app zeroes the intent's backing arrays once Wait returns.
	control.waitSecrets = make([][]byte, 0, len(secrets))
	for _, value := range secrets {
		control.waitSecrets = append(control.waitSecrets, slices.Clone(value))
	}
	control.waitSecretStorePayload = slices.Clone(storePayload)
	result := control.completion
	result.JobName = name
	return result, nil
}

func (control *fakeJobControl) Cancel(_ context.Context, name, _ string) error {
	control.canceledNames = append(control.canceledNames, name)
	return nil
}

func (control *fakeJobControl) SecretSnapshot(_ context.Context, jobName string, requested []string) (job.SecretSnapshot, error) {
	control.secretReads++
	control.snapshotJobs = append(control.snapshotJobs, jobName)
	control.requested = append(control.requested, slices.Clone(requested))
	if control.secretSnapshot.Name == "" {
		control.secretSnapshot.Name = "oberth-release-snapshot"
	}
	if control.secretSnapshot.Mounts.Secrets == nil {
		control.secretSnapshot.Mounts.Secrets = []job.ReleaseSecret{{Name: "gar-sa-key", Keys: []string{"json"}}}
	}
	if control.secretSnapshot.Data == nil {
		control.secretSnapshot.Data = map[string][]byte{"secret-0-key-0": []byte("release-secret")}
	}
	return control.secretSnapshot, nil
}

type fakeAuditor struct {
	actions []model.AuditActionSpec
	err     error
}

func (auditor *fakeAuditor) AppendAuditAction(_ context.Context, spec model.AuditActionSpec) (model.AuditAction, error) {
	auditor.actions = append(auditor.actions, spec)
	if auditor.err != nil {
		return model.AuditAction{}, auditor.err
	}
	return model.AuditAction{ID: int64(len(auditor.actions))}, nil
}

const secretStoreDeclarationSnippet = `var ReleaseSecrets = []string{"gar-sa-key"}

var SecretStoreSecrets = map[string]string{
	"r2-token": "ci/data/r2-upload",
}
`

func secretStoreJobServiceRequest(t *testing.T, workspace, runID string) service.JobRequest {
	t.Helper()
	request := jobServiceRequest(t, workspace, runID, true)
	request.Run.Actor = "uplink-fab"
	path := filepath.Join(request.SourceDir, ".oberth", "periapsis.go")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rewritten := strings.Replace(string(source), `var ReleaseSecrets = []string{"gar-sa-key"}`, secretStoreDeclarationSnippet, 1)
	if err := os.WriteFile(path, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	return request
}

func TestJobsFetchesSecretStoreSecretsAtReleaseAdmission(t *testing.T) {
	t.Parallel()
	control := &fakeJobControl{}
	auditor := &fakeAuditor{}
	workspace := t.TempDir()
	jobs, err := NewJobs(control, workspace, auditor)
	if err != nil {
		t.Fatal(err)
	}
	request := secretStoreJobServiceRequest(t, workspace, "run-store-release")
	if err := jobs.CreateRelease(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if control.storeReads != 1 || len(control.storeDeclared) != 1 {
		t.Fatalf("secret store reads = %d", control.storeReads)
	}
	declared := control.storeDeclared[0]
	if len(declared) != 1 || declared[0].Name != "r2-token" || declared[0].Path != "ci/data/r2-upload" {
		t.Fatalf("declared = %+v", declared)
	}
	if !slices.Equal(control.storeTakenNames[0], []string{"gar-sa-key"}) {
		t.Fatalf("kubernetes names = %v", control.storeTakenNames[0])
	}
	created := control.requests[0]
	if len(created.SecretStoreSecrets) != 1 || created.SecretStoreSecrets[0].Name != "r2-token" || created.SecretStoreSecrets[0].Path != "ci/data/r2-upload" {
		t.Fatalf("request secret store sources = %+v", created.SecretStoreSecrets)
	}
	control.completion = passedCompletion(t)
	if _, err := jobs.Wait(context.Background(), request.JobName, io.Discard); err != nil {
		t.Fatal(err)
	}
	masks := make([]string, 0, len(control.waitSecrets))
	for _, value := range control.waitSecrets {
		masks = append(masks, string(value))
	}
	if !slices.Contains(masks, "store-secret-value") || !slices.Contains(masks, "release-secret") {
		t.Fatalf("masks = %q", masks)
	}
	var payload runner.SecretStorePayload
	if err := json.Unmarshal(control.waitSecretStorePayload, &payload); err != nil {
		t.Fatalf("delivery payload %q: %v", control.waitSecretStorePayload, err)
	}
	if len(payload.Secrets) != 1 || payload.Secrets[0].Name != "r2-token" || string(payload.Secrets[0].Keys["token"]) != "store-secret-value" {
		t.Fatalf("payload = %+v", payload.Secrets)
	}
	if len(auditor.actions) != 2 {
		t.Fatalf("audit actions = %+v", auditor.actions)
	}
	intent, outcome := auditor.actions[0], auditor.actions[1]
	if intent.Action != "release.secretstore.fetch.intent" || intent.Actor != "uplink-fab" ||
		intent.ResourceType != "run" || intent.ResourceID != request.Run.ID ||
		!strings.Contains(intent.Details, "ci/data/r2-upload") {
		t.Fatalf("intent action = %+v", intent)
	}
	if outcome.Action != "release.secretstore.fetch.succeeded" || outcome.Actor != "uplink-fab" {
		t.Fatalf("outcome action = %+v", outcome)
	}
	if strings.Contains(intent.Details, "store-secret-value") || strings.Contains(outcome.Details, "store-secret-value") {
		t.Fatal("audit details contain a secret value")
	}
}

func TestJobsFailReleaseBeforeJobCreationWhenStoreSecretUnavailable(t *testing.T) {
	t.Parallel()
	control := &fakeJobControl{storeErr: errors.New(`secret store entry "ci/data/r2-upload" is unavailable: not found`)}
	auditor := &fakeAuditor{}
	workspace := t.TempDir()
	jobs, err := NewJobs(control, workspace, auditor)
	if err != nil {
		t.Fatal(err)
	}
	request := secretStoreJobServiceRequest(t, workspace, "run-store-missing")
	err = jobs.CreateRelease(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), `secret store entry "ci/data/r2-upload" is unavailable`) {
		t.Fatalf("admission error = %v", err)
	}
	if len(control.requests) != 0 {
		t.Fatalf("failed secret store admission still created a Job: %+v", control.requests)
	}
	if len(auditor.actions) != 2 || auditor.actions[1].Action != "release.secretstore.fetch.failed" ||
		!strings.Contains(auditor.actions[1].Details, "not found") {
		t.Fatalf("audit actions = %+v", auditor.actions)
	}
}

func TestJobsBranchTriggerNeverTouchesSecretStore(t *testing.T) {
	t.Parallel()
	control := &fakeJobControl{}
	workspace := t.TempDir()
	jobs, err := NewJobs(control, workspace, &fakeAuditor{})
	if err != nil {
		t.Fatal(err)
	}
	request := jobServiceRequest(t, workspace, "run-ci-store-declared", false)
	if err := os.MkdirAll(filepath.Join(request.SourceDir, ".oberth"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := `//go:build ignore

package main

import "oberth"

` + secretStoreDeclarationSnippet + `
func Pipeline(ctx *oberth.Context) oberth.Pipeline {
	return oberth.New().Retrograde("test", oberth.Step{Name: "test", Command: "true"}).Build()
}
`
	if err := os.WriteFile(filepath.Join(request.SourceDir, ".oberth", "periapsis.go"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := jobs.CreateCI(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if control.storeReads != 0 || control.secretReads != 0 {
		t.Fatalf("branch trigger reached secret sources: secretstore=%d kubernetes=%d", control.storeReads, control.secretReads)
	}
	if len(control.requests[0].SecretStoreSecrets) != 0 || control.requests[0].ReleaseSecrets != nil {
		t.Fatalf("branch request carries secrets: %+v", control.requests[0])
	}
	if len(control.waitSecretStorePayload) != 0 {
		t.Fatalf("branch wait received a secret store payload: %q", control.waitSecretStorePayload)
	}
}

func TestJobsRequireAuditorAndActorForSecretStoreSecrets(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	jobs, err := NewJobs(&fakeJobControl{}, workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := secretStoreJobServiceRequest(t, workspace, "run-store-noauditor")
	if err := jobs.CreateRelease(context.Background(), request); err == nil || !strings.Contains(err.Error(), "audit chain") {
		t.Fatalf("missing auditor error = %v", err)
	}
	jobs, err = NewJobs(&fakeJobControl{}, workspace, &fakeAuditor{})
	if err != nil {
		t.Fatal(err)
	}
	anonymous := secretStoreJobServiceRequest(t, workspace, "run-store-noactor")
	anonymous.Run.Actor = " "
	if err := jobs.CreateRelease(context.Background(), anonymous); err == nil || !strings.Contains(err.Error(), "attributable uplink identity") {
		t.Fatalf("missing actor error = %v", err)
	}
}

func TestJobsHardCodesCIAndReleaseCapabilities(t *testing.T) {
	t.Parallel()
	control := &fakeJobControl{}
	workspace := t.TempDir()
	jobs, err := NewJobs(control, workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	ci := jobServiceRequest(t, workspace, "run-ci", false)
	if err := jobs.CreateCI(context.Background(), ci); err != nil {
		t.Fatal(err)
	}
	if control.requests[0].Release || control.secretReads != 0 {
		t.Fatalf("CI request received release capability: %+v, reads=%d", control.requests[0], control.secretReads)
	}
	release := jobServiceRequest(t, workspace, "run-release", true)
	if err := jobs.CreateRelease(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	if !control.requests[1].Release || control.secretReads != 1 || len(control.snapshotJobs) != 1 || control.snapshotJobs[0] != release.JobName {
		t.Fatalf("release request/capability = %+v, reads=%d", control.requests[1], control.secretReads)
	}
	if len(control.requested) != 1 || !slices.Equal(control.requested[0], []string{"gar-sa-key"}) {
		t.Fatalf("repository credential request = %v", control.requested)
	}
	if control.requests[1].ReleaseSecrets == nil || len(control.requests[1].ReleaseSecrets.Mounts.Secrets) != 1 || control.requests[1].ReleaseSecrets.Mounts.Secrets[0].Name != "gar-sa-key" || len(control.requests[1].ReleaseSecrets.Mounts.Secrets[0].Keys) != 1 {
		t.Fatalf("release Secret snapshot = %#v", control.requests[1].ReleaseSecrets)
	}
	control.completion = passedCompletion(t)
	if _, err := jobs.Wait(context.Background(), release.JobName, io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(control.waitSecrets) != 1 || string(control.waitSecrets[0]) != "release-secret" {
		t.Fatalf("release masks = %q", control.waitSecrets)
	}
}

func TestJobsSerializesReleasePreparationAndCreateBeforeDelete(t *testing.T) {
	control := &blockingReleaseControl{
		preparationStarted: make(chan struct{}),
		allowPreparation:   make(chan struct{}),
		created:            make(chan string, 1),
		canceled:           make(chan string, 1),
	}
	workspace := t.TempDir()
	jobs, err := NewJobs(control, workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := jobServiceRequest(t, workspace, "run-release-create-delete", true)
	created := make(chan error, 1)
	go func() { created <- jobs.CreateRelease(context.Background(), request) }()
	select {
	case <-control.preparationStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("release preparation did not start")
	}
	deleted := make(chan error, 1)
	go func() { deleted <- jobs.Delete(context.Background(), request.JobName, request.Run.ID) }()
	select {
	case name := <-control.canceled:
		t.Fatalf("Delete canceled %q before release preparation and Create completed", name)
	case <-time.After(25 * time.Millisecond):
	}
	close(control.allowPreparation)
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	if name := <-control.created; name != request.JobName {
		t.Fatalf("created Job = %q, want %q", name, request.JobName)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	if name := <-control.canceled; name != request.JobName {
		t.Fatalf("canceled Job = %q, want %q", name, request.JobName)
	}
}

func TestJobsRejectsReleaseWithoutStaticCredentialContractBeforeReadingSecrets(t *testing.T) {
	control := &fakeJobControl{}
	workspace := t.TempDir()
	jobs, err := NewJobs(control, workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := jobServiceRequest(t, workspace, "run-release-no-contract", true)
	path := filepath.Join(request.SourceDir, ".oberth", "periapsis.go")
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	source = []byte(strings.Replace(string(source), `var ReleaseSecrets = []string{"gar-sa-key"}`, "", 1))
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := jobs.CreateRelease(context.Background(), request); err == nil || !strings.Contains(err.Error(), "must declare release credentials") {
		t.Fatalf("missing repository credential declaration error = %v", err)
	}
	if control.secretReads != 0 || len(control.requests) != 0 {
		t.Fatalf("missing declaration reached Secret read or Job create: reads=%d creates=%d", control.secretReads, len(control.requests))
	}
}

func TestJobsRejectsGreenCompletionWithoutBindingSummary(t *testing.T) {
	t.Parallel()
	control := &fakeJobControl{completion: job.Completion{Succeeded: true, ExitCode: 0}}
	workspace := t.TempDir()
	jobs, err := NewJobs(control, workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := jobServiceRequest(t, workspace, "run-no-evidence", false)
	if err := jobs.CreateCI(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := jobs.Wait(context.Background(), request.JobName, io.Discard); err == nil {
		t.Fatal("green Job without a binding summary was accepted")
	}
}

func TestJobsConvertsStrictStepEvidence(t *testing.T) {
	t.Parallel()
	control := &fakeJobControl{completion: failedCompletion(t)}
	workspace := t.TempDir()
	jobs, err := NewJobs(control, workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := jobServiceRequest(t, workspace, "run-failed", false)
	if err := jobs.CreateCI(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	result, err := jobs.Wait(context.Background(), request.JobName, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != model.RunFailed || result.FailedBurn != "test" || result.FailedStep != "go-test" || len(result.Steps) != 1 {
		t.Fatalf("result = %+v", result)
	}
	if result.Steps[0].Status != model.StepTimedOut || result.Steps[0].StartedAt == nil || result.Steps[0].FinishedAt == nil {
		t.Fatalf("step = %+v", result.Steps[0])
	}
}

func TestDecodeModelStepsRejectsMalformedTimestamps(t *testing.T) {
	t.Parallel()
	cases := map[string][2]string{
		"malformed started_at":  {"not-a-time", "2026-08-07T10:00:00Z"},
		"empty started_at":      {"", "2026-08-07T10:00:00Z"},
		"malformed finished_at": {"2026-08-07T10:00:00Z", "07/08/2026"},
	}
	for name, stamps := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal([]map[string]any{{
				"burn": "test", "step": "go-test", "status": "passed", "exit_code": 0,
				"started_at": stamps[0], "finished_at": stamps[1],
			}})
			if err != nil {
				t.Fatal(err)
			}
			steps, err := decodeModelSteps(encoded)
			if err == nil {
				t.Fatalf("malformed timestamp was accepted; corrupt timings must fail the collect phase, not record epoch: %+v", steps)
			}
		})
	}
}

func jobServiceRequest(t *testing.T, workspace, runID string, release bool) service.JobRequest {
	t.Helper()
	sha := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	run := model.Run{ID: runID, Ref: "feature/fab", SHA: sha, TestedSHA: sha, Release: release}
	sourceDir := filepath.Join(workspace, runID, "src")
	if release {
		if err := os.MkdirAll(filepath.Join(sourceDir, ".oberth"), 0o755); err != nil {
			t.Fatal(err)
		}
		const source = `//go:build ignore

package main

import "oberth"

var ReleaseSecrets = []string{"gar-sa-key"}

func Pipeline(ctx *oberth.Context) oberth.Pipeline {
	return oberth.New().Release("release", oberth.Step{Name: "release", Command: "true"}).Build()
}
`
		if err := os.WriteFile(filepath.Join(sourceDir, ".oberth", "periapsis.go"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return service.JobRequest{
		JobName: "oberth-" + runID, Run: run, Repository: model.Repository{Name: "oberth"},
		SourceDir: sourceDir,
	}
}

func passedCompletion(t *testing.T) job.Completion {
	t.Helper()
	now := time.Now().UTC()
	raw, err := json.Marshal([]runner.StepResult{{
		Burn: "test", Step: "go-test", Status: runner.StepPassed, ExitCode: 0,
		StartedAt: now.Format(time.RFC3339Nano), FinishedAt: now.Add(time.Second).Format(time.RFC3339Nano),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return job.Completion{Succeeded: true, ExitCode: 0, Summary: raw}
}

func failedCompletion(t *testing.T) job.Completion {
	t.Helper()
	now := time.Now().UTC()
	raw, err := json.Marshal([]runner.StepResult{{
		Burn: "test", Step: "go-test", Status: runner.StepTimedOut, ExitCode: -1,
		StartedAt: now.Format(time.RFC3339Nano), FinishedAt: now.Add(time.Second).Format(time.RFC3339Nano),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return job.Completion{ExitCode: 1, Reason: "Error", Summary: raw}
}
