package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestRunWaitExitReflectsFailedOrCancelledRun(t *testing.T) {
	for _, status := range []stepmodel.RunStatus{stepmodel.RunPassed, stepmodel.RunFailed, stepmodel.RunCancelled, stepmodel.RunQueued, stepmodel.RunRunning} {
		for _, format := range []string{"text", "json"} {
			t.Run(string(status)+"/"+format, func(t *testing.T) {
				checkRunWaitExit(t, status, format)
			})
		}
	}
}

func TestRunWaitHelpDocumentsExitCodes(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	if code := app.run(t.Context(), []string{"run", "wait", "--help"}); code != 0 {
		t.Fatal(code, diagnostic)
	}
	text := strings.Join(strings.Fields(out.String()), " ")
	for _, want := range []string{"0 observed (including timeout)", "1 failed/cancelled run or refusal", "2 usage", "130 interrupted"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing exit code", want, out)
		}
	}
}

func checkRunWaitExit(t *testing.T, status stepmodel.RunStatus, format string) {
	t.Helper()
	b := fake.Bindings()
	result := apicontract.WaitResult{Run: &stepmodel.RunSummary{ID: "deadbeef", Repo: "origin/team/widget", Ref: "refs/heads/main", Status: status}, TimedOut: status == stepmodel.RunRunning || status == stepmodel.RunQueued}
	b.RunWait.Handler = &fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{Value: result}
	app, out, diagnostic := fixture(t, b)
	want := 0
	if status == stepmodel.RunFailed || status == stepmodel.RunCancelled {
		want = 1
	}
	args := []string{"run", "wait", "deadbeef"}
	if format == "json" {
		args = append(args, "--json")
	}
	if code := app.run(t.Context(), args); code != want {
		t.Fatal("wrong wait exit", code, want, out, diagnostic)
	}
	if format == "json" {
		raw, _ := json.Marshal(result)
		if strings.TrimSpace(out.String()) != string(raw) {
			t.Fatal("wait JSON changed", out)
		}
	} else if !strings.Contains(out.String(), string(status)) {
		t.Fatal("wait result missing", out)
	}
}
