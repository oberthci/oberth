package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/store"
)

func TestVersionReportsInjectedReleaseIdentity(t *testing.T) {
	originalVersion, originalCommit, originalDate := version, commit, date
	t.Cleanup(func() { version, commit, date = originalVersion, originalCommit, originalDate })
	version, commit, date = "v1.2.3", "0123456789ab", "2026-08-06T10:00:00Z"
	var output bytes.Buffer
	if err := runCLI(context.Background(), []string{"version"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	want := "oberth v1.2.3 commit=0123456789ab date=2026-08-06T10:00:00Z\n"
	if got := output.String(); got != want {
		t.Fatalf("version output = %q, want %q", got, want)
	}
	if err := runCLI(context.Background(), []string{"version", "extra"}, strings.NewReader(""), &output); err == nil {
		t.Fatal("version accepted an unknown argument")
	}
}

func TestVersionSchemaFlag(t *testing.T) {
	var output bytes.Buffer
	if err := runCLI(context.Background(), []string{"version", "--schema"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%d\n", store.LatestMigrationVersion())
	if got := output.String(); got != want {
		t.Fatalf("version --schema = %q, want %q", got, want)
	}
}

// TestVersionFormatContractWithRelease asserts that the default `oberth version`
// output matches the format expected by .oberth/release.sh verify_binary_version().
// The release script's awk check requires exactly 4 space-separated fields:
//
//	NF == 4 && $1 == "oberth" && $2 == tag
//	commit = $3; sub(/^commit=/, "", commit); len(commit)==12, hex
//	date = $4; sub(/^date=/, "", date); date ~ /^[0-9]{4}-[0-9]{2}-[0-9]{2}T/
//
// A field count change (e.g. appending schema=N) or a reordering breaks the
// release build — this test catches that class of regression.
func TestVersionFormatContractWithRelease(t *testing.T) {
	originalVersion, originalCommit, originalDate := version, commit, date
	t.Cleanup(func() { version, commit, date = originalVersion, originalCommit, originalDate })
	version, commit, date = "v99.0.0", "abcdef012345", "2026-01-01T00:00:00Z"
	var output bytes.Buffer
	if err := runCLI(context.Background(), []string{"version"}, strings.NewReader(""), &output); err != nil {
		t.Fatal(err)
	}
	line := strings.TrimRight(output.String(), "\n")
	fields := strings.Fields(line)
	if len(fields) != 4 {
		t.Fatalf("version output has %d fields, want exactly 4 (release.sh NF==4 contract): %q", len(fields), line)
	}
	if fields[0] != "oberth" {
		t.Fatalf("field 1 = %q, want %q", fields[0], "oberth")
	}
	// Field 2 is the tag (caller-supplied, not validated here).
	// Field 3: commit=<12 hex chars>
	commitPattern := regexp.MustCompile(`^commit=[0-9a-f]{12}$`)
	if !commitPattern.MatchString(fields[2]) {
		t.Fatalf("field 3 = %q, want commit=<12 hex chars>", fields[2])
	}
	// Field 4: date=<ISO timestamp prefix>
	datePattern := regexp.MustCompile(`^date=\d{4}-\d{2}-\d{2}T`)
	if !datePattern.MatchString(fields[3]) {
		t.Fatalf("field 4 = %q, want date=<YYYY-MM-DDT...>", fields[3])
	}
}

// TestVersionLdflagsInjection proves that go build -ldflags -X flags override
// the default version/commit/date values — the same mechanism the Makefile and
// Dockerfile use. This is NOT a Makefile invocation test; it validates the
// linker contract that the Makefile relies on.
func TestVersionLdflagsInjection(t *testing.T) {
	t.Parallel()
	binary := filepath.Join(t.TempDir(), "oberth-ldflags-test")
	wantVersion := "ldflags-test-v42.0.0"
	wantCommit := "deadbeef1234"
	wantDate := "2026-08-17T00:00:00Z"
	ldflags := "-s -w" +
		" -X main.version=" + wantVersion +
		" -X main.commit=" + wantCommit +
		" -X main.date=" + wantDate
	cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", binary, "./")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	out, err := exec.Command(binary, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("oberth version: %v\n%s", err, out)
	}
	got := strings.TrimRight(string(out), "\n")
	// Default output must have exactly 4 fields (release.sh contract).
	if fields := strings.Fields(got); len(fields) != 4 {
		t.Fatalf("version output has %d fields, want 4: %q", len(fields), got)
	}
	for _, want := range []string{wantVersion, wantCommit, wantDate} {
		if !strings.Contains(got, want) {
			t.Errorf("version output %q missing %q", got, want)
		}
	}

	// --schema must still work on the ldflags-injected binary.
	schemaOut, err := exec.Command(binary, "version", "--schema").CombinedOutput()
	if err != nil {
		t.Fatalf("oberth version --schema: %v\n%s", err, schemaOut)
	}
	schemaStr := strings.TrimSpace(string(schemaOut))
	if schemaStr == "" || schemaStr == "0" {
		t.Fatalf("version --schema on ldflags binary = %q, want non-zero integer", schemaStr)
	}
}
