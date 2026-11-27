package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type cliLogUsageCase struct {
	name                         string
	capped, failed, carried, gap bool
}

func TestCappedCaptureIsVisibleInCLI(t *testing.T) {
	for _, tc := range []cliLogUsageCase{
		{name: "capped", capped: true},
		{name: "failed", capped: true, failed: true},
		{name: "carried", capped: true, carried: true},
		{name: "window only"},
		{name: "incomplete", gap: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, run := captureCLILog(t, tc)
			physicalID := run.ID
			path := run.Admission.Plan.Entries[0].Path
			step := stepmodel.RunStep{Path: path, Name: "build", Status: stepmodel.StepPassed}
			if tc.failed {
				step.Status, step.DisplayReason = stepmodel.StepFailed, "exited with code 7"
			}
			saveReaderStep(t, reader, run, step)
			if tc.carried {
				run.ID, run.Origin.ResumedFrom = "resumed", physicalID
				plan, err := json.Marshal(nativebinding.ResumePlan{Carried: []string{path}})
				if err != nil {
					t.Fatal(err)
				}
				run.Origin.ResumePlan = plan
			}
			request := apicontract.RunLogsArgs{ID: run.ID, Step: path}
			if !tc.capped {
				request.Limit = new(1)
			}
			pages := readCLILogPages(t, reader, run, request)
			result := pages[0]
			checkCLICapture(t, result, tc)
			if result.Usage.PhysicalRunID != physicalID {
				t.Fatal("physical source relabelled", result.Usage)
			}
			checkCLILogUsage(t, run.ID, step, pages)
		})
	}
}

func readCLILogPages(t *testing.T, reader serverLogs, run stateport.Run, request apicontract.RunLogsArgs) []apicontract.LogResult {
	t.Helper()
	var pages []apicontract.LogResult
	for {
		result, err := reader.Read(t.Context(), run, request)
		if err != nil || len(pages) > 3 {
			t.Fatal("capture pages", result, err)
		}
		pages = append(pages, result)
		if !result.Meta.HasMore {
			return pages
		}
		request.ByteOffset = new(result.Meta.NextByteOffset)
	}
}

func captureCLILog(t *testing.T, tc cliLogUsageCase) (serverLogs, stateport.Run) {
	t.Helper()
	reader, run := realServerLogReader(t)
	body := "a\nb\nc\n"
	if !tc.capped && !tc.gap {
		logs, err := runlog.Open(t.Context(), t.TempDir(), 128<<10)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = logs.Close() })
		reader.owner.c.logs = logs
		// The retained long line exceeds the window's record bound, not the capture cap.
		body = strings.Repeat("x", (64<<10)+1) + "\nkept\n"
	}
	if tc.capped {
		body = strings.Repeat("x", 4097)
	}
	entry := runlog.ContainerLog{NodeID: "node", Step: run.Admission.Plan.Entries[0].Path, PodUID: "pod", PodName: "pod", Namespace: "runs", Container: "main", Main: true}
	reservation, err := reader.owner.c.logs.Reserve(t.Context(), run.ID, entry)
	if err != nil {
		t.Fatal(err)
	}
	gap := runlog.Gap{}
	if tc.gap {
		gap.Reason = runlog.PodMissing
	}
	if err := reservation.Capture(t.Context(), runlog.CaptureRequest{Source: io.NopCloser(strings.NewReader(body)), Gap: gap}); err != nil {
		t.Fatal(err)
	}
	return reader, run
}

func checkCLICapture(t *testing.T, result apicontract.LogResult, tc cliLogUsageCase) {
	t.Helper()
	if result.Usage == nil || result.Usage.CaptureLimited != tc.capped || result.Usage.Incomplete != (tc.capped || tc.gap) {
		t.Fatal("capture facts", result)
	}
	if tc.capped {
		if len(result.Content) != 4096 || result.Meta.Reason != result.Usage.CaptureReason {
			t.Fatal("capture cap fixture", result)
		}
		return
	}
	checkCLIWindow(t, result, tc.gap)
}

func checkCLIWindow(t *testing.T, result apicontract.LogResult, gap bool) {
	t.Helper()
	want := "kept\n"
	if gap {
		want = "a\n"
	}
	if !result.Meta.Truncated || result.Content != want {
		t.Fatal("window fixture", result)
	}
	if !gap && result.Meta.HasMore {
		t.Fatal("final window must be truncated without capture loss", result)
	}
}

func checkCLILogUsage(t *testing.T, id string, step stepmodel.RunStep, pages []apicontract.LogResult) {
	t.Helper()
	b := fake.Bindings()
	status := stepmodel.RunPassed
	if step.Status == stepmodel.StepFailed {
		status = stepmodel.RunFailed
	}
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: id, Status: status, Steps: []stepmodel.RunStep{step}}}}
	logs := cliLogPages(t, pages)
	b.RunLogs.Handler = logs
	app, out, diagnostic := fixture(t, b)
	if code := app.run(t.Context(), []string{"log", id, "build"}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	if _, _, calls := logs.Observed(); calls != len(pages) {
		t.Fatal("CLI did not drain log windows", calls, len(pages))
	}
	result := pages[0]
	u := result.Usage
	if strings.Contains(out.String(), "run log byte limit reached") != u.CaptureLimited || strings.Contains(out.String(), "capture incomplete") != u.Incomplete {
		t.Fatal("capture loss hidden or confused with window truncation", out.String())
	}
	for _, text := range []string{result.Content, "Logs: physical run " + u.PhysicalRunID, fmt.Sprintf("Retained: %d body bytes, %d index bytes", u.Body.Logical, u.Index.Logical), fmt.Sprintf("Allocated: %d bytes", u.Body.Allocated+u.Index.Allocated), u.ProtectionReason, step.DisplayReason} {
		if !strings.Contains(out.String(), text) {
			t.Fatalf("missing %q in %q", text, out.String())
		}
	}
}

func cliLogPages(t *testing.T, pages []apicontract.LogResult) *fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult] {
	t.Helper()
	logs := &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{}
	logs.Run = func(context.Context) (apicontract.LogResult, error) {
		inputs, _, calls := logs.Observed()
		if calls > len(pages) {
			t.Fatal("CLI read past final page")
		}
		if calls > 1 && (inputs[calls-1].ByteOffset == nil || *inputs[calls-1].ByteOffset != pages[calls-2].Meta.NextByteOffset) {
			t.Fatal("CLI lost window cursor", inputs)
		}
		return pages[calls-1], nil
	}
	return logs
}
