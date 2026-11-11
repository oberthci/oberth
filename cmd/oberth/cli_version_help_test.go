package main

import (
	"strings"
	"testing"
)

// oberth --version prints what oberth version prints; oberth help [command]
// prints what --help prints (#3 of the fresh-install rehearsal).
func TestVersionFlagAndHelpCommand(t *testing.T) {
	run := func(args ...string) string {
		t.Helper()
		var out strings.Builder
		if err := runCLI(t.Context(), args, strings.NewReader(""), &out); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		return out.String()
	}
	if got, want := run("--version"), run("version"); got != want || !strings.HasPrefix(got, "oberth ") {
		t.Fatalf("--version = %q, version = %q", got, want)
	}
	if got, want := run("help"), run("--help"); got != want || !strings.Contains(got, "Commands:") {
		t.Fatalf("help = %q", got)
	}
	if got, want := run("help", "install"), run("install", "--help"); got != want {
		t.Fatalf("help install = %q, want %q", got, want)
	}
	if got, want := run("help", "uplink", "add"), run("uplink", "add", "--help"); got != want {
		t.Fatalf("help uplink add = %q, want %q", got, want)
	}
}

// Every command's flag help starts with the same "Usage: oberth <command>"
// line, never Go's default "Usage of <name>:".
func TestCommandHelpUsesOneUsageFormat(t *testing.T) {
	for _, command := range documentedCommands {
		var out strings.Builder
		if err := runCLI(t.Context(), append(strings.Fields(command), "--help"), strings.NewReader(""), &out); err != nil {
			t.Fatalf("%s --help: %v", command, err)
		}
		if strings.Contains(out.String(), "Usage of ") || !strings.HasPrefix(out.String(), "Usage: oberth ") {
			t.Errorf("%s --help starts %q", command, strings.SplitN(out.String(), "\n", 2)[0])
		}
	}
}
