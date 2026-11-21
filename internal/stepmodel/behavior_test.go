package stepmodel

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestEnumerationsAreClosedAndZeroIsInvalid(t *testing.T) {
	cases := []struct {
		values []string
		valid  func(string) bool
	}{
		{[]string{"pending", "queued", "running", "passed", "failed", "skipped", "unknown"}, func(s string) bool { return StepStatus(s).Valid() }},
		{[]string{"unknown", "author", "infrastructure", "deadline", "cancelled", "invariant"}, func(s string) bool { return FailureClass(s).Valid() }},
		{[]string{"none", "rerun", "retry_publication", "wait"}, func(s string) bool { return RetryAction(s).Valid() }},
		{[]string{"queued", "running", "passed", "failed", "cancelled"}, func(s string) bool { return RunStatus(s).Valid() }},
		{[]string{"Pending", "Running", "Succeeded", "Failed", "Error", "Unknown"}, func(s string) bool { return NativePhase(s).Valid() }},
	}
	for i, tc := range cases {
		for _, value := range tc.values {
			if !tc.valid(value) {
				t.Errorf("enum %d refuses %s", i, value)
			}
		}
		for _, value := range []string{"", "other", " passed", "PASSED"} {
			if tc.valid(value) {
				t.Errorf("enum %d accepts %q", i, value)
			}
		}
	}
}

func TestCountsCoverProjectionAndIgnoreAttempts(t *testing.T) {
	statuses := []StepStatus{StepPending, StepQueued, StepRunning, StepPassed, StepFailed, StepSkipped, StepUnknown}
	steps := make([]RunStep, len(statuses))
	for i, status := range statuses {
		steps[i] = RunStep{Name: "same", Status: status, AttemptCount: 17}
	}
	for _, exact := range []bool{true, false} {
		got, err := CountSteps(steps, exact)
		want := LogicalStepCounts{Total: 7, Exact: exact, Pending: 1, Queued: 1, Running: 1, Passed: 1, Failed: 1, Skipped: 1, Unknown: 1}
		if err != nil || got != want {
			t.Fatalf("%+v, %v; want %+v", got, err, want)
		}
		got, err = CountSteps(nil, exact)
		if err != nil || got != (LogicalStepCounts{Exact: exact}) {
			t.Fatal(got, err)
		}
	}
}

func TestCountsRefuseInvalidStatusWithoutPartialResult(t *testing.T) {
	for _, status := range []StepStatus{"", "invalid"} {
		got, err := CountSteps([]RunStep{{Status: StepPassed}, {Path: "bad", Status: status}}, true)
		var invalid *InvalidStatusError
		if !errors.As(errors.Join(errors.New("another"), err), &invalid) || invalid.Path != "bad" || got != (LogicalStepCounts{}) {
			t.Fatalf("got %+v, %v", got, err)
		}
	}
}

func TestRunCloneIsIndependentAtEveryReference(t *testing.T) {
	at := time.Unix(10, 0).UTC()
	commit, phase, offset := "commit", NativeRunning, 2
	original := RunSummary{CommitSHA: &commit, NativePhase: &phase, StartedAt: &at, FinishedAt: &at, NextStepOffset: &offset,
		Steps:   []RunStep{{StartedAt: &at, FinishedAt: &at, Attempts: []StepAttempt{{NodeID: "node"}}, Logs: []LogSelector{{Container: "main"}}}},
		Failure: &FailureRecord{Retry: RetryHint{After: &at}}}
	before, _ := json.Marshal(original)
	copy := original.Clone()
	*copy.CommitSHA = "changed"
	*copy.NativePhase = NativeFailed
	*copy.NextStepOffset = 9
	*copy.StartedAt = time.Time{}
	*copy.FinishedAt = time.Time{}
	*copy.Steps[0].StartedAt = time.Time{}
	*copy.Steps[0].FinishedAt = time.Time{}
	copy.Steps[0].Attempts[0].NodeID = "changed"
	copy.Steps[0].Logs[0].Container = "changed"
	*copy.Failure.Retry.After = time.Time{}
	after, _ := json.Marshal(original)
	if string(before) != string(after) {
		t.Fatalf("clone changed original: %s", after)
	}
}

func TestClonePreservesNilAndEmptyCollections(t *testing.T) {
	for _, value := range []RunSummary{{}, {Steps: []RunStep{}}, {Steps: []RunStep{{Attempts: []StepAttempt{}, Logs: []LogSelector{}}}}} {
		if !reflect.DeepEqual(value, value.Clone()) {
			t.Fatalf("clone changes %#v", value)
		}
	}
}

func TestCountsIncludeExpandedProjectionAndTerminalUnknownRemainder(t *testing.T) {
	steps := []RunStep{{Path: "static", Status: StepPending},
		{Path: "dynamic.item0", Status: StepPassed}, {Path: "dynamic.item1", Status: StepFailed},
		{Path: "dynamic.<remaining>", Status: StepUnknown}}
	got, err := CountSteps(steps, false)
	want := LogicalStepCounts{Total: 4, Exact: false, Pending: 1, Passed: 1, Failed: 1, Unknown: 1}
	if err != nil || got != want {
		t.Fatalf("counts = %+v, %v; want %+v", got, err, want)
	}
}
