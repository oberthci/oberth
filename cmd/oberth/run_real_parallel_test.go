package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/pkg/argoworkflow/argoworkflowtest"
)

func TestRealTaskGraphHasBoundedCliGroups(t *testing.T) {
	plan := argoworkflowtest.Plan(t)
	steps := stateport.ProjectSteps(stateport.Run{Admission: admissioncontract.PreparedView{Plan: plan}}, nil)
	raw, err := json.Marshal(apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "run1", Status: stepmodel.RunRunning, Steps: steps}})
	if err != nil {
		t.Fatal(err)
	}
	block, ok := Display("run_get", raw)
	if !ok {
		t.Fatal("real task graph has no display block")
	}
	assertRealGraphMarks(t, block, plan)
}

func assertRealGraphMarks(t *testing.T, block string, plan admissioncontract.Plan) {
	t.Helper()
	if strings.Count(block, "┌ ") != 8 || strings.Count(block, "└ ") != 8 || strings.Count(block, "│ ") != 18 {
		t.Fatal("real DAG groups have no distinct ends", block)
	}
	marks := displayMarks(block)
	for _, entry := range plan.Entries {
		if _, ok := marks[entry.Name]; !ok {
			t.Fatal("real DAG lost a step", entry.Name, block)
		}
	}
	for name, want := range map[string]string{
		"release-index": "┌", "tools-dir": "│", "deprecations": "└",
		"link-go": "┌", "verify-helm-schema": "└",
		"install-systemd-tools": "┌", "security": "└",
		"scan-systemd-tools": "┌", "verify-version-pins": "└",
		"smoke-port-chart-index": "┌", "test": "└",
		"test-systemd-budget": "┌", "fixture-cosign": "└",
		"build-linux-amd64": "┌", "trivy-fixture-build": "└",
		"build-darwin-amd64": "┌", "trivy-db-update": "└",
		"residue-lint": "", "profiles": "", "trivy-fixture-qualify": "", "test-openbao": "",
	} {
		if marks[name] != want {
			t.Fatal("wrong real DAG boundary", name, marks[name], want)
		}
	}
}

func displayMarks(block string) map[string]string {
	marks := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "┌" || fields[0] == "│" || fields[0] == "└" {
			marks[fields[1]] = fields[0]
		} else {
			marks[fields[0]] = ""
		}
	}
	return marks
}
