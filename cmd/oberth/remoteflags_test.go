package main

import (
	"flag"
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

func TestRegisteredFlagTypesPreserveInterspersedArguments(t *testing.T) {
	for _, args := range [][]string{{"abc123", "--burn", "ci", "--tail"}, {"--tail", "abc123", "--json"}, {"--json", "--", "-weird-id"}} {
		flags := flag.NewFlagSet("fixture", flag.ContinueOnError)
		group := flags.String("burn", "", "group")
		tail := flags.Bool("tail", false, "tail")
		asJSON := flags.Bool("json", false, "json")
		if err := parseCommandFlags(flags, args); err != nil {
			t.Fatal(err)
		}
		want := "abc123"
		if args[1] == "--" {
			want = "-weird-id"
		}
		if flags.NArg() != 1 || flags.Arg(0) != want {
			t.Fatalf("positionals %v for %v", flags.Args(), args)
		}
		if args[0] == "abc123" && (*group != "ci" || !*tail) {
			t.Fatalf("flag values lost: %s %t", *group, *tail)
		}
		if args[0] == "--tail" && (!*tail || !*asJSON) {
			t.Fatal("boolean consumed positional")
		}
	}
}
