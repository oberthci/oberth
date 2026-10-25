package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestRunUpgradeHelpReturnsNil(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runUpgrade(context.Background(), []string{"--help"}, &output)
	if err != nil {
		t.Fatalf("runUpgrade --help = %v", err)
	}
	text := output.String()
	for _, flag := range []string{"namespace", "context", "dry-run", "yes", "timeout", "wait-timeout", "wait-for", "chart", "values", "set"} {
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

func TestUpgradeCompletionExitStatus(t *testing.T) {
	for _, test := range []struct{ dry, upgraded, already, wantError bool }{
		{false, true, false, false}, {false, false, false, true}, {false, false, true, false}, {true, false, false, false},
	} {
		err := upgradeCompletionError(test.dry, installer.UpgradeResult{Upgraded: test.upgraded, AlreadyUpToDate: test.already})
		if (err != nil) != test.wantError {
			t.Fatalf("%+v: %v", test, err)
		}
	}
}

func TestUpgradeRejectsInvalidWaitOptionsBeforeClusterAccess(t *testing.T) {
	for _, args := range [][]string{{"--wait-idle", "--wait-timeout=0"}, {"--wait-idle", "--wait-timeout=-1s"}, {"--wait-for=unknown"}, {"--wait-for="}, {"--wait-timeout=1h"}} {
		if err := runUpgrade(t.Context(), args, io.Discard); !errors.Is(err, errUsage) {
			t.Fatalf("%v: %v", args, err)
		}
	}
}
