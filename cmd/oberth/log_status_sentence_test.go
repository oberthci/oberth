package main

import (
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
	"strings"
	"testing"
)

func TestLogFailedStepHasAStatusSentenceWithoutASpecificReason(t *testing.T) {
	for _, tc := range []struct{ name, reason, detail, want string }{
		{"generic", "", "Pod failed without exit evidence.", "step failed\nPod failed without exit evidence.\n"},
		{"no detail", "", "", "step failed\n"},
		{"specific", "exited with code 7", "Complete native diagnostics.", "exited with code 7\nComplete native diagnostics.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fake.Bindings()
			b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "run", Status: stepmodel.RunFailed, Steps: []stepmodel.RunStep{{Path: "build", Name: "build", Status: stepmodel.StepFailed, DisplayReason: tc.reason, Detail: tc.detail}}}}}
			b.RunLogs.Handler = &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{Value: apicontract.LogResult{Content: "native output"}}
			app, out, diagnostic := fixture(t, b)
			if code := app.run(t.Context(), []string{"log", "run", "build"}); code != 0 {
				t.Fatal(code, diagnostic.String())
			}
			if want := "== build ==\nnative output\n" + tc.want; !strings.HasPrefix(out.String(), want) {
				t.Fatalf("failed step has no complete status sentence: %q, want prefix %q", out.String(), want)
			}
		})
	}
}
