package main

import (
	"strings"
	"testing"
)

// The push banner prints twelve characters and the dashboard abbreviates too,
// so the identifier a reader has to hand is usually a prefix. Refusing it sends
// them to list runs and copy the long form, for no reason: the server can be
// asked which run it is.
func TestRunIDPrefixResolvesToTheOneRunItMatches(t *testing.T) {
	t.Parallel()
	known := []string{
		"ba7760409e48a045ea85e2bc3a01c610",
		"d34b5c086299373274a65f2e361c0be2",
	}

	resolved, err := resolveRunIDPrefix("ba7760409e48", known)
	if err != nil {
		t.Fatalf("a unique prefix was refused: %v", err)
	}
	if resolved != known[0] {
		t.Errorf("resolved to %q, want %q", resolved, known[0])
	}
}

// A whole identifier is passed through untouched, so nothing about this costs a
// second request on the common path.
func TestWholeRunIDIsUsedAsGiven(t *testing.T) {
	t.Parallel()
	full := "ba7760409e48a045ea85e2bc3a01c610"
	resolved, err := resolveRunIDPrefix(full, nil)
	if err != nil || resolved != full {
		t.Fatalf("resolved = %q, err = %v; want the identifier unchanged", resolved, err)
	}
}

// Guessing between two runs would be worse than refusing, so an ambiguous
// prefix names the candidates instead.
func TestAmbiguousPrefixNamesTheCandidates(t *testing.T) {
	t.Parallel()
	known := []string{
		"ba7760409e48a045ea85e2bc3a01c610",
		"ba7760409e48ffffffffffffffffffff",
	}

	_, err := resolveRunIDPrefix("ba7760409e48", known)
	if err == nil {
		t.Fatal("an ambiguous prefix resolved to one run")
	}
	for _, want := range known {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name candidate %s: %v", want, err)
		}
	}
}

// A prefix matching nothing says so, rather than reporting the run as missing
// and leaving the reader unsure which of the two it was.
func TestPrefixMatchingNothingSaysSo(t *testing.T) {
	t.Parallel()
	_, err := resolveRunIDPrefix("deadbeef", []string{"ba7760409e48a045ea85e2bc3a01c610"})
	if err == nil {
		t.Fatal("a prefix matching no run resolved successfully")
	}
	if !strings.Contains(err.Error(), "deadbeef") {
		t.Errorf("error does not quote what was asked for: %v", err)
	}
}
