package store

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	// CI supplies a bounded memory-backed volume charged to the test step.
	// Only the store test binary redirects its fixtures; Go builds and other
	// package tests retain their normal temporary directories.
	if dir := os.Getenv("OBERTH_TEST_STORE_TMPDIR"); dir != "" {
		// Go 1.26's testing.TempDir uses GOTMPDIR; os.MkdirTemp uses TMPDIR.
		for _, key := range []string{"TMPDIR", "GOTMPDIR"} {
			if err := os.Setenv(key, dir); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
	}
	os.Exit(m.Run())
}
