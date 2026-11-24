package main

import (
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestServerLogReadDoesNotFollowAnAbsentSource(t *testing.T) {
	reader, run := realServerLogReader(t)
	path := run.Admission.Plan.Entries[0].Path
	saveReaderStep(t, reader, run, stepmodel.RunStep{Path: path, Name: "build", Status: stepmodel.StepRunning})
	entry := runlog.ContainerLog{NodeID: "running", Step: path, PodUID: "pod-uid", PodName: "pod", Namespace: "ci", Container: "main", Main: true, State: runlog.Following}
	if err := reader.owner.c.logs.Track(t.Context(), run.ID, []runlog.ContainerLog{entry}); err != nil {
		t.Fatal(err)
	}
	reader.owner.c.logs.KeepFollowing(run.ID, nil)
	for _, status := range []stepmodel.RunStatus{stepmodel.RunRunning, stepmodel.RunPassed, stepmodel.RunCancelled} {
		run.Status = status
		result, err := reader.Read(t.Context(), run, apicontract.RunLogsArgs{ID: run.ID, Step: path})
		if err != nil || result.Meta.Follow || result.Meta.Copying || result.Meta.Reason != "container capture incomplete" || len(result.Containers) != 0 {
			t.Fatal("absent source promises more live output", status, result, err)
		}
	}
}
