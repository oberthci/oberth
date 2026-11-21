package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestLogShowsEveryStepInPlanOrderAcrossPages(t *testing.T) {
	b := fake.Bindings()
	get := &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{}
	get.Run = func(context.Context) (apicontract.RunRecord, error) {
		inputs, _, _ := get.Observed()
		page := apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "run", Status: stepmodel.RunFailed}}
		if inputs[len(inputs)-1].StepOffset == 0 {
			page.Steps, page.NextStepOffset = []stepmodel.RunStep{{Path: "z-first", Name: "prepare", Status: stepmodel.StepPassed}}, new(1)
		} else {
			page.Steps = []stepmodel.RunStep{{Path: "a-second", Name: "build", Status: stepmodel.StepFailed}}
		}
		return page, nil
	}
	b.RunGet.Handler = get
	logs := &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{}
	logs.Run = func(context.Context) (apicontract.LogResult, error) {
		inputs, _, _ := logs.Observed()
		switch inputs[len(inputs)-1].Step {
		case "z-first":
			return apicontract.LogResult{Content: "own prepare output"}, nil
		case "a-second":
			return apicontract.LogResult{Content: "source delivery failed\n", Containers: []apicontract.Container{{NodeID: "failed", Container: "init-artifact-oberth"}}}, nil
		}
		t.Fatal("unscoped aggregate log requested")
		return apicontract.LogResult{}, nil
	}
	b.RunLogs.Handler = logs
	app, out, diagnostic := fixture(t, b)
	if code := app.run(t.Context(), []string{"log", "run"}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	want := "== prepare ==\nown prepare output\n== build ==\ncontainer: init-artifact-oberth\nsource delivery failed\n"
	if !strings.HasPrefix(out.String(), want) {
		t.Fatal("plan order, headings or failure source lost", out.String())
	}
	inputs, _, calls := logs.Observed()
	if calls != 2 || inputs[0].Step != "z-first" || inputs[1].Step != "a-second" {
		t.Fatal("not every logical step was read once", inputs)
	}
}

func TestLogChangingFailureContainerRestartsItsOwnedCursor(t *testing.T) {
	b := fake.Bindings()
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "run", Status: stepmodel.RunFailed, Steps: []stepmodel.RunStep{{Path: "build", Name: "build", Status: stepmodel.StepFailed}}}}}
	logs := &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{}
	logs.Run = func(context.Context) (apicontract.LogResult, error) {
		inputs, _, calls := logs.Observed()
		request := inputs[len(inputs)-1]
		if calls == 1 {
			return apicontract.LogResult{Content: "own output\n", Meta: logcontract.Meta{HasMore: true, NextByteOffset: 10}}, nil
		}
		result := apicontract.LogResult{Content: "wait failure\n", Containers: []apicontract.Container{{NodeID: "failed", Container: "wait"}}}
		if calls == 2 {
			if request.ByteOffset == nil || *request.ByteOffset != 10 {
				t.Fatal("original cursor changed")
			}
			result.Content = "stale page must not print\n"
		} else if calls != 3 || request.ByteOffset != nil {
			t.Fatal("new container reused the old cursor", calls, request)
		}
		return result, nil
	}
	b.RunLogs.Handler = logs
	app, out, diagnostic := fixture(t, b)
	if code := app.run(t.Context(), []string{"log", "run"}); code != 0 || strings.Contains(out.String(), "stale page") || !strings.Contains(out.String(), "container: wait\nwait failure") {
		t.Fatal(code, out.String(), diagnostic.String())
	}
}

func TestLogRootUsesItsNativeAttempt(t *testing.T) {
	b := fake.Bindings()
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "run", Status: stepmodel.RunPassed, Steps: []stepmodel.RunStep{{Name: "root", Status: stepmodel.StepPassed, Attempts: []stepmodel.StepAttempt{{NodeID: "root-node", Status: stepmodel.StepPassed}}}}}}}
	logs := &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{Value: apicontract.LogResult{Content: "root output\n"}}
	b.RunLogs.Handler = logs
	app, out, diagnostic := fixture(t, b)
	if code := app.run(t.Context(), []string{"log", "run"}); code != 0 || !strings.HasPrefix(out.String(), "== root ==\nroot output\n") {
		t.Fatal(code, out.String(), diagnostic.String())
	}
	inputs, _, calls := logs.Observed()
	if calls != 1 || inputs[0].NodeID != "root-node" {
		t.Fatal("root selector read other steps", inputs)
	}
}

