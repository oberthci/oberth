package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/releaseverify/verifytest"
)

//nolint:gochecknoglobals // Tests share one immutable executable for the package run.
var cliBinary struct {
	once            sync.Once
	directory, path string
	err             error
}

func TestMain(m *testing.M) {
	code := verifytest.WithKey(m)
	if cliBinary.directory != "" {
		if err := os.RemoveAll(cliBinary.directory); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "remove command fixture:", err)
			code = 1
		}
	}
	os.Exit(code)
}

func composeBinary(t *testing.T) string {
	t.Helper()
	cliBinary.once.Do(buildCommandFixture)
	if cliBinary.err != nil {
		t.Fatal(cliBinary.err)
	}
	return cliBinary.path
}

func buildCommandFixture() {
	cliBinary.directory, cliBinary.err = os.MkdirTemp(os.Getenv("GOTMPDIR"), "oberth-cli-test-")
	if cliBinary.err != nil {
		return
	}
	cliBinary.path = filepath.Join(cliBinary.directory, "oberth")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "build", "-p=1", "-o", cliBinary.path, ".")
	command.WaitDelay = 5 * time.Second
	if output, err := command.CombinedOutput(); err != nil {
		cliBinary.err = fmt.Errorf("compile product: %w: %s", err, output)
		return
	}
	cliBinary.err = os.Chmod(cliBinary.path, 0555)
}
