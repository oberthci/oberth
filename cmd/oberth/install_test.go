package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRunInstallHelpReturnsNil(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runInstall(context.Background(), []string{"--help"}, strings.NewReader(""), &output)
	if err != nil {
		t.Fatalf("runInstall --help = %v", err)
	}
	text := output.String()
	if !strings.Contains(text, "dev") || !strings.Contains(text, "production") {
		t.Fatalf("help output missing expected flags: %q", text)
	}
}

func TestRunInstallRejectsPositionalArguments(t *testing.T) {
	t.Parallel()
	err := runInstall(context.Background(), []string{"extra"}, strings.NewReader(""), io.Discard)
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "no positional arguments") {
		t.Fatalf("positional argument error = %v", err)
	}
}

func TestRunInstallRejectsUnknownFlags(t *testing.T) {
	t.Parallel()
	err := runInstall(context.Background(), []string{"--nonexistent"}, strings.NewReader(""), io.Discard)
	if !errors.Is(err, errUsage) {
		t.Fatalf("unknown flag error = %v", err)
	}
}

func TestRunCLIRoutesAllCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantErr bool
		wantStr string
	}{
		{name: "no args", args: nil, wantErr: true, wantStr: "expected"},
		{name: "unknown", args: []string{"bogus"}, wantErr: true, wantStr: "unknown command"},
		{name: "version", args: []string{"version"}, wantStr: "oberth dev"},
		{name: "version extra args", args: []string{"version", "extra"}, wantErr: true, wantStr: "no arguments"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			err := runCLI(context.Background(), test.args, strings.NewReader(""), &output)
			if test.wantErr {
				if err == nil {
					t.Fatalf("runCLI(%q) succeeded unexpectedly", test.args)
				}
				if test.wantStr != "" && !strings.Contains(err.Error(), test.wantStr) {
					t.Fatalf("runCLI(%q) error = %v, want %q", test.args, err, test.wantStr)
				}
				return
			}
			if err != nil {
				t.Fatalf("runCLI(%q) = %v", test.args, err)
			}
			if test.wantStr != "" && !strings.Contains(output.String(), test.wantStr) {
				t.Fatalf("runCLI(%q) output = %q, want %q", test.args, output.String(), test.wantStr)
			}
		})
	}
}

func TestRunCLISecretstoreRouting(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "no subcommand", args: []string{"secretstore"}, wantErr: "secretstore setup|verify"},
		{name: "unknown", args: []string{"secretstore", "unknown"}, wantErr: "unknown secretstore command"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := runCLI(context.Background(), test.args, strings.NewReader(""), io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("runCLI(%q) = %v, want %q", test.args, err, test.wantErr)
			}
		})
	}
}

func TestRunCLISecretstoreSetupPrintsGuidance(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runCLI(context.Background(), []string{"secretstore", "setup"}, strings.NewReader(""), &output)
	if err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "Store-side setup") || !strings.Contains(text, "setup-secretstore.sh") {
		t.Fatalf("setup output = %q", text)
	}
}

func TestRunCLISecretstoreSetupPrintScript(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runCLI(context.Background(), []string{"secretstore", "setup", "--print-script"}, strings.NewReader(""), &output)
	if err != nil {
		t.Fatal(err)
	}
	if output.Len() == 0 {
		t.Fatal("--print-script produced empty output")
	}
}

func TestRunCLISecretstoreBootstrapTLSUsageErrors(t *testing.T) {
	t.Parallel()
	// Missing required flags.
	err := runCLI(context.Background(), []string{"secretstore", "bootstrap-tls"}, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("missing flags error = %v", err)
	}
}

func TestRunCLIHelpPrintsUsage(t *testing.T) {
	t.Parallel()
	for _, flag := range []string{"--help", "-h"} {
		var output bytes.Buffer
		if err := runCLI(context.Background(), []string{flag}, strings.NewReader(""), &output); err != nil {
			t.Fatalf("runCLI(%q) = %v", flag, err)
		}
		got := output.String()
		if !strings.Contains(got, "Usage:") || !strings.Contains(got, "init") || !strings.Contains(got, "version") {
			t.Fatalf("runCLI(%q) output = %q, want usage listing commands", flag, got)
		}
	}
}

// TestRunCLIRoutesEveryCommandViaHelp exercises every case statement in runCLI
// by passing --help (or causing a usage error) through the top-level router,
// so the coverage tool sees each switch arm as executed.
func TestRunCLIRoutesEveryCommandViaHelp(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		args    []string
		wantNil bool
		wantErr string
	}{
		{name: "global --help", args: []string{"--help"}, wantNil: true},
		{name: "global -h", args: []string{"-h"}, wantNil: true},
		{name: "init help", args: []string{"init", "--help"}, wantNil: true},
		{name: "validate usage", args: []string{"validate", "--unknown"}, wantErr: "usage error"},
		{name: "serve help", args: []string{"serve", "--help"}, wantNil: true},
		{name: "upstream usage", args: []string{"upstream"}, wantErr: "upstream add|list|remove"},
		{name: "repo usage", args: []string{"repo"}, wantErr: "repo add|remove"},
		{name: "uplink usage", args: []string{"uplink"}, wantErr: "uplink add|list|remove"},
		{name: "access usage", args: []string{"access"}, wantErr: "access list|allow|revoke"},
		{name: "install help", args: []string{"install", "--help"}, wantNil: true},
		{name: "upgrade help", args: []string{"upgrade", "--help"}, wantNil: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			err := runCLI(context.Background(), test.args, strings.NewReader(""), &output)
			if test.wantNil {
				if err != nil {
					t.Fatalf("runCLI(%q) = %v", test.args, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("runCLI(%q) = %v, want %q", test.args, err, test.wantErr)
			}
		})
	}
}

// TestRunInstallWiresSecretStoreFlag guards the flag→Config wiring. A dropped
// StringVar line leaves --secretstore parsing as an unknown flag rather than
// selecting anything, and a scripted install that believes it asked for a
// store would get whatever the non-interactive default happens to be.
func TestRunInstallWiresSecretStoreFlag(t *testing.T) {
	t.Parallel()
	err := runInstall(context.Background(), []string{"--secretstore", "bogus"}, strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "production, dev or none") {
		t.Fatalf("--secretstore should reach Config validation, got: %v", err)
	}
}
