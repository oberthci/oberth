package main

import (
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

func realServerLogReader(t *testing.T) (serverLogs, stateport.Run) {
	t.Helper()
	document := strings.Replace(starterWorkflow(), "entrypoint: build", "entrypoint: pipeline", 1)
	document = strings.Replace(document, "  templates:\n", "  templates:\n    - name: pipeline\n      dag:\n        tasks: [{name: build, template: build}]\n", 1)
	return realServerLogReaderFor(t, document)
}

func realServerLogReaderFor(t *testing.T, document string, ids ...string) (serverLogs, stateport.Run) {
	t.Helper()
	db, err := store.Create(t.Context(), filepath.Join(t.TempDir(), "state.db"), store.DefaultOptions(fake.NewClock().Now))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := storetest.Repository(t, db)
	sha := strings.Repeat("a", 40)
	id := "run-logs"
	if len(ids) > 0 {
		id = ids[0]
	}
	request := admissioncontract.PrepareRequest{Mode: admissioncontract.ModeInspect, Repository: repo.Canonical, RunID: id, Ref: "refs/heads/main", ObjectSHA: sha, CommitSHA: sha, FileBlob: sha, Trigger: model.TriggerBranch, SelectedFile: ".oberth/build.yaml", Document: []byte(document)}
	prepared, err := argoworkflow.Prepare(t.Context(), request, argoworkflow.Policy{}, admissioncontract.PrepareHooks{OfflineValidation: true})
	if err != nil {
		t.Fatal(err)
	}
	author := actor.Actor{Identity: "operator", AuthMethod: "cert"}
	ci := model.CIRequest{ReceiveID: "log-reader", Repo: repo.Canonical, Ref: request.Ref, ObjectSHA: sha, CommitSHA: &sha, Trigger: model.TriggerBranch, Actor: author}
	accepted, err := db.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: ci.ReceiveID, Repository: ci.Repo, Actor: author, Runs: []stateport.Acceptance{{Request: ci, Prepared: prepared}}})
	if err != nil || len(accepted.Runs) != 1 {
		t.Fatal("run acceptance", accepted, err)
	}
	logs, err := runlog.Open(t.Context(), t.TempDir(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.Close() })
	return serverLogs{owner: &serverOwners{c: &composition{store: db, logs: logs}}}, accepted.Runs[0]
}

func saveReaderStep(t *testing.T, reader serverLogs, run stateport.Run, step stepmodel.RunStep) {
	t.Helper()
	if err := reader.owner.c.store.SaveProgress(t.Context(), run.ID, stateport.Progress{Steps: []stepmodel.RunStep{step}}); err != nil {
		t.Fatal(err)
	}
}

func captureReaderContainers(t *testing.T, reader serverLogs, run stateport.Run) {
	t.Helper()
	for _, container := range []string{"main", "init-artifact-oberth", "wait"} {
		entry := runlog.ContainerLog{NodeID: "node", Step: run.Admission.Plan.Entries[0].Path, PodUID: "pod-uid", PodName: "pod", Namespace: "runs", Container: container, Main: container == "main"}
		reservation, err := reader.owner.c.logs.Reserve(t.Context(), run.ID, entry)
		if err != nil {
			t.Fatal(err)
		}
		if err = reservation.Capture(t.Context(), runlog.CaptureRequest{Source: io.NopCloser(strings.NewReader(container + " output\n"))}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestServerLogReaderHidesExecutorOutputByDefault(t *testing.T) {
	reader, run := realServerLogReader(t)
	step := stepmodel.RunStep{Path: run.Admission.Plan.Entries[0].Path, Name: "build", Status: stepmodel.StepPassed}
	saveReaderStep(t, reader, run, step)
	captureReaderContainers(t, reader, run)
	result, err := reader.Read(t.Context(), run, apicontract.RunLogsArgs{ID: run.ID})
	if err != nil || result.Content != "main output\n" || len(result.Containers) != 0 {
		t.Fatal("executor output leaked", result, err)
	}
}

// A single-template pipeline's step has an empty plan path; callers name it
// as run get lists it, by its authored name.
func TestServerLogReaderSelectsAStepByItsAuthoredName(t *testing.T) {
	reader, run := realServerLogReaderFor(t, starterWorkflow())
	entry := run.Admission.Plan.Entries[0]
	if entry.Path != "" || entry.Name != "build" {
		t.Fatal("fixture lost its single authored step", entry)
	}
	run.Status = stepmodel.RunPassed
	saveReaderStep(t, reader, run, stepmodel.RunStep{Path: entry.Path, Name: entry.Name, Status: stepmodel.StepPassed})
	captureReaderContainers(t, reader, run)
	result, err := reader.Read(t.Context(), run, apicontract.RunLogsArgs{ID: run.ID, Step: "build"})
	if err != nil || result.Content != "main output\n" {
		t.Fatal("authored step name did not select its log", result, err)
	}
	if _, err = reader.Read(t.Context(), run, apicontract.RunLogsArgs{ID: run.ID, Step: "absent"}); err == nil {
		t.Fatal("unknown step name selected a log")
	}
}

func TestServerLogReaderSelectsAndNamesTheFailedContainer(t *testing.T) {
	reader, run := realServerLogReader(t)
	run.Status = stepmodel.RunFailed
	step := stepmodel.RunStep{Path: run.Admission.Plan.Entries[0].Path, Name: "build", Status: stepmodel.StepFailed, AttemptCount: 1, Attempts: []stepmodel.StepAttempt{{NodeID: "node", PodName: "pod", Status: stepmodel.StepFailed}}, Logs: []stepmodel.LogSelector{{NodeID: "node", PodName: "pod", Container: "init-artifact-oberth"}}}
	saveReaderStep(t, reader, run, step)
	captureReaderContainers(t, reader, run)
	result, err := reader.Read(t.Context(), run, apicontract.RunLogsArgs{ID: run.ID})
	if err != nil || result.Content != "init-artifact-oberth output\n" {
		t.Fatal("failed container output lost", result, err)
	}
	if len(result.Containers) != 1 || result.Containers[0].Container != "init-artifact-oberth" {
		t.Fatal("failed container not named", result.Containers)
	}
}

func TestServerLogReaderReplacesEndedWaitingState(t *testing.T) {
	reader, run := realServerLogReader(t)
	run.Status = stepmodel.RunFailed
	step := stepmodel.RunStep{Path: run.Admission.Plan.Entries[0].Path, Name: "build", Status: stepmodel.StepFailed, DisplayReason: "failed before starting", Detail: "native source refusal\nExit code: 64"}
	saveReaderStep(t, reader, run, step)
	result, err := reader.Read(t.Context(), run, apicontract.RunLogsArgs{ID: run.ID})
	if err != nil || result.Meta.Follow || result.Meta.Reason != step.DisplayReason || result.Meta.Detail != step.Detail {
		t.Fatal("ended run still waiting", result, err)
	}
}
