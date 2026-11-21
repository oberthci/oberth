package app

import (
	"github.com/oberthci/oberth/internal/admissioncontract"
	"slices"
	"strings"
	"testing"
)

func TestReviewAssistRefusalLines(t *testing.T) {
	t.Parallel()
	plan := admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build", HoldEligible: true}, {Path: "deploy", Name: "deploy", HoldRefusal: "reads a secret"}, {Path: "fetch", Name: "fetch", HoldRefusal: "uses artifacts"}}}
	want := []string{"assist not used: deploy: reads a secret", "assist not used: fetch: uses artifacts"}
	if got := assistRefusalLines(assistRefusals(true, plan)); !slices.Equal(got, want) {
		t.Fatal(got)
	}
	if got := assistRefusals(false, plan); len(got) != 0 {
		t.Fatal(got)
	}
	for _, line := range want {
		if len(line) > 80 {
			t.Fatal(line)
		}
	}
	r := PushResult{AssistIgnored: true, assistNotUsed: assistRefusals(true, plan)}
	if lines := r.Lines(); len(lines) != 1 || strings.Contains(lines[0], "assist not used:") {
		t.Fatal(lines)
	}
}

func TestRefusalReasonsAreUniqueBoundedAcrossRuns(t *testing.T) {
	t.Parallel()
	plan := admissioncontract.Plan{}
	for i := 0; i < 500; i++ {
		plan.Entries = append(plan.Entries, admissioncontract.PlanEntry{Name: "publish", Template: "shared", Path: "release.publish(3:linux-amd64)", HoldRefusal: "has container restart policy"})
	}
	r := PushResult{assistNotUsed: append(assistRefusals(true, plan), assistRefusals(true, plan)...)}
	if got := r.Lines(); len(got) != 1 || got[0] != "assist not used: publish: has container restart policy" {
		t.Fatal(got)
	}
	plan.Entries[0].Name = strings.Repeat("s", 63)
	if got := assistRefusalLines(assistRefusals(true, plan)); len(got) != 1 || got[0] != "assist not used: has container restart policy" {
		t.Fatal(got)
	}
}
func TestRefusalSummaryCapsDistinctReasons(t *testing.T) {
	t.Parallel()
	reasons := []string{"reads a secret", "uses artifacts", "no explicit command", "is a daemon", "has authored probes", "has container restart policy", "has unsupported retries"}
	var facts []assistRefusal
	for _, reason := range reasons {
		facts = append(facts, assistRefusal{strings.Repeat("s", 63), reason})
	}
	lines := assistRefusalLines(facts)
	if len(lines) != 5 || lines[4] != "assist not used: and 3 more reasons" {
		t.Fatal(lines)
	}
	for _, line := range lines {
		if len(line) > 80 || strings.Contains(line, "...") {
			t.Fatal(line)
		}
	}
}

func TestInvocationRefusalsStayCompleteAndUnique(t *testing.T) {
	t.Parallel()
	reasons := []string{"inside a retried group", "its template is also used where assist cannot apply", "inside a loop"}
	var refusals []assistRefusal
	for _, reason := range reasons {
		for range 3 {
			refusals = append(refusals, assistRefusal{strings.Repeat("step", 40), reason})
		}
	}
	lines := assistRefusalLines(refusals)
	if len(lines) != 3 {
		t.Fatal("refusals were not grouped", lines)
	}
	for i, line := range lines {
		if len(line) > 80 || line != "assist not used: "+reasons[i] {
			t.Fatal("incomplete refusal", line)
		}
	}
}
