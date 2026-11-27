package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestLogUsageViewsAndCarriedPhysicalSource(t *testing.T) {
	reader, run := realServerLogReader(t)
	path := run.Admission.Plan.Entries[0].Path
	saveReaderStep(t, reader, run, stepmodel.RunStep{Path: path, Name: "build", Status: stepmodel.StepPassed})
	captureReaderContainers(t, reader, run)
	old, err := reader.Read(t.Context(), run, apicontract.RunLogsArgs{ID: run.ID, Step: path})
	checkPhysicalLogUsage(t, old, err, run.ID)
	child := run
	child.ID = "resumed"
	child.Origin.ResumedFrom = run.ID
	child.Origin.ResumePlan, err = json.Marshal(nativebinding.ResumePlan{Carried: []string{path}})
	if err != nil {
		t.Fatal(err)
	}
	carried, err := reader.Read(t.Context(), child, apicontract.RunLogsArgs{ID: child.ID, Step: path})
	if err != nil || carried.ID != child.ID || carried.Usage == nil || *carried.Usage != *old.Usage {
		t.Fatal("physical source was relabelled", carried, err)
	}
	checkCarriedStorage(t, reader, child)
	child.Repo = "foreign/team/hidden"
	if _, err := reader.Read(t.Context(), child, apicontract.RunLogsArgs{Step: path}); err == nil {
		t.Fatal("carried logs crossed repository authority")
	}
}

func checkPhysicalLogUsage(t *testing.T, old apicontract.LogResult, err error, id string) {
	t.Helper()
	if err != nil || old.Usage == nil || old.Usage.Body.Logical == 0 || old.Usage.PhysicalRunID != id || old.Usage.EligibilityKnown {
		t.Fatal(old, err)
	}
}

func checkCarriedStorage(t *testing.T, reader serverLogs, child stateport.Run) {
	t.Helper()
	path := child.Admission.Plan.Entries[0].Path
	before, err := reader.owner.c.logs.Inventory(t.Context(), reader.owner.c.clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reader.Read(t.Context(), child, apicontract.RunLogsArgs{Step: path})
	if err != nil {
		t.Fatal(err)
	}
	after, err := reader.owner.c.logs.Inventory(t.Context(), reader.owner.c.clock.Now)
	if err != nil || before.LogicalBytes != after.LogicalBytes || before.AllocatedBytes != after.AllocatedBytes {
		t.Fatal("carried logs counted twice", before, after, err)
	}
}

func TestLogUsageRunRendering(t *testing.T) {
	var out bytes.Buffer
	run := map[string]any{"id": "run", "status": "passed", "log_usage": map[string]any{"body": map[string]any{"logical": 123}, "physical_run_id": "physical", "capture_limited": true, "capture_reason": "run output limit reached"}}
	if err := renderRun(&out, run, false); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"123", "physical", "capture limited", "run output limit reached"} {
		if !strings.Contains(out.String(), text) {
			t.Fatal(text, out.String())
		}
	}
}
