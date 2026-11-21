package main

import (
	"context"
	"encoding/json"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEndedLogPrintsFailedInitOutputAndNativeMessage(t *testing.T) {
	reader, nativeRun := realServerLogReader(t)
	nativeRun.Status = stepmodel.RunFailed
	step := stepmodel.RunStep{Path: "build", Name: "build", Status: stepmodel.StepFailed, AttemptCount: 1, DisplayReason: "failed before starting", Detail: "source access forbidden by the controller\nExit code: 64", Attempts: []stepmodel.StepAttempt{{NodeID: "node", Status: stepmodel.StepFailed}}, Logs: []stepmodel.LogSelector{{NodeID: "node", Container: "main"}, {NodeID: "node", Container: "init-artifact-oberth"}}}
	run := stepmodel.RunSummary{ID: nativeRun.ID, Status: stepmodel.RunFailed, DisplayReason: step.DisplayReason, Failure: &stepmodel.FailureRecord{Class: stepmodel.FailureInfrastructure, Step: step.Path, Name: step.Name}, Steps: []stepmodel.RunStep{step}}
	b := fake.Bindings()
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: apicontract.RunRecord{RunSummary: run}}
	saveReaderStep(t, reader, nativeRun, step)
	captureReaderContainers(t, reader, nativeRun)
	logs := &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{}
	logs.Run = func(ctx context.Context) (apicontract.LogResult, error) {
		inputs, _, _ := logs.Observed()
		return reader.Read(ctx, nativeRun, inputs[len(inputs)-1])
	}
	b.RunLogs.Handler = logs
	app, out, diagnostic := fixture(t, b)
	app.config.Wait = func(context.Context, time.Duration) error { t.Fatal("ended run waited for a live stream"); return nil }
	if code := app.run(t.Context(), []string{"log", nativeRun.ID, "build"}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	for _, part := range []string{"== build ==\ncontainer: init-artifact-oberth\ninit-artifact-oberth output\n", "failed before starting", "source access forbidden by the controller", "Exit code: 64"} {
		if !strings.Contains(out.String(), part) {
			t.Fatal("missing failure evidence", part, out.String())
		}
	}
	if strings.Contains(out.String(), "waiting to start") || strings.Contains(out.String(), "main output") {
		t.Fatal("wrong container or live hint", out.String())
	}
	out.Reset()
	data, _ := json.Marshal(run)
	if err := app.render(data, false, commonOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Failed step build: failed before starting") || strings.Contains(out.String(), "unknown") {
		t.Fatal("run failure lost its step or cause", out.String())
	}
}

func TestEndedLogReplacesWaitingHintWithoutHidingActiveCopy(t *testing.T) {
	steps := []stepmodel.RunStep{{Path: "build", Status: stepmodel.StepFailed, DisplayReason: "failed before starting", Detail: "native failure"}}
	selector := logcontract.Selector{Step: "build", StepSet: true}
	for _, copying := range []bool{false, true} {
		meta := logcontract.Meta{Reason: "step waiting to start", Follow: true, Copying: copying}
		got := endedLogMeta(stateport.Run{RunSummary: stepmodel.RunSummary{Status: stepmodel.RunFailed}}, steps, selector, meta)
		if copying {
			if !reflect.DeepEqual(got, meta) {
				t.Fatal("active capture was hidden", got)
			}
		} else if got.Reason != steps[0].DisplayReason || got.Detail != steps[0].Detail || got.Follow {
			t.Fatal("ended run suggested a live stream", got)
		}
	}
}
