package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/runner"
)

const executablePipeline = `//go:build ignore

package main

import "oberth"

func Pipeline(ctx *oberth.Context) oberth.Pipeline {
	println("secret-value")
	return oberth.New().
		Retrograde("verify", oberth.Step{
			Name: "check", Command: "sh",
			Args: []string{"-c", "printf 'secret-value\\n'"},
		}).
		Build()
}
`

func TestRunExecutesPipelineMasksSecretsAndWritesSummary(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "src")
	writeTestFile(t, filepath.Join(sourceRoot, ".oberth", "periapsis.go"), executablePipeline)
	secretRoot := filepath.Join(root, "secrets")
	writeTestFile(t, filepath.Join(secretRoot, "token"), "secret-value")
	terminationPath := filepath.Join(root, "termination.json")

	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"--src", sourceRoot,
		"--secret-dir", secretRoot,
		"--termination-log", terminationPath,
		"--step-timeout", "1s",
	}, strings.NewReader(""), &stdout, &stderr, testEnvironment(map[string]string{
		"OBERTH_REPO": "demo", "OBERTH_REF": "main", "OBERTH_SHA": strings.Repeat("a", 40), "OBERTH_TRIGGER": "ci",
	}))
	if code != 0 {
		t.Fatalf("exit code = %d; stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, "secret-value") {
		t.Fatalf("secret leaked: %s", combined)
	}
	if !strings.Contains(stdout.String(), "[verify/check] ") || !strings.Contains(stdout.String(), runner.MaskReplacement) {
		t.Fatalf("output lacks prefix or mask: %s", stdout.String())
	}
	results := readTestResults(t, terminationPath)
	if len(results) != 1 || results[0].Status != runner.StepPassed {
		t.Fatalf("results = %#v", results)
	}
}

func TestRunWritesFailureSummaryForInterpretError(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "src")
	writeTestFile(t, filepath.Join(sourceRoot, ".oberth", "periapsis.go"), `//go:build ignore

package main
import (
	"oberth"
	"os"
)
func Pipeline(ctx *oberth.Context) oberth.Pipeline { return oberth.New().Build() }
`)
	terminationPath := filepath.Join(root, "termination.json")
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"--src", sourceRoot, "--termination-log", terminationPath}, strings.NewReader(""), &stdout, &stderr, testEnvironment(nil))
	if code == 0 {
		t.Fatal("invalid source succeeded")
	}
	if !strings.Contains(stderr.String(), "[runner/runner] ") || strings.Contains(stderr.String(), "\nimport ") {
		t.Fatalf("runner error is not safely prefixed: %q", stderr.String())
	}
	result := requireRunnerFailureResult(t, terminationPath)
	if !strings.Contains(stderr.String(), "not allowed") {
		t.Fatalf("result = %#v, stderr = %q", result, stderr.String())
	}
}

func TestRunWritesFailureSummaryForOptionError(t *testing.T) {
	terminationPath := filepath.Join(t.TempDir(), "termination.json")
	var stderr bytes.Buffer
	code := run(context.Background(), []string{
		"--termination-log", terminationPath,
		"--step-timeout", "0s",
	}, strings.NewReader(""), &bytes.Buffer{}, &stderr, testEnvironment(nil))
	if code == 0 {
		t.Fatal("invalid options succeeded")
	}
	result := requireRunnerFailureResult(t, terminationPath)
	if !strings.Contains(stderr.String(), "step timeout must be greater than zero") {
		t.Fatalf("result = %#v, stderr = %q", result, stderr.String())
	}
}

func TestRunRejectsPeriapsisSymlinkOutsideSourceRoot(t *testing.T) {
	root := t.TempDir()
	sourceRoot := filepath.Join(root, "src")
	outside := filepath.Join(root, "outside.go")
	writeTestFile(t, outside, executablePipeline)
	if err := os.MkdirAll(filepath.Join(sourceRoot, ".oberth"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(sourceRoot, ".oberth", "periapsis.go")); err != nil {
		t.Fatal(err)
	}
	terminationPath := filepath.Join(root, "termination.json")
	var stderr bytes.Buffer
	code := run(context.Background(), []string{"--src", sourceRoot, "--termination-log", terminationPath}, strings.NewReader(""), &bytes.Buffer{}, &stderr, testEnvironment(nil))
	if code == 0 {
		t.Fatal("outside symlink succeeded")
	}
	result := requireRunnerFailureResult(t, terminationPath)
	if !strings.Contains(stderr.String(), "outside source root") {
		t.Fatalf("result = %#v, stderr = %q", result, stderr.String())
	}
}

func writeTestFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readTestResults(t *testing.T, path string) []runner.StepResult {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 || raw[0] != '[' {
		t.Fatalf("termination payload is not an array: %s", raw)
	}
	var results []runner.StepResult
	if err := json.Unmarshal(raw, &results); err != nil {
		t.Fatalf("decode results: %v: %s", err, raw)
	}
	return results
}

func requireRunnerFailureResult(t *testing.T, path string) runner.StepResult {
	t.Helper()
	results := readTestResults(t, path)
	if len(results) != 1 {
		t.Fatalf("results = %#v, want one runner failure", results)
	}
	result := results[0]
	if result.Burn != "runner" || result.Step != "runner" || result.Status != runner.StepFailed || result.ExitCode != 1 {
		t.Fatalf("runner failure = %#v", result)
	}
	started, err := time.Parse(time.RFC3339Nano, result.StartedAt)
	if err != nil {
		t.Fatalf("parse started_at: %v", err)
	}
	finished, err := time.Parse(time.RFC3339Nano, result.FinishedAt)
	if err != nil {
		t.Fatalf("parse finished_at: %v", err)
	}
	if finished.Before(started) {
		t.Fatalf("runner failure finished before it started: %#v", result)
	}
	return result
}

func testEnvironment(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}
