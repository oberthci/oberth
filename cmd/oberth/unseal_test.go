package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// The command exists and describes itself. A sealed store costs one short
// command only if that command is discoverable from --help.
func TestUnsealIsAListedCommandWithItsOwnHelp(t *testing.T) {
	var top bytes.Buffer
	if err := runCLI(context.Background(), []string{"--help"}, strings.NewReader(""), &top); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(top.String(), "unseal") {
		t.Fatalf("unseal is not listed:\n%s", top.String())
	}

	var help bytes.Buffer
	if err := runCLI(context.Background(), []string{"unseal", "--help"}, strings.NewReader(""), &help); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(help.String(), "openbao-namespace") {
		t.Fatalf("unseal help does not describe its flags:\n%s", help.String())
	}
}

func TestUnsealRefusesPositionalArguments(t *testing.T) {
	var output bytes.Buffer
	err := runCLI(context.Background(), []string{"unseal", "openbao-0"}, strings.NewReader(""), &output)
	if err == nil || !strings.Contains(err.Error(), "no positional arguments") {
		t.Fatalf("err = %v, want a usage error", err)
	}
}
