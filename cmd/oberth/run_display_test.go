package main

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func TestCLIDisplayRunStatusKeepsTerminalCausesDistinct(t *testing.T) {
	for _, tc := range []struct {
		run  remoteRun
		want string
	}{
		{remoteRun{Status: "waiting"}, "queued"},
		{remoteRun{Status: "running"}, "queued"},
		{remoteRun{Status: "running", StartedAt: ptrTime(time.Now())}, "running"},
		{remoteRun{Status: "interrupted", SupersededBy: "newer"}, "superseded"},
		{remoteRun{Status: "interrupted", Phase: model.PhaseCancelled}, "cancelled"},
		{remoteRun{Status: "interrupted", Reason: "oberth restarted"}, "interrupted"},
		{remoteRun{Status: "waiting", DisplayStatus: "queued"}, "queued"},
	} {
		if got := displayRunStatus(tc.run); got != tc.want {
			t.Fatalf("display %#v = %q, want %q", tc.run, got, tc.want)
		}
	}
	var output strings.Builder
	if err := renderActiveRuns(&output, []remoteActiveRun{{remoteRun: remoteRun{ID: "run-id", Ref: "main", Status: "waiting"},
		Scheduling: &model.RunScheduling{Message: "Waiting for capacity."}}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "queued") || strings.Contains(output.String(), "waiting  ") || !strings.Contains(output.String(), "Waiting for capacity.") {
		t.Fatalf("active output = %q", output.String())
	}
}

func ptrTime(value time.Time) *time.Time { return &value }
