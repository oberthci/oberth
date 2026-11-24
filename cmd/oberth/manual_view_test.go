package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestRunListKeepsSelectionInExistingReasonColumn(t *testing.T) {
	for _, file := range []string{"", "hack/walk-vm.yaml"} {
		for _, reason := range []string{"", "step failed"} {
			rows, err := runTableRows([]any{map[string]any{"id": "deadbeef", "status": "passed", "ref": "refs/heads/topic", "selected_file": file, "display_reason": reason}})
			if err != nil || len(rows) != 2 || !slices.Equal(rows[0], []string{"ID", "STATUS", "REF", "REASON"}) || len(rows[1]) != 4 {
				t.Fatal("workflow selection changed the list columns", rows, err)
			}
			want := reason
			if file != "" {
				want = file + " selected"
				if reason != "" {
					want += " · " + reason
				}
			}
			if rows[1][3] != want {
				t.Fatal("workflow selection lost the existing reason", rows[1])
			}
		}
	}
}

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
