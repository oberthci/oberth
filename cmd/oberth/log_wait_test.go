package main

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestLogWaitsForPlanWithZeroConfigCallbacks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bindings := fake.Bindings()
		calls := 0
		bindings.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, stepmodel.RunSummary]{Run: func(context.Context) (stepmodel.RunSummary, error) {
			calls++
			status := stepmodel.RunQueued
			if calls > 1 {
				status = stepmodel.RunPassed
			}
			return stepmodel.RunSummary{ID: "run", Status: status, Steps: []stepmodel.RunStep{}}, nil
		}}
		app, out, diagnostic := fixture(t, bindings)
		app.config.Now, app.config.Wait = nil, nil
		if code := app.run(t.Context(), []string{"log", "run"}); code != 0 {
			t.Fatal(code, diagnostic.String())
		}
		if calls != 2 || !strings.Contains(out.String(), "result: passed") {
			t.Fatal("pending plan was not followed", calls, out.String())
		}
	})
}
