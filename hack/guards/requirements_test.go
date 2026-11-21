package main

import (
	"encoding/json"
	"testing"
)

func TestRequirementSummaryCountsTestsSeparatelyFromWalkDefinitions(t *testing.T) {
	p := fixture(t)
	write(t, "docs/plans/requirements.json", `{"requirements":[
{"id":"REQ-C1-001","component":"C1","inspection":{"kind":"contract_test","name":"TestContract"}},
{"id":"REQ-C1-002","component":"C1","inspection":{"kind":"walk_step","name":"push"}},
{"id":"REQ-C1-003","component":"C1","inspection":{"kind":"walk_step","name":"release"}}]}`)
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	write(t, "docs/plans/dependencies.json", string(body))
	write(t, "component/a/a_test.go", "package a\nimport \"testing\"\n// Covers: REQ-C1-001\nfunc TestContract(t *testing.T){}\n")
	t.Setenv("OBERTH_GUARDS_WALK_STEPS", "push\nrelease")
	t.Setenv("OBERTH_GUARDS_WALK_PASSED", "push")
	c := inspect("example.invalid/unit", []byte("component/a/a_test.go\x00test/walk/walklib.py"))
	clean(t, c.findings)
	want := "requirements: 3 due; 1 covered by tests; 2 defined in the walk; 1 proven by a candidate walk"
	if len(c.notes) != 1 || c.notes[0] != want {
		t.Fatalf("notes = %v; want %s", c.notes, want)
	}
}
