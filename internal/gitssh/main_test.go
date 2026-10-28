package gitssh

import (
	"context"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("OBERTH_GITSSH_FIXTURE_PROCESS") == "1" {
		os.Exit(ExitCode(Run(context.Background(), os.Args[1:], os.Getenv(ConfigEnvironment), os.Stdin, os.Stdout, os.Stderr)))
	}
	os.Exit(m.Run())
}
