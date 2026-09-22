package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintRunChecksListsEachCheckOnItsOwnLine(t *testing.T) {
	var out bytes.Buffer
	if err := printRunChecks(&out, []remoteCheck{
		{Name: "commit-judge", Verdict: "warn", Summary: "describes 0.38"},
		{Name: "affected-tests", Verdict: "pass", Summary: "vitest 2 of 11"},
	}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"check commit-judge", "warn", "describes 0.38", "check affected-tests", "vitest 2 of 11"} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q missing from:\n%s", want, text)
		}
	}
}

func TestPrintRunChecksWritesNothingWithoutChecks(t *testing.T) {
	var out bytes.Buffer
	if err := printRunChecks(&out, nil); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("expected no output, got %q", out.String())
	}
}
