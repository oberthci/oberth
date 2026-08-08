package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestVersionReportsInjectedReleaseIdentity(t *testing.T) {
	originalVersion, originalCommit, originalDate := version, commit, date
	t.Cleanup(func() { version, commit, date = originalVersion, originalCommit, originalDate })
	version, commit, date = "v1.2.3", "0123456789ab", "2026-08-06T10:00:00Z"
	var output bytes.Buffer
	if err := runCLI(context.Background(), []string{"version"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "oberth v1.2.3 commit=0123456789ab date=2026-08-06T10:00:00Z\n"; got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
	if err := runCLI(context.Background(), []string{"version", "extra"}, strings.NewReader(""), &output); err == nil {
		t.Fatal("version accepted an argument")
	}
}
