package main

import (
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestServerLogReaderUsesAuthoredRoleForMainAttribution(t *testing.T) {
	for _, authored := range []bool{false, true} {
		t.Run(strconv.FormatBool(authored), func(t *testing.T) {
			reader, run := realServerLogReader(t)
			run.Status = stepmodel.RunFailed
			path := run.Admission.Plan.Entries[0].Path
			step := stepmodel.RunStep{Path: path, Status: stepmodel.StepFailed, AttemptCount: 1,
				Attempts: []stepmodel.StepAttempt{{NodeID: "node", PodName: "pod", Status: stepmodel.StepFailed}},
				Logs:     []stepmodel.LogSelector{{NodeID: "node", PodName: "pod", Container: "main"}},
			}
			saveReaderStep(t, reader, run, step)
			entry := runlog.ContainerLog{NodeID: "node", Step: path, PodUID: "pod", PodName: "pod", Namespace: "runs", Container: "main", Main: authored}
			capture, err := reader.owner.c.logs.Reserve(t.Context(), run.ID, entry)
			if err != nil {
				t.Fatal(err)
			}
			if err = capture.Capture(t.Context(), runlog.CaptureRequest{Source: io.NopCloser(strings.NewReader("failed main\n"))}); err != nil {
				t.Fatal(err)
			}
			result, err := reader.Read(t.Context(), run, apicontract.RunLogsArgs{ID: run.ID})
			if err != nil || result.Content != "failed main\n" {
				t.Fatal("main output lost", result, err)
			}
			if authored {
				if len(result.Containers) != 0 {
					t.Fatal("authored output named as an executor failure", result.Containers)
				}
			} else if len(result.Containers) != 1 || result.Containers[0].Container != "main" {
				t.Fatal("non-authored main attribution lost", result.Containers)
			}
		})
	}
}
