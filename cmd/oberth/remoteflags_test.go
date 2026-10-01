package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Go's flag package stops parsing at the first non-flag argument, so
// `oberth run <id> --json` read --json as a second positional and failed with
// a usage error that said nothing about flag order. The documentation promises
// --json on every command without saying where it goes, and the end is where
// it gets typed.
func TestFlagsAreAcceptedAfterPositionals(t *testing.T) {
	flags, asJSON, err := remoteFlags("run", []string{"abc123", "--json"}, io_Discard{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !*asJSON {
		t.Fatal("--json after the run ID was not seen")
	}
	if flags.NArg() != 1 || flags.Arg(0) != "abc123" {
		t.Fatalf("positional arguments = %v, want [abc123]", flags.Args())
	}
}

func TestFlagsBeforePositionalsStillWork(t *testing.T) {
	flags, asJSON, err := remoteFlags("run", []string{"--json", "abc123"}, io_Discard{})
	if err != nil || !*asJSON || flags.Arg(0) != "abc123" {
		t.Fatalf("asJSON=%v args=%v err=%v", asJSON, flags.Args(), err)
	}
}

// A value-taking flag must keep its value when it moves, and a boolean must
// not swallow the argument after it. This exercises the permutation directly:
// each command registers its own flags, so remoteFlags alone knows only --json.
func TestPermutationKeepsFlagValuesAndSparesBooleans(t *testing.T) {
	got := permuteFlagsFirst([]string{"abc123", "--burn", "ci", "--tail"})
	want := []string{"--burn", "ci", "--tail", "abc123"}
	if len(got) != len(want) {
		t.Fatalf("permuted to %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("permuted to %v, want %v", got, want)
		}
	}
}

// The boolean case, arranged so a wrong answer is visible.
//
// With the boolean first and one positional, consuming the next argument
// produces the same sequence either way, so the mistake hides. A trailing flag
// separates them: if --tail swallows the run ID, the ID never reaches the
// positional tail and --json ends up behind it.
func TestABooleanFlagDoesNotSwallowThePositional(t *testing.T) {
	got := permuteFlagsFirst([]string{"--tail", "abc123", "--json"})
	want := []string{"--tail", "--json", "abc123"}
	if len(got) != len(want) {
		t.Fatalf("permuted to %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("permuted to %v, want %v; --tail consumed the run ID", got, want)
		}
	}
}

// A positional that begins with a dash survives after "--".
func TestDoubleDashEndsFlagPermutation(t *testing.T) {
	got := permuteFlagsFirst([]string{"--json", "--", "-weird-id"})
	if len(got) != 2 || got[0] != "--json" || got[1] != "-weird-id" {
		t.Fatalf("permuted to %v", got)
	}
}

// TestAFailedRunReportsItsReason guards the field the client used to drop.
//
// remoteRun had no Error or Phase, so the API returned them and the decoder
// discarded them: a failed run printed only "failed", which is the one thing
// the reader already knew.
func TestAFailedRunReportsItsReason(t *testing.T) {
	source, err := os.ReadFile("remote.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`Error\s+string`).Match(source) {
		t.Fatal("remoteRun has no Error field, so the reason a run failed is decoded away")
	}
	if !strings.Contains(string(source), "failed in %s: %s") {
		t.Fatal("the run detail never prints the failure reason it decoded")
	}
}

type io_Discard struct{}

func (io_Discard) Write(p []byte) (int, error) { return len(p), nil }
