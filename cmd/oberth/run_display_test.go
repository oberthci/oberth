package main

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestCLIDisplayRunStatusCombinesCancelledCauses(t *testing.T) {
	for _, tc := range []struct {
		run  remoteRun
		want string
	}{
		{remoteRun{Status: "waiting"}, ""},
		{remoteRun{Status: "running", DisplayStatus: "running"}, "running"},
		{remoteRun{Status: "running", DisplayStatus: "running", StartedAt: ptrTime(time.Now())}, "running"},
		{remoteRun{Status: "interrupted", DisplayStatus: "cancelled", Phase: model.PhaseCancelled}, "cancelled"},
		{remoteRun{Status: "interrupted", DisplayStatus: "cancelled", Reason: "oberth restarted"}, "cancelled"},
		{remoteRun{Status: "waiting", DisplayStatus: "queued"}, "queued"},
	} {
		if got := displayRunStatus(tc.run); got != tc.want {
			t.Fatalf("display %#v = %q, want %q", tc.run, got, tc.want)
		}
	}
	var output strings.Builder
	if err := renderActiveRuns(&output, []remoteActiveRun{{remoteRun: remoteRun{ID: "run-id", Ref: "main", Status: "waiting", DisplayStatus: "queued", DisplayReason: "waiting for the Argo controller"},
		Scheduling: &model.RunScheduling{Message: "Waiting for the Argo controller."}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "queued") || strings.Contains(output.String(), "waiting  ") || !strings.Contains(output.String(), "waiting for the Argo controller") {
		t.Fatalf("active output = %q", output.String())
	}
	// No SIZE column: runs carry no admission size since v0.18.0, so it
	// could only ever print "-".
	header := strings.Fields(strings.SplitN(output.String(), "\n", 2)[0])
	if strings.Join(header, " ") != "RUN STATUS ELAPSED REF REASON" {
		t.Fatalf("active header = %q", header)
	}
}

func ptrTime(value time.Time) *time.Time { return &value }
