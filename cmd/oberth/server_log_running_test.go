package main

import (
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestServerLogReadOffersTheLiveAuthoredContainer(t *testing.T) {
	reader, run := realServerLogReader(t)
	entry := runlog.ContainerLog{NodeID: "running", Step: run.Admission.Plan.Entries[0].Path, PodUID: "pod-uid", PodName: "pod", Namespace: "ci", Container: "main", Main: true, State: runlog.Following}
	saveReaderStep(t, reader, run, stepmodel.RunStep{Path: entry.Step, Name: "build", Status: stepmodel.StepRunning, AttemptCount: 1, Attempts: []stepmodel.StepAttempt{{NodeID: entry.NodeID, Status: stepmodel.StepRunning}}})
	if err := reader.owner.c.logs.Track(t.Context(), run.ID, []runlog.ContainerLog{entry}); err != nil {
		t.Fatal(err)
	}
	out, err := reader.Read(t.Context(), run, apicontract.RunLogsArgs{ID: run.ID, Step: "build"})
	if err != nil || !out.Meta.Follow || len(out.Containers) != 1 || out.Containers[0].NodeID != entry.NodeID || out.Containers[0].Container != "main" {
		t.Fatal("running output has no live target", out, err)
	}
}
