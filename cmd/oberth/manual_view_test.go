package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestRunGetAndListNameSelectedWorkflow(t *testing.T) {
	for _, args := range [][]string{{"run", "get", "deadbeef"}, {"run", "list"}, {"run", "get", "deadbeef", "-o", "json"}, {"run", "list", "-o", "json"}} {
		b := fake.Bindings()
		run := apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "deadbeef", Repo: "origin/team/api", Ref: "refs/heads/topic", Status: stepmodel.RunPassed}, SelectedFile: "hack/walk-vm.yaml"}
		b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: run}
		b.RunList.Handler = &fake.Handler[apicontract.RunListArgs, apicontract.RunPage]{Value: apicontract.RunPage{Runs: []apicontract.RunRecord{run}}}
		app, out, diagnostic := fixture(t, b)
		if code := app.run(t.Context(), args); code != 0 || !strings.Contains(out.String(), run.SelectedFile) {
			t.Fatal("run output lost the selected workflow", args, code, out.String(), diagnostic.String())
		}
	}
}
