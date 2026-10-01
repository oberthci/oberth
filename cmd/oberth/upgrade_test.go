package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRunUpgradeHelpReturnsNil(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runUpgrade(context.Background(), []string{"--help"}, &output)
	if err != nil {
		t.Fatalf("runUpgrade --help = %v", err)
	}
	text := output.String()
	for _, flag := range []string{"namespace", "context", "dry-run", "yes", "timeout", "chart", "values", "set"} {
		if !strings.Contains(text, flag) {
			t.Fatalf("help output missing flag %q: %q", flag, text)
		}
	}
}

func TestRunUpgradeRejectsPositionalArguments(t *testing.T) {
	t.Parallel()
	err := runUpgrade(context.Background(), []string{"extra"}, io.Discard)
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "no positional arguments") {
		t.Fatalf("positional argument error = %v", err)
	}
}

func TestRunUpgradeRejectsUnknownFlags(t *testing.T) {
	t.Parallel()
	err := runUpgrade(context.Background(), []string{"--nonexistent"}, io.Discard)
	if !errors.Is(err, errUsage) {
		t.Fatalf("unknown flag error = %v", err)
	}
}

func TestRunUpgradeRejectsDevBuild(t *testing.T) {
	t.Parallel()
	// The default version is "dev" which should be rejected.
	err := runUpgrade(context.Background(), nil, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "dev build") {
		t.Fatalf("dev build error = %v", err)
	}
}
