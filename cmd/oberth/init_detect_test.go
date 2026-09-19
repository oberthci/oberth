package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/pipelinegen"
)

// readGenerated returns what `oberth init` wrote for a directory.
func readGenerated(t *testing.T, dir string, typeOverride string) (string, string) {
	t.Helper()
	var output bytes.Buffer
	if err := executeInit(context.Background(), dir, typeOverride, "", false, &output); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, ".oberth", "build.yaml")) // #nosec G304 -- test temp dir
	if err != nil {
		t.Fatal(err)
	}
	return string(content), output.String()
}

func TestExecuteInitNodeRunsTheRepositoryScripts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "package.json"),
		`{"scripts":{"lint":"eslint .","test":"vitest","build":"vite build"}}`+"\n")

	content, output := readGenerated(t, dir, "")

	// The steps must be this repository's, not a fixed demo.
	for _, want := range []string{"npm ci", "npm run lint", "npm run test", "npm run build"} {
		if !strings.Contains(content, want) {
			t.Fatalf("generated pipeline missing %q:\n%s", want, content)
		}
	}
	// A script the repository does not have must not appear: a step that runs
	// `npm run typecheck` in a repository with no typecheck script fails for a
	// reason that has nothing to do with the code.
	if strings.Contains(content, "npm run typecheck") {
		t.Fatalf("generated a step for a script this repository does not have:\n%s", content)
	}
	if !strings.Contains(output, "package.json") {
		t.Fatalf("init must say what it read, got:\n%s", output)
	}
}

func TestExecuteInitMavenRunsMaven(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "pom.xml"),
		"<project><modelVersion>4.0.0</modelVersion><artifactId>svc</artifactId></project>\n")

	content, _ := readGenerated(t, dir, "")
	if !strings.Contains(content, "mvn -B -ntp") {
		t.Fatalf("a pom.xml must produce Maven steps:\n%s", content)
	}
	if !strings.Contains(content, "maven:3.9-eclipse-temurin-") {
		t.Fatalf("a pom.xml must run in a Maven image:\n%s", content)
	}
}

// TestExecuteInitUnrecognizedRepositoryFailsLoudly is the behaviour change
// that matters. The previous generator wrote a demo that copied a file and
// checksummed it, and that demo went green for a Python project, a Rust
// project, and an empty directory alike.
func TestExecuteInitUnrecognizedRepositoryFailsLoudly(t *testing.T) {
	t.Parallel()
	for _, marker := range []string{"pyproject.toml", "Cargo.toml", ""} {
		t.Run(marker, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if marker != "" {
				writeTestFile(t, filepath.Join(dir, marker), "[project]\n")
			}

			content, output := readGenerated(t, dir, "")
			if !strings.Contains(content, "THIS PIPELINE IS NOT FINISHED") {
				t.Fatalf("the file must say it is unfinished:\n%s", content)
			}
			if !strings.Contains(content, "exit 1") {
				t.Fatalf("the scaffold must fail rather than pass by doing nothing:\n%s", content)
			}
			if !strings.Contains(output, "THIS PIPELINE IS NOT FINISHED") {
				t.Fatalf("the terminal must say it too, got:\n%s", output)
			}
			if strings.Contains(output, "next: commit and push") {
				t.Fatalf("an unfinished pipeline must not be presented as ready to push:\n%s", output)
			}
		})
	}
}

func TestExecuteInitTypeOverrideWins(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "package.json"), `{"scripts":{"test":"vitest"}}`+"\n")

	content, _ := readGenerated(t, dir, "generic")
	if !strings.Contains(content, "THIS PIPELINE IS NOT FINISHED") {
		t.Fatalf("--type generic must override detection:\n%s", content)
	}
}

func TestParseProjectTypeRejectsUnknown(t *testing.T) {
	t.Parallel()
	if _, err := parseProjectType("erlang"); err == nil {
		t.Fatal("an unknown type must be a usage error")
	}
	for value, want := range map[string]pipelinegen.Kind{
		"go":      pipelinegen.KindGo,
		"NODE":    pipelinegen.KindNode,
		"maven":   pipelinegen.KindMaven,
		"generic": pipelinegen.KindUnknown,
	} {
		kind, err := parseProjectType(value)
		if err != nil {
			t.Fatalf("%s: %v", value, err)
		}
		if kind != want {
			t.Fatalf("%s -> %s, want %s", value, kind, want)
		}
	}
}
