package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestSharedFlagSyntax(t *testing.T) {
	t.Setenv("OBERTH_TOKEN", "fixture")
	config, err := parseMCPFlags("fixture", []string{"arg", "-url=http://fixture.invalid/mcp"}, 1)
	if err != nil || config.URL != "http://fixture.invalid/mcp" {
		t.Fatalf("single dash or trailing flag rejected: %+v %v", config, err)
	}
	options, err := parseCancelOptions([]string{"run", "--force=false"}, false)
	if err != nil || options.force {
		t.Fatalf("explicit boolean rejected: %+v %v", options, err)
	}
	flags, _, err := remoteFlags("fixture", []string{"--json", "--", "-literal"}, &bytes.Buffer{})
	if err != nil || flags == nil || flags.NArg() != 1 || flags.Arg(0) != "-literal" {
		t.Fatalf("double dash lost: %v %v", flags, err)
	}
}
func TestMCPSubcommandHelpSucceeds(t *testing.T) {
	var out strings.Builder
	if err := runCLI(t.Context(), []string{"mcp", "tools", "--help"}, nil, &out); err != nil || !strings.Contains(out.String(), "Usage:") {
		t.Fatalf("help = %v %s", err, &out)
	}
}
func TestFlagErrorsStayOutOfCommandOutput(t *testing.T) {
	for _, args := range [][]string{{"wait-idle", "--unknown"}, {"run", "resources", "--unknown"}} {
		var out strings.Builder
		if err := runCLI(t.Context(), args, nil, &out); err == nil || out.Len() != 0 {
			t.Fatalf("%v: %v output %q", args, err, out.String())
		}
	}
}

func TestSharedFlagsPreserveStdinSentinel(t *testing.T) {
	t.Setenv("OBERTH_TOKEN", "fixture")
	t.Setenv("OBERTH_MCP_URL", "https://example.invalid/mcp")
	config, err := parseMCPFlags("mcp call", []string{"status", "--args-file", "-"}, 1)
	if err != nil || config.argsFile != "-" {
		t.Fatalf("stdin sentinel rejected: %+v %v", config, err)
	}
}
func TestAdminHelpNamesOnlySupportedMaintenance(t *testing.T) {
	var out strings.Builder
	if err := runCLI(t.Context(), []string{"admin", "--help"}, nil, &out); err != nil || !strings.Contains(out.String(), "read-file") || strings.Contains(out.String(), "database") {
		t.Fatalf("admin help: %v %s", err, &out)
	}
}

func TestHelpOmitsRemovedAttentionCommand(t *testing.T) {
	var out strings.Builder
	if err := runCLI(t.Context(), []string{"--help"}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "attention") || strings.Contains(usageCommands, "attention") {
		t.Fatalf("help advertises removed attention command: %s; usage: %s", &out, usageCommands)
	}
	if !strings.Contains(out.String(), "issues") {
		t.Fatalf("help omits supported issues command: %s", &out)
	}
}
