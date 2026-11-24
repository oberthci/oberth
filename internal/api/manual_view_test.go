package api_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestRunViewsExposeSelectedFileAcrossTransports(t *testing.T) {
	run := apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "12345678", Status: stepmodel.RunPassed}, SelectedFile: "hack/walk-vm.yaml"}
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: run}
		b.RunList.Handler = &fake.Handler[apicontract.RunListArgs, apicontract.RunPage]{Value: apicontract.RunPage{Runs: []apicontract.RunRecord{run}}}
	})
	for _, path := range []string{"/api/runs", "/api/runs/12345678"} {
		response := request(f, "GET", path, nil)
		assertSuccess(t, response)
		if !strings.Contains(response.Body.String(), `"selected_file":"hack/walk-vm.yaml"`) {
			t.Fatal("web run response lost workflow selection", response.Body.String())
		}
	}
	for _, tool := range []string{"run_get", "run_list"} {
		args := map[string]any{}
		if tool == "run_get" {
			args["id"] = run.ID
		}
		response := rpc(f, tool, args)
		assertSuccess(t, response)
		if !strings.Contains(response.Body.String(), `"selected_file":"hack/walk-vm.yaml"`) {
			t.Fatal("MCP run response lost workflow selection", response.Body.String())
		}
	}
}