func TestLogWaitsForAPlannedStepWithoutRepeatingItsHeading(t *testing.T) {
	b := fake.Bindings()
	waits := 0
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Run: func(context.Context) (apicontract.RunRecord, error) {
		run := apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "run", Status: stepmodel.RunRunning, Steps: []stepmodel.RunStep{{Path: "build", Name: "build", Status: stepmodel.StepPending}}}}
		if waits > 0 {
			run.Status, run.Steps[0].Status = stepmodel.RunPassed, stepmodel.StepPassed
		}
		return run, nil
	}}
	b.RunLogs.Handler = &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{Run: func(context.Context) (apicontract.LogResult, error) {
		if waits == 0 {
			return apicontract.LogResult{}, nil
		}
		return apicontract.LogResult{Content: "arrived output\n"}, nil
	}}
	app, out, diagnostic := fixture(t, b)
	app.config.Wait = func(context.Context, time.Duration) error { waits++; return nil }
	if code := app.run(t.Context(), []string{"log", "run"}); code != 0 || waits != 1 || strings.Count(out.String(), "== build ==") != 1 || !strings.Contains(out.String(), "arrived output") {
		t.Fatal(code, waits, out.String(), diagnostic.String())
	}
}

func TestLogNamesUnavailableOutputInsteadOfSilentlyFinishing(t *testing.T) {
	b := fake.Bindings()
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "run", Status: stepmodel.RunPassed, Steps: []stepmodel.RunStep{{Path: "build", Name: "build", Status: stepmodel.StepPassed}}}}}
	b.RunLogs.Handler = &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{Value: apicontract.LogResult{Meta: logcontract.Meta{Reason: "step output is being copied"}}}
	app, out, diagnostic := fixture(t, b)
	if code := app.run(t.Context(), []string{"log", "run"}); code != 0 || !strings.Contains(out.String(), "== build ==\nstep output is being copied\n") {
		t.Fatal(code, out.String(), diagnostic.String())
	}
}

func TestLogSkippedStepDoesNotReadANonexistentContainer(t *testing.T) {
	b := fake.Bindings()
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "run", Status: stepmodel.RunPassed, Steps: []stepmodel.RunStep{{Path: "lint", Name: "lint", Status: stepmodel.StepSkipped}}}}}
	logs := &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{}
	b.RunLogs.Handler = logs
	app, out, diagnostic := fixture(t, b)
	if code := app.run(t.Context(), []string{"log", "run"}); code != 0 || !strings.HasPrefix(out.String(), "== lint ==\nrun:") {
		t.Fatal(code, out.String(), diagnostic.String())
	}
	if _, _, calls := logs.Observed(); calls != 0 {
		t.Fatal("skipped step requested output", calls)
	}
}

func TestLogWaitsForFinishedStepOutputStillBeingCopied(t *testing.T) {
	b := fake.Bindings()
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "run", Status: stepmodel.RunPassed, Steps: []stepmodel.RunStep{{Path: "build", Name: "build", Status: stepmodel.StepPassed}}}}}
	reads := 0
	b.RunLogs.Handler = &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{Run: func(context.Context) (apicontract.LogResult, error) {
		reads++
		if reads == 1 {
			return apicontract.LogResult{Content: "earlier attempt\n", Meta: logcontract.Meta{Copying: true, NextByteOffset: 16}}, nil
		}
		return apicontract.LogResult{Content: "finished attempt\n", Meta: logcontract.Meta{NextByteOffset: 33}}, nil
	}}
	app, out, diagnostic := fixture(t, b)
	waits := 0
	app.config.Wait = func(context.Context, time.Duration) error { waits++; return nil }
	if code := app.run(t.Context(), []string{"log", "run"}); code != 0 || reads != 2 || waits != 1 || !strings.HasPrefix(out.String(), "== build ==\nearlier attempt\nfinished attempt\n") {
		t.Fatal(code, reads, waits, out.String(), diagnostic.String())
	}
}

// `oberth log <run> <step>` takes the step name `run get` lists, also for the
// single container of a run whose step has no plan path.
func TestLogSelectsTheStepNameRunGetLists(t *testing.T) {
	b := fake.Bindings()
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "run", Status: stepmodel.RunFailed, Steps: []stepmodel.RunStep{
		{Path: "", Name: "main", Status: stepmodel.StepFailed, Attempts: []stepmodel.StepAttempt{{NodeID: "run-main"}}},
		{Path: "other", Name: "other", Status: stepmodel.StepPassed},
	}}}}
	logs := &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{}
	logs.Run = func(context.Context) (apicontract.LogResult, error) {
		inputs, _, _ := logs.Observed()
		if r := inputs[len(inputs)-1]; r.Step != "" || r.NodeID != "run-main" {
			t.Error("wrong step read", r)
		}
		return apicontract.LogResult{Content: "failing step output\n"}, nil
	}
	b.RunLogs.Handler = logs
	app, out, diagnostic := fixture(t, b)
	if code := app.run(t.Context(), []string{"log", "run", "main"}); code != 0 || !strings.HasPrefix(out.String(), "== main ==\nfailing step output\n") {
		t.Fatal("listed step name refused", code, out.String(), diagnostic.String())
	}
	if _, _, calls := logs.Observed(); calls != 1 {
		t.Fatal("another step was read", calls)
	}
}
