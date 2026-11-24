package client

import (
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestFooterShowsSelectedWorkflow(t *testing.T) {
	now := time.Date(2026, 11, 18, 0, 1, 0, 0, time.UTC)
	run := apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: strings.Repeat("a", 32), Repo: "origin/team/widget", Ref: "refs/heads/topic", Status: stepmodel.RunPassed, CreatedAt: now, FinishedAt: &now}, SelectedFile: "hack/walk-vm.yaml"}
	tree := string(footerRows(apicontract.RunPage{Runs: []apicontract.RunRecord{run}}, nil, now))
	if !strings.Contains(tree, run.SelectedFile) || !strings.Contains(tree, "passed") || !strings.Contains(tree, "green") {
		t.Fatal("footer lost the selected workflow or changed its result", tree)
	}
}
