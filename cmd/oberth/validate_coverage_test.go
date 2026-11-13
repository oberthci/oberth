package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("write failed") }

func TestResolveValidateTargetRejectsFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "file.txt")
	if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := resolveValidateTarget(path)
	if err == nil || !strings.Contains(err.Error(), "must be a directory") {
		t.Fatalf("file target error = %v", err)
	}
}

func TestResolveValidateTargetEmptyArgUsesWorkingDir(t *testing.T) {
	t.Parallel()
	target, err := resolveValidateTarget("")
	if err != nil {
		t.Fatal(err)
	}
	if target.repoRoot == "" {
		t.Fatal("empty argument resolved to empty root")
	}
}

func TestValidateWithInvalidBuildYAMLReportsErrors(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oberthDir := filepath.Join(root, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oberthDir, "build.yaml"), []byte("not: valid: yaml: {[}"), 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runValidate(context.Background(), []string{root}, &output)
	if err == nil {
		t.Fatal("invalid YAML accepted")
	}
	if !strings.Contains(output.String(), "result: FAIL") {
		t.Fatalf("output missing FAIL result: %q", output.String())
	}
}

func TestValidateWithReleaseYAMLPresent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oberthDir := filepath.Join(root, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Build YAML from a valid generated template.
	if err := os.WriteFile(filepath.Join(oberthDir, "build.yaml"), []byte(buildYAMLDemo), 0o644); err != nil {
		t.Fatal(err)
	}
	// Release YAML with valid structure.
	if err := os.WriteFile(filepath.Join(oberthDir, "release.yaml"), []byte(buildYAMLDemo), 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runValidate(context.Background(), []string{root}, &output); err != nil {
		t.Fatalf("valid build+release rejected: %v\n%s", err, output.String())
	}
	text := output.String()
	if !strings.Contains(text, "build.yaml") || !strings.Contains(text, "release.yaml") {
		t.Fatalf("output missing file names: %q", text)
	}
}

func TestValidateHelpReturnsNil(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	err := runValidate(context.Background(), []string{"--help"}, &output)
	if err != nil {
		t.Fatalf("validate --help = %v", err)
	}
}

func TestValidateRejectsTooManyArguments(t *testing.T) {
	t.Parallel()
	err := runValidate(context.Background(), []string{"one", "two"}, nil)
	if err == nil || !strings.Contains(err.Error(), "at most one path") {
		t.Fatalf("too many args error = %v", err)
	}
}

func TestValidateReportLineSkipsAfterWriteError(t *testing.T) {
	t.Parallel()
	report := &validateReport{out: &failWriter{}}
	report.line("first line")
	if report.writeErr == nil {
		t.Fatal("expected write error")
	}
	// Subsequent lines should be no-ops due to the early return.
	report.line("second line")
	report.problem("a problem")
	// problem still counts even though writing failed.
	if report.errors != 1 {
		t.Fatalf("errors = %d, want 1", report.errors)
	}
}

func TestValidateAdmissionFailureReportsError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oberthDir := filepath.Join(root, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A valid Workflow YAML but with an image not in the allowlist.
	yaml := strings.ReplaceAll(buildYAMLDemo, "debian:trixie-slim", "disallowed:1.0")
	if err := os.WriteFile(filepath.Join(oberthDir, "build.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	// An explicit operator prefix does not match the authored image.
	err := runValidate(context.Background(), []string{"--runner-image-prefixes=debian:", root}, &output)
	if err == nil {
		t.Fatal("disallowed image accepted")
	}
	if !strings.Contains(output.String(), "result: FAIL") {
		t.Fatalf("output missing FAIL: %q", output.String())
	}
}

func TestValidateFromOberthSubdirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oberthDir := filepath.Join(root, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oberthDir, "build.yaml"), []byte(buildYAMLDemo), 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	// Point the validator at the .oberth subdirectory itself.
	if err := runValidate(context.Background(), []string{oberthDir}, &output); err != nil {
		t.Fatalf("validate from .oberth dir: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "result: PASS") {
		t.Fatalf("output missing PASS: %q", output.String())
	}
}

func TestValidateWriteErrorPropagates(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oberthDir := filepath.Join(root, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oberthDir, "build.yaml"), []byte(buildYAMLDemo), 0o644); err != nil {
		t.Fatal(err)
	}
	err := executeValidate(validateTarget{repoRoot: root, imagePrefixes: []string{"debian:"}}, &failWriter{})
	if err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Fatalf("write error = %v, want write failed", err)
	}
}

func TestValidateWithoutReleaseYAMLShowsOptionalMessage(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oberthDir := filepath.Join(root, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oberthDir, "build.yaml"), []byte(buildYAMLDemo), 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runValidate(context.Background(), []string{root}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "not present (optional)") {
		t.Fatalf("missing optional release.yaml message: %q", output.String())
	}
}

// A retired Workflow annotation is inert metadata and passes validation.
func TestValidateIgnoresTheRemovedConcurrencyGroupAnnotation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	oberthDir := filepath.Join(root, ".oberth")
	if err := os.MkdirAll(oberthDir, 0o755); err != nil {
		t.Fatal(err)
	}
	document := strings.Replace(buildYAMLDemo, "kind: Workflow\n", "kind: Workflow\nmetadata:\n  annotations:\n    oberth.ci/concurrency-group: terraform-state\n", 1)
	if err := os.WriteFile(filepath.Join(oberthDir, "build.yaml"), []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := runValidate(context.Background(), []string{root}, &output)
	if err != nil || !strings.Contains(output.String(), "result: PASS") {
		t.Fatalf("removed group annotation = %v\n%s", err, output.String())
	}
}
