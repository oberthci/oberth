package runlog

import (
	"errors"
	"os"
	"testing"

	"github.com/oberthci/oberth/internal/runprogress"
)

func planStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open run log store: %v", err)
	}
	return store
}

// TestStepPlanRoundTripsInOrdinalOrder is the durability the whole feature
// rests on: the plan is written once, before the first Pod exists, and read
// back by every later request for the run's step list.
func TestStepPlanRoundTripsInOrdinalOrder(t *testing.T) {
	store := planStore(t)
	written := []runprogress.PlannedStep{
		{Burn: "setup", Step: "tools-dir", Ordinal: 0},
		{Burn: "setup", Step: "link-go", Ordinal: 1},
		{Burn: "test", Step: "test", Ordinal: 2},
	}
	if err := store.WriteStepPlan("run-plan", written); err != nil {
		t.Fatalf("write step plan: %v", err)
	}
	read, err := store.StepPlan("run-plan")
	if err != nil {
		t.Fatalf("read step plan: %v", err)
	}
	if len(read) != len(written) {
		t.Fatalf("read %d planned steps, wrote %d", len(read), len(written))
	}
	for index, step := range read {
		if step != written[index] {
			t.Fatalf("planned step %d = %+v, want %+v", index, step, written[index])
		}
	}
}

// TestStepPlanIsAbsentRatherThanEmpty distinguishes "this run predates plans"
// from "this run planned nothing", so a reader can degrade instead of failing.
func TestStepPlanIsAbsentRatherThanEmpty(t *testing.T) {
	store := planStore(t)
	_, err := store.StepPlan("run-without-plan")
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing plan error = %v, want one wrapping os.ErrNotExist", err)
	}
}

// TestWriteStepPlanRefusesAnAmbiguousPlan keeps a malformed plan out of the
// record entirely. A duplicated key or a shared ordinal would make the run's
// step list ambiguous exactly where it claims to be authoritative.
func TestWriteStepPlanRefusesAnAmbiguousPlan(t *testing.T) {
	store := planStore(t)
	for name, plan := range map[string][]runprogress.PlannedStep{
		"duplicate key": {
			{Burn: "setup", Step: "tools-dir", Ordinal: 0},
			{Burn: "setup", Step: "tools-dir", Ordinal: 1},
		},
		"shared ordinal": {
			{Burn: "setup", Step: "tools-dir", Ordinal: 0},
			{Burn: "setup", Step: "link-go", Ordinal: 0},
		},
		"empty step": {{Burn: "setup", Step: "", Ordinal: 0}},
		"control character": {
			{Burn: "setup", Step: "a\nb", Ordinal: 0},
		},
		"negative ordinal": {{Burn: "setup", Step: "tools-dir", Ordinal: -1}},
	} {
		if err := store.WriteStepPlan("run-"+name, plan); err == nil {
			t.Errorf("%s: plan was accepted", name)
		}
	}
}

func TestStepPlanRejectsAnUnsafeRunID(t *testing.T) {
	store := planStore(t)
	if err := store.WriteStepPlan("../escape", []runprogress.PlannedStep{{Burn: "a", Step: "b"}}); err == nil {
		t.Fatal("a traversing run ID was accepted")
	}
	if _, err := store.StepPlan("../escape"); err == nil {
		t.Fatal("a traversing run ID was accepted for read")
	}
}

func TestReplayPlanVersionDistinguishesLegacyClassification(t *testing.T) {
	logs := planStore(t)
	path, _ := logs.planPath("legacy")
	if err := os.WriteFile(path, []byte(`{"version":1,"steps":[{"burn":"publish","step":"push","ordinal":0}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := logs.StepPlan("legacy"); err != nil {
		t.Fatalf("legacy display plan unreadable: %v", err)
	}
	record, err := logs.StepPlanRecord("legacy")
	if err != nil || record.Version == runprogress.PlanVersion {
		t.Fatalf("legacy plan presented as current credential evidence: %+v %v", record, err)
	}
	if err := logs.WriteStepPlan("current", []runprogress.PlannedStep{{Burn: "publish", Step: "push", Ordinal: 0, Credentialed: true}}); err != nil {
		t.Fatal(err)
	}
	record, err = logs.StepPlanRecord("current")
	if err != nil || record.Version != 2 || !record.Steps[0].Credentialed {
		t.Fatalf("new plan lost classification provenance: %+v %v", record, err)
	}
}

func TestReplayPlanV2MissingCredentialMarkerIsUnknown(t *testing.T) {
	logs := planStore(t)
	path, _ := logs.planPath("missing-marker")
	if err := os.WriteFile(path, []byte(`{"version":2,"steps":[{"burn":"publish","step":"push","ordinal":0}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := logs.StepPlanRecord("missing-marker"); err == nil {
		t.Fatal("v2 without per-step credential marker accepted")
	}
}
