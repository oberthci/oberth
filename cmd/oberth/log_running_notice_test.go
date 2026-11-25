package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestLogExplainsRunningStepBeforeWaitingForOutput(t *testing.T) {
	for _, output := range []string{"", "first output\n"} {
		t.Run(map[bool]string{true: "quiet", false: "output"}[output == ""], func(t *testing.T) {
			quietRunningLogCase(t, output)
		})
	}
}

func quietRunningLogCase(t *testing.T, output string) {
	t.Helper()
	b := fake.Bindings()
	waits := 0
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Run: func(context.Context) (apicontract.RunRecord, error) {
		run := apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "run", Status: stepmodel.RunRunning, Steps: []stepmodel.RunStep{{Path: "build", Name: "build", Status: stepmodel.StepRunning}}}}
		if waits == 2 {
			run.Status, run.Steps[0].Status = stepmodel.RunPassed, stepmodel.StepPassed
		}
		return run, nil
	}}
	b.RunLogs.Handler = &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{Run: func(context.Context) (apicontract.LogResult, error) {
		if waits == 2 {
			return apicontract.LogResult{Content: "finished output\n"}, nil
		}
		return apicontract.LogResult{Content: output}, nil
	}}
	app, out, diagnostic := fixture(t, b)
	const notice = "Step is running; output follows here."
	app.config.Wait = func(context.Context, time.Duration) error {
		if output == "" && !strings.Contains(out.String(), notice) {
			t.Fatal("quiet running step waited without explaining its output", out.String())
		}
		waits++
		return nil
	}
	code := app.run(t.Context(), []string{"log", "run", "build"})
	want := 0
	if output == "" {
		want = 1
	}
	if code != 0 || waits != 2 || strings.Count(out.String(), notice) != want || !strings.Contains(out.String(), "finished output") {
		t.Fatal("running log notice repeated or hid output", code, waits, out.String(), diagnostic.String())
	}
}
